package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// JSON config files are edited as ordered trees, so a merge keeps unknown
// keys and their order. Values are *object, []any, string, json.Number,
// bool or nil.

// object is a JSON object that remembers the order of its keys.
type object struct {
	keys []string
	vals map[string]any
}

func newObject() *object { return &object{vals: map[string]any{}} }

func (o *object) get(key string) (any, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// set replaces a value in place, or appends a new key.
func (o *object) set(key string, v any) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = v
}

// maxDepth bounds nesting, so a hostile file cannot exhaust the stack.
const maxDepth = 512

// decodeJSON parses one JSON value, keeping key order. A repeated key keeps
// its first position and its last value.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	dec.UseNumber()
	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the top-level value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch d {
	case '{':
		o := newObject()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := kt.(string)
			if !ok {
				return nil, fmt.Errorf("object key %v is not a string", kt)
			}
			v, err := decodeValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			o.set(key, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return o, nil
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := decodeValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unexpected %q", d)
}

// encodeJSON writes v with two-space indentation and a trailing newline.
func encodeJSON(v any) []byte {
	var b bytes.Buffer
	writeJSON(&b, v, 0)
	b.WriteByte('\n')
	return b.Bytes()
}

func writeJSON(b *bytes.Buffer, v any, depth int) {
	indent := func(d int) {
		for range d {
			b.WriteString("  ")
		}
	}
	switch t := v.(type) {
	case *object:
		if len(t.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range t.keys {
			indent(depth + 1)
			writeScalar(b, k)
			b.WriteString(": ")
			writeJSON(b, t.vals[k], depth+1)
			if i < len(t.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		indent(depth)
		b.WriteByte('}')
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range t {
			indent(depth + 1)
			writeJSON(b, e, depth+1)
			if i < len(t)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		indent(depth)
		b.WriteByte(']')
	case json.Number:
		b.WriteString(t.String())
	default:
		writeScalar(b, t)
	}
}

// writeScalar writes a string, bool or null without escaping <, > and &.
func writeScalar(b *bytes.Buffer, v any) {
	enc := json.NewEncoder(b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		b.WriteString("null") // unreachable for the values decodeJSON yields
		return
	}
	b.Truncate(b.Len() - 1) // Encode's newline
}

// jsonEqual compares two decoded values; object key order does not matter.
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case *object:
		y, ok := b.(*object)
		if !ok || len(x.vals) != len(y.vals) {
			return false
		}
		for k, v := range x.vals {
			w, ok := y.vals[k]
			if !ok || !jsonEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

// childObject returns o[key] as an object, adding an empty one when the key
// is missing. path names the key in errors.
func childObject(o *object, key, path string) (child *object, added bool, err error) {
	v, ok := o.get(key)
	if !ok {
		child = newObject()
		o.set(key, child)
		return child, true, nil
	}
	child, ok = v.(*object)
	if !ok {
		return nil, false, fmt.Errorf("%s is not a JSON object", path)
	}
	return child, false, nil
}

// childArray returns o[key] as an array; a missing key is an empty array.
func childArray(o *object, key, path string) ([]any, error) {
	v, ok := o.get(key)
	if !ok {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a JSON array", path)
	}
	return arr, nil
}
