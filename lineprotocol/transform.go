package lineprotocol

import (
	"fmt"
)

// FieldReplaceFunc is called by ReplaceFields for each field occurrence
// whose decoded key is in the whitelist. The key is the decoded
// (unescaped) field key and value holds the parsed field value;
// value.Kind reports its type.
//
// The returned value is encoded with the usual line-protocol value
// encoding (see Value.AppendBytes), so it may contain arbitrary
// characters - quotes, backslashes, newlines and commas are escaped
// as needed and cannot corrupt the surrounding line.
//
// If it returns an error, ReplaceFields rejects the whole line.
type FieldReplaceFunc func(key []byte, value Value) (Value, error)

// ReplaceFields parses line, which must contain exactly one
// line-protocol entry, and returns a copy of it in which the values
// of the fields named in fields have been replaced by the results of
// calling replace.
//
// Field names in fields are matched against the decoded (unescaped)
// field keys, not the raw escaped text found in the line.
//
// Only the raw byte ranges of the replaced field values are modified:
// the measurement, tags, non-matching fields, field order, timestamp
// and line ending are all preserved byte-for-byte, and the replacement
// values are encoded with the normal value encoding so that quotes
// and newlines cannot break the line.
//
// If a field key appears more than once, replace is invoked once for
// each occurrence and each occurrence is replaced independently (it
// is never the case that only the last occurrence is changed).
//
// The transformation is all-or-nothing: if the line has a syntax
// error, if a matching field's value cannot be parsed, or if replace
// returns an error for any matching field, ReplaceFields returns an
// error and no partial output is produced.
func ReplaceFields(line []byte, fields []string, replace FieldReplaceFunc) ([]byte, error) {
	if replace == nil {
		return nil, fmt.Errorf("lineprotocol: ReplaceFields requires a non-nil replace function")
	}
	whitelist := make(map[string]bool, len(fields))
	for _, f := range fields {
		whitelist[f] = true
	}

	// Fully decode the entry first. This validates the syntax of the
	// whole line (measurement, tags, fields, timestamp and any
	// trailing text) and gives us the decoded keys and values.
	d := NewDecoderWithBytes(line)
	if !d.Next() {
		return nil, fmt.Errorf("lineprotocol: no line-protocol entry found")
	}
	if _, err := d.Measurement(); err != nil {
		return nil, err
	}
	for {
		key, _, err := d.NextTag()
		if err != nil {
			return nil, err
		}
		if key == nil {
			break
		}
	}
	var keys [][]byte
	var kinds []ValueKind
	var vals [][]byte
	for {
		key, kind, val, err := d.NextFieldBytes()
		if err != nil {
			return nil, err
		}
		if key == nil {
			break
		}
		// The decoder reuses its buffers, so copy the data we need
		// to keep around.
		keys = append(keys, append([]byte(nil), key...))
		kinds = append(kinds, kind)
		vals = append(vals, append([]byte(nil), val...))
	}
	if _, err := d.TimeBytes(); err != nil {
		return nil, err
	}
	if d.Next() {
		return nil, fmt.Errorf("lineprotocol: more than one entry found; ReplaceFields handles a single entry")
	}

	// Find the raw byte ranges of the field values in the original
	// line so that everything else can be preserved exactly.
	ranges, err := fieldValueRanges(line)
	if err != nil {
		return nil, err
	}
	if len(ranges) != len(keys) {
		return nil, fmt.Errorf("lineprotocol: internal error: found %d field values but decoded %d fields", len(ranges), len(keys))
	}

	type replacement struct {
		start, end int
		data       []byte
	}
	var repls []replacement
	for i, key := range keys {
		if !whitelist[string(key)] {
			continue
		}
		v, err := NewValueFromBytes(kinds[i], vals[i])
		if err != nil {
			return nil, fmt.Errorf("lineprotocol: cannot parse value for field %q: %w", key, err)
		}
		nv, err := replace(key, v)
		if err != nil {
			return nil, fmt.Errorf("lineprotocol: cannot replace value for field %q: %w", key, err)
		}
		repls = append(repls, replacement{
			start: ranges[i][0],
			end:   ranges[i][1],
			data:  nv.AppendBytes(nil),
		})
	}
	if len(repls) == 0 {
		return append([]byte(nil), line...), nil
	}
	out := make([]byte, 0, len(line))
	pos := 0
	for _, r := range repls {
		out = append(out, line[pos:r.start]...)
		out = append(out, r.data...)
		pos = r.end
	}
	return append(out, line[pos:]...), nil
}

