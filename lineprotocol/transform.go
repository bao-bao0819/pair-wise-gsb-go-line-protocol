package lineprotocol

import (
	"fmt"
)

// FieldTransformFunc is called by TransformFields for each field whose
// decoded key appears in the given whitelist. It receives the decoded
// field key and the parsed field value (not the raw line-protocol
// syntax), and returns the value that should replace it.
//
// The returned value is encoded with the usual line-protocol encoding
// rules (see Value.AppendBytes), so strings containing quotes,
// backslashes or newlines cannot corrupt the structure of the line.
//
// The key and the contents of v are only valid for the duration of the
// call. If f returns an error, TransformFields returns an error and
// produces no output.
type FieldTransformFunc func(key []byte, v Value) (Value, error)

// TransformFields returns a copy of the line-protocol entry in line in
// which the value of every field whose decoded key appears in fields
// has been replaced by the value returned from f.
//
// Field keys are matched against fields after unescaping, so a
// whitelist entry "my key" matches the raw key `my\ key`.
//
// Only the raw value fragments of matching fields are rewritten:
// the measurement, tags, non-matching fields (including their exact
// original formatting), field order and timestamp are all preserved
// byte for byte, as is any trailing newline.
//
// If a whitelisted key occurs more than once in the entry, f is called
// for each occurrence in order of appearance and each occurrence is
// replaced independently - the last one does not silently win.
//
// TransformFields is all-or-nothing: if the entry has a syntax error,
// if a matching field's value cannot be parsed, or if f returns an
// error for any matching field, it returns an error and no output.
// The input must contain exactly one entry (empty lines and comment
// lines around it are permitted and preserved).
func TransformFields(line []byte, fields []string, f FieldTransformFunc) ([]byte, error) {
	whitelist := make(map[string]bool, len(fields))
	for _, k := range fields {
		whitelist[k] = true
	}
	dec := NewDecoderWithBytes(line)
	if !dec.Next() {
		return nil, fmt.Errorf("no line-protocol entry found")
	}
	if _, err := dec.Measurement(); err != nil {
		return nil, err
	}
	for {
		key, _, err := dec.NextTag()
		if err != nil {
			return nil, err
		}
		if key == nil {
			break
		}
	}
	// Note: dec was created with NewDecoderWithBytes, so its buffer is
	// never moved or grown and dec.valStart/valEnd are always valid
	// indexes into line.
	type replacement struct {
		start, end int
		v          Value
	}
	var repls []replacement
	for {
		key, kind, data, err := dec.NextFieldBytes()
		if err != nil {
			return nil, err
		}
		if key == nil {
			break
		}
		if !whitelist[string(key)] {
			continue
		}
		v, err := NewValueFromBytes(kind, data)
		if err != nil {
			return nil, fmt.Errorf("cannot parse value for field %q: %w", key, err)
		}
		newv, err := f(key, v)
		if err != nil {
			return nil, fmt.Errorf("cannot transform field %q: %w", key, err)
		}
		repls = append(repls, replacement{dec.valStart, dec.valEnd, newv})
	}
	if _, err := dec.TimeBytes(); err != nil {
		return nil, err
	}
	if dec.Next() {
		return nil, fmt.Errorf("found more than one line-protocol entry")
	}
	if err := dec.Err(); err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(line))
	pos := 0
	for _, r := range repls {
		out = append(out, line[pos:r.start]...)
		out = r.v.AppendBytes(out)
		pos = r.end
	}
	return append(out, line[pos:]...), nil
}
