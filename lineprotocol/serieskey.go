package lineprotocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// ErrDuplicateTagKey is returned by Decoder.SeriesKey when an entry
// contains the same tag key more than once. Duplicate tag keys make
// the series identity ambiguous, so they are always rejected rather
// than silently dropped.
var ErrDuplicateTagKey = errors.New("duplicate tag key")

// ErrInvalidSeriesKey is returned by DecodeSeriesKey when the
// provided key is not a valid series key as produced by
// Decoder.SeriesKey.
var ErrInvalidSeriesKey = errors.New("invalid series key")

// Tag represents a single tag key/value pair as decoded
// from a series key by DecodeSeriesKey.
type Tag struct {
	Key   []byte
	Value []byte
}

// SeriesKey returns a binary key that uniquely identifies the series
// of the current entry: its measurement name and its set of tags.
// Field keys, field values and the timestamp are not part of the key,
// so two entries with the same measurement and tags always produce
// the same key regardless of their fields or timestamp.
//
// The key is derived from the decoded (unescaped) measurement and tag
// values, so entries that differ only in tag order or in escaping
// spelling produce the same key. Comparison is purely byte-wise: no
// case folding or Unicode normalization is applied, so byte-distinct
// names always map to distinct keys.
//
// The encoding is unambiguous: all components are length-prefixed, so
// empty tag values, missing tags and separator characters (comma,
// equals, space) inside values are all represented distinctly.
//
// If the entry contains a duplicate tag key, SeriesKey reports an
// error for which errors.Is(err, ErrDuplicateTagKey) returns true,
// regardless of whether the duplicate values are the same.
//
// The returned slice is owned by the caller: it is not aliased with
// any internal Decoder buffer and remains valid and unchanged across
// subsequent calls to Next or any other Decoder method.
//
// SeriesKey consumes the measurement and tag sections of the entry;
// fields and timestamp can still be read afterwards with NextField
// and Time. It returns an error if the measurement section has
// already been passed (for example if SeriesKey is called twice on
// the same entry).
func (d *Decoder) SeriesKey() ([]byte, error) {
	measurement, err := d.Measurement()
	if err != nil {
		return nil, err
	}
	if measurement == nil {
		return nil, errors.New("cannot extract series key after the measurement section has been passed")
	}
	// The slices returned by Measurement and NextTag alias internal
	// decoder buffers that are reused by subsequent calls, so copy
	// everything we need into scratch before going further.
	scratch := make([]byte, 0, len(measurement)+64)
	scratch = append(scratch, measurement...)
	measurementLen := len(measurement)

	var tags []seriesTagRef
	for {
		tagKey, tagVal, err := d.NextTag()
		if err != nil {
			return nil, err
		}
		if tagKey == nil {
			break
		}
		ref := seriesTagRef{keyOff: len(scratch), keyLen: len(tagKey)}
		scratch = append(scratch, tagKey...)
		ref.valOff = len(scratch)
		ref.valLen = len(tagVal)
		scratch = append(scratch, tagVal...)
		tags = append(tags, ref)
	}
	// Sort tags by key so that tag order in the entry doesn't
	// affect the resulting key. Comparison is byte-wise only.
	sort.Slice(tags, func(i, j int) bool {
		return bytes.Compare(tagKeyBytes(scratch, tags[i]), tagKeyBytes(scratch, tags[j])) < 0
	})
	// Reject duplicate tag keys explicitly, whether or not their
	// values are the same.
	for i := 1; i < len(tags); i++ {
		if bytes.Equal(tagKeyBytes(scratch, tags[i-1]), tagKeyBytes(scratch, tags[i])) {
			return nil, fmt.Errorf("%w %q", ErrDuplicateTagKey, tagKeyBytes(scratch, tags[i]))
		}
	}
	key := make([]byte, 0, len(scratch)+len(tags)*4+binary.MaxVarintLen64)
	key = appendUvarint(key, uint64(measurementLen))
	key = append(key, scratch[:measurementLen]...)
	for _, ref := range tags {
		key = appendUvarint(key, uint64(ref.keyLen))
		key = append(key, scratch[ref.keyOff:ref.keyOff+ref.keyLen]...)
		key = appendUvarint(key, uint64(ref.valLen))
		key = append(key, scratch[ref.valOff:ref.valOff+ref.valLen]...)
	}
	return key, nil
}

// seriesTagRef refers to a tag key and value held in a scratch buffer.
type seriesTagRef struct {
	keyOff, keyLen int
	valOff, valLen int
}

func tagKeyBytes(scratch []byte, ref seriesTagRef) []byte {
	return scratch[ref.keyOff : ref.keyOff+ref.keyLen]
}

// DecodeSeriesKey decodes a series key previously produced by
// Decoder.SeriesKey back into its measurement name and tags,
// for display or debugging purposes.
//
// The returned slices refer to the bytes of key, preserving the
// exact byte semantics of the original names: no case folding or
// Unicode normalization is applied in either direction. The tags
// are returned in the canonical (byte-wise sorted) order used by
// the key.
//
// If key is not a valid series key, it returns an error for which
// errors.Is(err, ErrInvalidSeriesKey) returns true.
func DecodeSeriesKey(key []byte) (measurement []byte, tags []Tag, err error) {
	measurement, rest, ok := takeSeriesToken(key)
	if !ok {
		return nil, nil, fmt.Errorf("%w: truncated measurement", ErrInvalidSeriesKey)
	}
	for len(rest) > 0 {
		var tag Tag
		var ok bool
		tag.Key, rest, ok = takeSeriesToken(rest)
		if !ok {
			return nil, nil, fmt.Errorf("%w: truncated tag key", ErrInvalidSeriesKey)
		}
		tag.Value, rest, ok = takeSeriesToken(rest)
		if !ok {
			return nil, nil, fmt.Errorf("%w: truncated tag value", ErrInvalidSeriesKey)
		}
		tags = append(tags, tag)
	}
	return measurement, tags, nil
}

// takeSeriesToken takes a single length-prefixed token from the
// front of buf, returning the token and the remaining bytes.
func takeSeriesToken(buf []byte) (token, rest []byte, ok bool) {
	n, nLen := binary.Uvarint(buf)
	if nLen <= 0 || n > uint64(len(buf)-nLen) {
		return nil, nil, false
	}
	return buf[nLen : nLen+int(n)], buf[nLen+int(n):], true
}

// appendUvarint appends x to buf in unsigned varint encoding.
func appendUvarint(buf []byte, x uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], x)
	return append(buf, tmp[:n]...)
}
