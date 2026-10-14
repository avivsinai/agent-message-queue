package canon

import (
	"encoding/binary"
	"fmt"
)

func AppendField(dst, field []byte) []byte {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(field)))
	dst = append(dst, length[:]...)
	return append(dst, field...)
}

func ReadField(raw []byte, fieldCap int) (field, rest []byte, err error) {
	if len(raw) < 4 {
		return nil, nil, fmt.Errorf("canon: short length")
	}
	n := binary.BigEndian.Uint32(raw[:4])
	raw = raw[4:]
	if int(n) > fieldCap {
		return nil, nil, fmt.Errorf("canon: field exceeds %d bytes", fieldCap)
	}
	if int(n) > len(raw) {
		return nil, nil, fmt.Errorf("canon: short field")
	}
	return raw[:n], raw[n:], nil
}

func Fields(raw []byte, n int, fieldCap int) ([][]byte, error) {
	out := make([][]byte, 0, n)
	rest := raw
	for i := 0; i < n; i++ {
		field, next, err := ReadField(rest, fieldCap)
		if err != nil {
			return nil, err
		}
		out = append(out, field)
		rest = next
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("canon: trailing bytes")
	}
	return out, nil
}
