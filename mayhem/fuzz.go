package fuzz

import "strconv"
import "io"
import "github.com/couchbase/nitro"

func mayhemit(bytes []byte) int {

    var num int
    if len(bytes) > 1 {
        num, _ = strconv.Atoi(string(bytes[0]))

        switch num {
    
        case 0:
            var itm nitro.Item
            var w io.Writer
            var test nitro.Nitro
            test.EncodeItem(&itm, bytes, w)
            return 0
    
        case 1:
            var r io.Reader
            var test nitro.Nitro
            test.DecodeItem(num, bytes, r)
            return 0

        case 2:
            len := len(bytes)
            str1 := bytes[0:len/2]
            str2 := bytes[len/2:len]

            nitro.KVToBytes(str1, str2)
            return 0

        case 3:
            nitro.KVFromBytes(bytes)
            return 0

        default:
            len := len(bytes)
            str1 := bytes[0:len/2]
            str2 := bytes[len/2:len]

            nitro.CompareKV(str1, str2)
            return 0
        }
    }
    return 0
}

func Fuzz(data []byte) int {
    _ = mayhemit(data)
    return 0
}