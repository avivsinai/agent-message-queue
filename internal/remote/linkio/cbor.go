package linkio

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// cborDecode reads the subset of CBOR (RFC 8949) a WebAuthn registration
// carries: unsigned and negative integers, byte and text strings, arrays,
// maps and the simple values false, true and null. Integers decode as int64,
// maps as map[any]any. Trailing bytes after the first item are allowed: the
// credential public key sits inside authenticator data, and an extension map
// may follow it.
func cborDecode(data []byte) (any, error) {
	v, _, err := cborItem(data, 0)
	return v, err
}

const cborMaxDepth = 16

func cborItem(data []byte, depth int) (any, []byte, error) {
	if depth > cborMaxDepth {
		return nil, nil, errors.New("cbor: nested too deep")
	}
	if len(data) == 0 {
		return nil, nil, errors.New("cbor: unexpected end")
	}
	major, info := data[0]>>5, data[0]&0x1f
	n, rest, err := cborArg(info, data[1:])
	if err != nil {
		return nil, nil, err
	}
	switch major {
	case 0:
		if n > 1<<63-1 {
			return nil, nil, errors.New("cbor: integer too large")
		}
		return int64(n), rest, nil
	case 1:
		if n > 1<<63-1 {
			return nil, nil, errors.New("cbor: integer too large")
		}
		return -1 - int64(n), rest, nil
	case 2, 3:
		if n > uint64(len(rest)) {
			return nil, nil, errors.New("cbor: string longer than its input")
		}
		if major == 2 {
			return append([]byte(nil), rest[:n]...), rest[n:], nil
		}
		return string(rest[:n]), rest[n:], nil
	case 4:
		if n > uint64(len(rest)) {
			return nil, nil, errors.New("cbor: array longer than its input")
		}
		out := make([]any, 0, n)
		for range n {
			var v any
			if v, rest, err = cborItem(rest, depth+1); err != nil {
				return nil, nil, err
			}
			out = append(out, v)
		}
		return out, rest, nil
	case 5:
		if n > uint64(len(rest)) {
			return nil, nil, errors.New("cbor: map longer than its input")
		}
		out := make(map[any]any, n)
		for range n {
			var k, v any
			if k, rest, err = cborItem(rest, depth+1); err != nil {
				return nil, nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, nil, errors.New("cbor: map key is not an integer or a text string")
			}
			if v, rest, err = cborItem(rest, depth+1); err != nil {
				return nil, nil, err
			}
			if _, dup := out[k]; dup {
				return nil, nil, fmt.Errorf("cbor: duplicate map key %v", k)
			}
			out[k] = v
		}
		return out, rest, nil
	case 7:
		switch info {
		case 20:
			return false, rest, nil
		case 21:
			return true, rest, nil
		case 22:
			return nil, rest, nil
		}
	}
	return nil, nil, fmt.Errorf("cbor: unsupported item (major %d, info %d)", major, info)
}

// cborArg reads the argument of an initial byte. Indefinite lengths are not
// part of WebAuthn's canonical CBOR and are refused.
func cborArg(info byte, data []byte) (uint64, []byte, error) {
	switch {
	case info < 24:
		return uint64(info), data, nil
	case info == 24 && len(data) >= 1:
		return uint64(data[0]), data[1:], nil
	case info == 25 && len(data) >= 2:
		return uint64(binary.BigEndian.Uint16(data)), data[2:], nil
	case info == 26 && len(data) >= 4:
		return uint64(binary.BigEndian.Uint32(data)), data[4:], nil
	case info == 27 && len(data) >= 8:
		return binary.BigEndian.Uint64(data), data[8:], nil
	}
	return 0, nil, errors.New("cbor: bad or truncated argument")
}