// fieldValueRanges returns the raw byte range [start, end) of every
// field value in line, in order of appearance. The range of a string
// value includes its surrounding quotes and the range of a numeric
// value includes any type suffix, so replacing a range with a freshly
// encoded value always yields a well formed field.
//
// It assumes that line holds a single syntactically valid
// line-protocol entry as verified by Decoder.
func fieldValueRanges(line []byte) ([][2]int, error) {
	// Skip leading blank lines, comments and white space, mirroring
	// the decoder's behaviour when it advances to an entry.
	i := 0
	for i < len(line) {
		switch line[i] {
		case ' ', '\n':
			i++
		case '\r':
			if i+1 < len(line) && line[i+1] == '\n' {
				i += 2
			} else {
				i++
			}
		case '#':
			// Comment line: skip to and including the newline.
			for i < len(line) && line[i] != '\n' {
				i++
			}
		default:
			goto entryStart
		}
	}
entryStart:
	// Skip the measurement and tags by scanning for the first
	// unescaped space. The union of the escapable characters in the
	// measurement, tag-key and tag-value sections is ",= ", which is
	// exactly what tagKeyEscapes covers.
	for i < len(line) {
		c := line[i]
		if c == '\\' && i+1 < len(line) && tagKeyEscapes.revTable[line[i+1]] != 0 {
			i += 2
			continue
		}
		if c == ' ' {
			break
		}
		i++
	}
	// The decoder allows multiple spaces between the tags and the
	// fields, so discard them all.
	for i < len(line) && line[i] == ' ' {
		i++
	}
	var ranges [][2]int
	for i < len(line) {
		keyEnd := scanEscapedToken(line, i, fieldKeyChars, &fieldKeyEscapes.revTable)
		if keyEnd >= len(line) || line[keyEnd] != '=' {
			return nil, fmt.Errorf("lineprotocol: internal error: expected '=' after field key at byte %d", keyEnd)
		}
		valueStart := keyEnd + 1
		var valueEnd int
		if valueStart < len(line) && line[valueStart] == '"' {
			// String value: find the closing quote, skipping
			// escaped characters. Note that string values may
			// contain commas, spaces and even newlines.
			j := valueStart + 1
			for j < len(line) {
				c := line[j]
				if c == '\\' && j+1 < len(line) && fieldStringValEscapes.revTable[line[j+1]] != 0 {
					j += 2
					continue
				}
				if c == '"' {
					break
				}
				j++
			}
			if j >= len(line) {
				return nil, fmt.Errorf("lineprotocol: internal error: unterminated string field value at byte %d", valueStart)
			}
			valueEnd = j + 1
		} else {
			// Boolean, integer, unsigned or float value: a bare
			// token terminated by a comma, white space or the end
			// of the line.
			valueEnd = valueStart
			for valueEnd < len(line) && fieldValChars.get(line[valueEnd]) {
				valueEnd++
			}
		}
		ranges = append(ranges, [2]int{valueStart, valueEnd})
		if valueEnd < len(line) && line[valueEnd] == ',' {
			i = valueEnd + 1
			continue
		}
		// Anything after the last field value (white space,
		// timestamp, line ending) is preserved verbatim.
		break
	}
	return ranges, nil
}

// scanEscapedToken returns the index of the first byte at or after
// start that is not part of a token consisting of characters in set,
// where a backslash followed by a character in escapeTable counts as
// part of the token.
func scanEscapedToken(line []byte, start int, set *byteSet, escapeTable *[256]byte) int {
	i := start
	for i < len(line) {
		c := line[i]
		if c == '\\' && i+1 < len(line) && escapeTable[line[i+1]] != 0 {
			i += 2
			continue
		}
		if !set.get(c) {
			break
		}
		i++
	}
	return i
}
