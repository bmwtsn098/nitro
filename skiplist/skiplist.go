package skiplist

import (
	"math/rand"
	"sync/atomic"
	"unsafe"
)

const MaxLevel = 32
const p = 0.25

type Item interface {
	Compare(Item) int
}

type Skiplist struct {
	head  *Node
	tail  *Node
	level int32
}

func New() *Skiplist {
	minItem := &nilItem{
		cmp: -1,
	}

	maxItem := &nilItem{
		cmp: 1,
	}

	head := newNode(minItem, MaxLevel)
	tail := newNode(maxItem, MaxLevel)

	for i := 0; i <= MaxLevel; i++ {
		head.setNext(i, tail, false)
	}

	s := &Skiplist{
		head: head,
		tail: tail,
	}

	return s
}

type Node struct {
	next  []unsafe.Pointer
	itm   Item
	level uint16
}
type NodeRef struct {
	deleted bool
	ptr     *Node
}

func newNode(itm Item, level int) *Node {
	return &Node{
		next:  make([]unsafe.Pointer, level+1),
		itm:   itm,
		level: uint16(level),
	}
}

func (n *Node) setNext(level int, ptr *Node, deleted bool) {
	n.next[level] = unsafe.Pointer(&NodeRef{ptr: ptr, deleted: deleted})
}

func (n *Node) getNext(level int) (*Node, bool) {
	ref := (*NodeRef)(atomic.LoadPointer(&n.next[level]))
	if ref != nil {
		return ref.ptr, ref.deleted
	}

	return nil, false
}

func (n *Node) dcasNext(level int, prevPtr, newPtr *Node, prevIsdeleted, newIsdeleted bool) bool {
	var swapped bool
	addr := &n.next[level]
	ref := (*NodeRef)(atomic.LoadPointer(addr))
	if ref != nil {
		if ref.ptr == prevPtr && ref.deleted == prevIsdeleted {
			swapped = atomic.CompareAndSwapPointer(addr, unsafe.Pointer(ref),
				unsafe.Pointer(&NodeRef{ptr: newPtr, deleted: newIsdeleted}))
		}
	}

	return swapped
}

func (s *Skiplist) randomLevel(randFn func() float32) int {
	var nextLevel int

	for ; randFn() < p; nextLevel++ {
	}

	if nextLevel > MaxLevel {
		nextLevel = MaxLevel
	}

	level := int(atomic.LoadInt32(&s.level))
	if nextLevel > level {
		atomic.CompareAndSwapInt32(&s.level, int32(level), int32(level+1))
		nextLevel = level + 1
	}

	return nextLevel
}

func (s *Skiplist) helpDelete(level int, prev, curr, next *Node) bool {
	return prev.dcasNext(level, curr, next, false, false)
}

func (s *Skiplist) findPath(itm Item) (preds, succs []*Node, found bool) {
	var cmpVal int = 1

	preds = make([]*Node, MaxLevel+1)
	succs = make([]*Node, MaxLevel+1)

retry:
	prev := s.head
	level := int(atomic.LoadInt32(&s.level))
	for i := level; i >= 0; i-- {
		curr, _ := prev.getNext(i)
	levelSearch:
		for {
			next, deleted := curr.getNext(i)
			for deleted {
				if !s.helpDelete(i, prev, curr, next) {
					goto retry
				}

				curr, _ = prev.getNext(i)
				next, deleted = curr.getNext(i)
			}

			cmpVal = curr.itm.Compare(itm)
			if cmpVal < 0 {
				prev = curr
				curr, _ = prev.getNext(i)
			} else {
				break levelSearch
			}
		}

		preds[i] = prev
		succs[i] = curr
	}

	if cmpVal == 0 {
		found = true
	}
	return
}

func (s *Skiplist) Insert(itm Item) {
	s.Insert2(itm, rand.Float32)
}

func (s *Skiplist) Insert2(itm Item, randFn func() float32) {
	itemLevel := s.randomLevel(randFn)
	x := newNode(itm, itemLevel)
retry:
	preds, succs, _ := s.findPath(itm)

	x.setNext(0, succs[0], false)
	if !preds[0].dcasNext(0, succs[0], x, false, false) {
		goto retry
	}

	for i := 1; i <= int(itemLevel); i++ {
	fixThisLevel:
		for {
			x.setNext(i, succs[i], false)
			if preds[i].dcasNext(i, succs[i], x, false, false) {
				break fixThisLevel
			}
			preds, succs, _ = s.findPath(itm)
		}
	}
}

func (s *Skiplist) Delete(itm Item) {
	var deleteMarked bool
	_, succs, found := s.findPath(itm)
	if !found {
		return
	}

	delNode := succs[0]
	targetLevel := int(delNode.level)
	for i := targetLevel; i >= 0; i-- {
		next, deleted := delNode.getNext(i)
		for !deleted {
			deleteMarked = delNode.dcasNext(i, next, next, false, true)
			next, deleted = delNode.getNext(i)
		}
	}

	if deleteMarked {
		s.findPath(itm)
	}

}