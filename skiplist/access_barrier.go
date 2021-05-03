// Copyright (c) 2016 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package skiplist

import (
	"math"
	"sync"
	"sync/atomic"
	"unsafe"
)

/*
* Algorithm:
* Access barrier is used to facilitize safe remory reclaimation in the lockfree
* skiplist. Every skiplist access needs to be passed through a gate which tracks
* the safety premitives to figure out when is the right time to deallocate a
* skiplist node.
*
* Even though lockfree skiplist deletion algorithm takes care of completely unlinking
* a skiplist node from the skiplist, still there could be a small perioid during which
* deleted node is accessible to already live skiplist accessors. We need to wait until
* a safe period before the memory for the node can be deallocated.
*
* In this algorithm, the unit of safety period is called barrier session. All the
* live accessor of the skiplist are tracked in a barrier session. Whenever a
* skiplist delete or group of deletes is performed, current barrier session is
* closed and new barrier session is started. The previous barrier
* session tracks all the live accessors until the session of closed. The right
* time to safely reclaim the node is when all the accessor becomes dead. This makes
* sure that unlinked node will be invisible to everyone. The accessors in the
* barrier session can cooperatively detect and mark when each of them terminates.
* When the last accessor leaves, it can take the action to call the destructor
* for the node and barrier session terminates.
*
* Closing and installing a new barrier session:
* A session liveCount is incremented every time an accessor is entering the skiplist
* and decremented when the leave the skiplist. When a session is closed and new
* one needs to be installed, we just swap the global barrier session reference.
* There could be race conditions while a session is being marked as closed. Still
* an ongoing skiplist accessor can increment the counter of a session which was marked
* as closed. To detect those accessors and make them retry, we add a large number
* to the liveCount as part of the session close phase. When the accessor finds
* that the incremented result is greater than that large offset, it needs to backoff
* from the current session and acquire new session to increment the count. In this
* scheme, whoever decrements the count and gets the count equal to the large offset
* is responsible for deallocation of the object.
*
* The algorithm has to consider one more condition before it can call destructor for
* the session. Multiple closed sessions can be active at a time. We cannot call the
* destructor for a closed session while a previous closed session is still not terminated.
* Because, even through accessors from a closed session has become zero, accessors from previous
* closed session would be able to access items in the later closed session. Hence, a closed session
* can be terminated only after termination of all previous closed sessions.
* */

// BarrierSessionDestructor is a callback for SMR based reclaim of objects
type BarrierSessionDestructor func(objectRef unsafe.Pointer)

const barrierFlushOffset = math.MaxInt32 / 2

// BarrierSession handle tracks the live accessors of a barrier session
type BarrierSession struct {
	liveCount *int32
	objectRef unsafe.Pointer
	seqno     uint64
	closed    int32
}

// CompareBS is a barrier session comparator based on seqno
func CompareBS(this, that unsafe.Pointer) int {
	thisItm := (*BarrierSession)(this)
	thatItm := (*BarrierSession)(that)

	return int(thisItm.seqno) - int(thatItm.seqno)
}

func newBarrierSession() *BarrierSession {
	bs := &BarrierSession{
		liveCount: new(int32),
	}

	return bs
}

// AccessBarrier is the SMR core data structure for the skiplist
type AccessBarrier struct {
	activeSeqno uint64
	session     unsafe.Pointer
	callb       BarrierSessionDestructor

	freeq               *Skiplist
	freeSeqno           uint64
	isDestructorRunning int32

	numAllocated int64
	numFreed     int64

	active bool
	sync.Mutex
}

func newAccessBarrier(active bool, callb BarrierSessionDestructor) *AccessBarrier {
	ab := &AccessBarrier{
		active:       active,
		session:      unsafe.Pointer(newBarrierSession()),
		callb:        callb,
		numAllocated: 1,
	}
	if active {
		ab.freeq = New()
	}
	return ab
}

func (ab *AccessBarrier) GetStats() (int64, int64, int64, uint64) {
	if ab.freeq != nil {
		return ab.numAllocated, ab.numFreed, int64(ab.freeq.GetStats().NodeCount), ab.freeSeqno
	}
	return ab.numAllocated, ab.numFreed, 0, ab.freeSeqno
}

func (ab *AccessBarrier) doCleanup() {
	buf1 := ab.freeq.MakeBuf()
	buf2 := ab.freeq.MakeBuf()
	defer ab.freeq.FreeBuf(buf1)
	defer ab.freeq.FreeBuf(buf2)

	iter := ab.freeq.NewIterator(CompareBS, buf1)
	defer iter.Close()

	for iter.SeekFirst(); iter.Valid(); iter.Next() {
		node := iter.GetNode()
		bs := (*BarrierSession)(node.Item())
		if bs.seqno != ab.freeSeqno+1 {
			return
		}

		ab.freeSeqno++
		ab.callb(bs.objectRef)
		ab.freeq.DeleteNode(node, CompareBS, buf2, &ab.freeq.Stats)
		ab.numFreed++
	}
}

// Acquire marks enter of an accessor in the skiplist
func (ab *AccessBarrier) Acquire() *BarrierSession {
	if ab.active {
	retry:
		bs := (*BarrierSession)(atomic.LoadPointer(&ab.session))
		liveCount := atomic.AddInt32(bs.liveCount, 1)
		if liveCount > barrierFlushOffset {
			ab.Release(bs)
			goto retry
		}

		return bs
	}

	return nil
}

// Release marks leaving of an accessor in the skiplist
func (ab *AccessBarrier) Release(bs *BarrierSession) {
	if ab.active {
		liveCount := atomic.AddInt32(bs.liveCount, -1)
		if liveCount == barrierFlushOffset {
			buf := ab.freeq.MakeBuf()
			defer ab.freeq.FreeBuf(buf)

			// Accessors which entered a closed barrier session steps down automatically
			// But, they may try to close an already closed session.
			if atomic.AddInt32(&bs.closed, 1) == 1 {
				ab.freeq.Insert(unsafe.Pointer(bs), CompareBS, buf, &ab.freeq.Stats)
				if atomic.CompareAndSwapInt32(&ab.isDestructorRunning, 0, 1) {
					ab.doCleanup()
					atomic.CompareAndSwapInt32(&ab.isDestructorRunning, 1, 0)
				}
			}
		} else if liveCount < 0 || liveCount == barrierFlushOffset-1 {
			panic("Unsafe memory reclamation detected")
		}
	}
}

// FlushSession closes the current barrier session and starts the new session.
// The caller should provide the destructor pointer for the new session.
func (ab *AccessBarrier) FlushSession(ref unsafe.Pointer) {
	if ab.active {
		ab.Lock()
		defer ab.Unlock()

		bsPtr := atomic.LoadPointer(&ab.session)
		newBsPtr := unsafe.Pointer(newBarrierSession())
		atomic.CompareAndSwapPointer(&ab.session, bsPtr, newBsPtr)
		bs := (*BarrierSession)(bsPtr)
		bs.objectRef = ref
		ab.activeSeqno++
		bs.seqno = ab.activeSeqno
		ab.numAllocated++

		atomic.AddInt32(bs.liveCount, barrierFlushOffset+1)
		ab.Release(bs)
	}
}
