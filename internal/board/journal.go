package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// journalRecord is one publication after the base snapshot: the leaf changes
// that turn the previous generation's document into this one.
type journalRecord struct {
	Generation uint64      `json:"generation"`
	Ops        []journalOp `json:"ops"`
}

// journalOp sets or deletes one value. Path holds object keys and, inside an
// array, a decimal index; an index op only replaces an existing element.
type journalOp struct {
	Path   []string        `json:"p"`
	Value  json.RawMessage `json:"v,omitempty"`
	Delete bool            `json:"d,omitempty"`
}

// diffRaw appends the ops that turn old into new. Objects recurse by key and
// arrays by index when both have the same length; any other change replaces
// the value at its path.
func diffRaw(old, new json.RawMessage, path []string, ops []journalOp) ([]journalOp, error) {
	if bytes.Equal(old, new) {
		return ops, nil
	}
	set := func() []journalOp {
		return append(ops, journalOp{Path: append([]string(nil), path...), Value: append(json.RawMessage(nil), new...)})
	}
	switch kind(old) {
	case '{':
		if kind(new) != '{' {
			return set(), nil
		}
		var a, b map[string]json.RawMessage
		if err := json.Unmarshal(old, &a); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(new, &b); err != nil {
			return nil, err
		}
		for _, key := range sortedKeys(a) {
			if _, ok := b[key]; !ok {
				ops = append(ops, journalOp{Path: append(append([]string(nil), path...), key), Delete: true})
			}
		}
		var err error
		for _, key := range sortedKeys(b) {
			child := append(append([]string(nil), path...), key)
			if prev, ok := a[key]; ok {
				if ops, err = diffRaw(prev, b[key], child, ops); err != nil {
					return nil, err
				}
				continue
			}
			ops = append(ops, journalOp{Path: child, Value: b[key]})
		}
		return ops, nil
	case '[':
		if kind(new) != '[' {
			return set(), nil
		}
		var a, b []json.RawMessage
		if err := json.Unmarshal(old, &a); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(new, &b); err != nil {
			return nil, err
		}
		if len(a) != len(b) {
			return set(), nil
		}
		var err error
		for i := range b {
			if ops, err = diffRaw(a[i], b[i], append(append([]string(nil), path...), strconv.Itoa(i)), ops); err != nil {
				return nil, err
			}
		}
		return ops, nil
	}
	return set(), nil
}

// kind is the first byte of a JSON value: '{', '[' or another value's start.
func kind(raw json.RawMessage) byte {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return c
	}
	return 0
}

// jnode is a JSON value decoded only as deep as ops reach: an untouched
// subtree keeps its raw bytes, so a replay decodes and encodes each touched
// container once however many records touch it.
type jnode struct {
	raw json.RawMessage
	obj map[string]*jnode
	arr []*jnode
}

func (n *jnode) decode() error {
	if n.obj != nil || n.arr != nil {
		return nil
	}
	switch kind(n.raw) {
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(n.raw, &m); err != nil {
			return err
		}
		n.obj = make(map[string]*jnode, len(m))
		for k, v := range m {
			n.obj[k] = &jnode{raw: v}
		}
	case '[':
		var a []json.RawMessage
		if err := json.Unmarshal(n.raw, &a); err != nil {
			return err
		}
		n.arr = make([]*jnode, len(a))
		for i, v := range a {
			n.arr[i] = &jnode{raw: v}
		}
		if n.arr == nil {
			n.arr = []*jnode{}
		}
	default:
		return errors.New("journal path crosses a scalar")
	}
	n.raw = nil
	return nil
}

func (n *jnode) apply(op journalOp) error {
	if len(op.Path) == 0 {
		return errors.New("journal op has an empty path")
	}
	for _, key := range op.Path[:len(op.Path)-1] {
		if err := n.decode(); err != nil {
			return err
		}
		next, err := n.child(key)
		if err != nil {
			return err
		}
		n = next
	}
	if err := n.decode(); err != nil {
		return err
	}
	last := op.Path[len(op.Path)-1]
	if n.obj != nil {
		if op.Delete {
			if _, ok := n.obj[last]; !ok {
				return fmt.Errorf("journal deletes missing key %q", last)
			}
			delete(n.obj, last)
			return nil
		}
		n.obj[last] = &jnode{raw: op.Value}
		return nil
	}
	i, err := strconv.Atoi(last)
	if err != nil || i < 0 || i >= len(n.arr) || op.Delete {
		return fmt.Errorf("journal op on array index %q is invalid", last)
	}
	n.arr[i] = &jnode{raw: op.Value}
	return nil
}

func (n *jnode) child(key string) (*jnode, error) {
	if n.obj != nil {
		if c, ok := n.obj[key]; ok {
			return c, nil
		}
		return nil, fmt.Errorf("journal path names missing key %q", key)
	}
	i, err := strconv.Atoi(key)
	if err != nil || i < 0 || i >= len(n.arr) {
		return nil, fmt.Errorf("journal path names missing index %q", key)
	}
	return n.arr[i], nil
}

func (n *jnode) encode(b *bytes.Buffer) error {
	switch {
	case n.obj != nil:
		b.WriteByte('{')
		for i, k := range sortedKeys(n.obj) {
			if i > 0 {
				b.WriteByte(',')
			}
			key, err := json.Marshal(k)
			if err != nil {
				return err
			}
			b.Write(key)
			b.WriteByte(':')
			if err := n.obj[k].encode(b); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case n.arr != nil:
		b.WriteByte('[')
		for i, c := range n.arr {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := c.encode(b); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		b.Write(n.raw)
	}
	return nil
}

// replayJournal applies records to base. Records must continue base's
// generation one by one; any gap, repeat or op that does not fit the document
// means the store needs recovery.
func replayJournal(base json.RawMessage, baseGeneration uint64, records []journalRecord) (json.RawMessage, error) {
	if len(records) == 0 {
		return base, nil
	}
	root := &jnode{raw: base}
	for i, r := range records {
		if r.Generation != baseGeneration+uint64(i)+1 {
			return nil, fmt.Errorf("board journal generation %d does not follow %d", r.Generation, baseGeneration+uint64(i))
		}
		for _, op := range r.Ops {
			if err := root.apply(op); err != nil {
				return nil, err
			}
		}
	}
	var b bytes.Buffer
	b.Grow(len(base))
	if err := root.encode(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
