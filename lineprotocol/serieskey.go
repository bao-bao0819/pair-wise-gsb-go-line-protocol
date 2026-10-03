package lineprotocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// ErrDuplicateTagKey is returned (wrapped) when a series key is
// requested for an entry or tag set that contains the same tag key
// more than once. Duplicate tags make series identity ambiguous, so
// they are always rejected explicitly, whether or not the duplicated
// values agree - the first occurrence is never silently kept.
var ErrDuplicateTagKey = errors.New("duplicate tag key")

// Tag holds a single decoded tag key/value pair.
type Tag struct {
	Key   []byte
	Value []byte
}

// SeriesKey returns a binary key that identifies the series of the
// current entry - that is, its measurement name and tag set. Field
// values and the timestamp never take part in the key, so two entries
// that differ only in those sections produce the same key.
//
// The key is built from the decoded (unescaped) values, so entries
// whose tags appear in a different order, or whose escape spellings
// differ but decode to the same bytes, produce identical keys. The
// encoding is length-prefixed and unambiguous: separator characters
// (comma, equals, space) inside values, empty tag values and missing
// tags all produce distinct keys. Bytes are compared exactly - no case
// folding or Unicode normalization is applied - so sequences that
// differ only in letter case or Unicode canonical form remain
// distinct series.
//
// If the entry contains a duplicate tag key, SeriesKey returns an
// error for which errors.Is(err, ErrDuplicateTagKey) reports true.
//
// SeriesKey must be called after Next and before NextField or Time.
// It consumes the measurement and tag sections of the entry, so the
// fields and timestamp can still be read afterwards.
//
// The returned slice is freshly allocated on each call and remains
// valid (and unchanged) across subsequent calls to Next or any other
// Decoder method.
func (d *Decoder) SeriesKey() ([]byte, error) {
	measurement, err := d.Measurement()
	if err != nil {
		return nil, err
	}
	// The measurement and tag slices returned by the Decoder refer to
	// internal buffers that are reused by subsequent calls, so copy
	// them before decoding further.
	measurement = append([]byte(nil), measurement...)
	var tags []Tag
	for {
		k, v, err := d.NextTag()
		if err != nil {
			return nil, err
		}
		if k == nil {
			break
		}
		tags = append(tags, Tag{
			Key:   append([]byte(nil), k...),
			Value: append([]byte(nil), v...),
		})
	}
	return NewSeriesKey(measurement, tags)
}

// NewSeriesKey builds a series key from a decoded measurement name
// and tag set. The tags may be in any order; they are sorted by tag
// key bytes before encoding, so the same tag set always produces the
// same key. The caller's slices are not retained or modified.
//
// As with Decoder.SeriesKey, duplicate tag keys are rejected with an
// error wrapping ErrDuplicateTagKey, and the returned key is
// unambiguous with respect to separators, empty values and missing
// tags. See Decoder.SeriesKey for the full key semantics.
func NewSeriesKey(measurement []byte, tags []Tag) ([]byte, error) {
	sorted := make([]Tag, len(tags))
	copy(sorted, tags)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i].Key, sorted[j].Key) < 0
	})
	for i := 1; i < len(sorted); i++ {
		if bytes.Equal(sorted[i-1].Key, sorted[i].Key) {
			return nil, fmt.Errorf("cannot make series key: tag key %q appears more than once: %w", sorted[i].Key, ErrDuplicateTagKey)
		}
	}
	// The key is a sequence of uvarint-length-prefixed byte strings:
	// the measurement, the tag count, then each tag's key and value in
	// sorted-key order. Length prefixing means any byte (including
	// separator characters) can appear inside a component without
	// ambiguity, and empty components are distinct from absent ones.
	n := binary.MaxVarintLen64*(2+2*len(sorted)) + len(measurement)
	for _, t := range sorted {
		n += len(t.Key) + len(t.Value)
	}
	key := make([]byte, 0, n)
	key = appendSeriesKeyComponent(key, measurement)
	key = appendUvarint(key, uint64(len(sorted)))
	for _, t := range sorted {
		key = appendSeriesKeyComponent(key, t.Key)
		key = appendSeriesKeyComponent(key, t.Value)
	}
	return key, nil
}

// ParseSeriesKey decodes a series key produced by Decoder.SeriesKey
// or NewSeriesKey back into its measurement name and tags. The tags
// are returned in the key's canonical order (sorted by tag key
// bytes).
//
// It is intended for displaying or logging series identity: the
// returned slices preserve the exact bytes held in the key - no case
// folding or Unicode normalization is applied - so series whose keys
// differ byte-wise never collapse into the same displayed value.
//
// The returned slices share storage with key and must not be mutated.
// It returns an error if key is truncated or has trailing data.
func ParseSeriesKey(key []byte) (measurement []byte, tags []Tag, err error) {
	rest := key
	if measurement, rest, err = takeSeriesKeyComponent(rest); err != nil {
		return nil, nil, err
	}
	n, rest, err := takeUvarint(rest)
	if err != nil {
		return nil, nil, err
	}
	if n > uint64(len(rest)) {
		// Each tag needs at least two length prefixes, so this
		// many tags can never fit. Avoid a huge allocation.
		return nil, nil, fmt.Errorf("invalid series key: tag count %d too large for %d remaining bytes", n, len(rest))
	}
	tags = make([]Tag, 0, n)
	for i := uint64(0); i < n; i++ {
		var t Tag
		if t.Key, rest, err = takeSeriesKeyComponent(rest); err != nil {
			return nil, nil, fmt.Errorf("invalid series key: tag %d key: %w", i, err)
		}
		if t.Value, rest, err = takeSeriesKeyComponent(rest); err != nil {
			return nil, nil, fmt.Errorf("invalid series key: tag %d value: %w", i, err)
		}
		tags = append(tags, t)
	}
	if len(rest) != 0 {
		return nil, nil, fmt.Errorf("invalid series key: %d trailing bytes", len(rest))
	}
	return measurement, tags, nil
}

// appendSeriesKeyComponent appends a length-prefixed component to key.
func appendSeriesKeyComponent(key, component []byte) []byte {
	key = appendUvarint(key, uint64(len(component)))
	return append(key, component...)
}

// takeSeriesKeyComponent reads a length-prefixed component from the
// front of key, returning the component and the remaining bytes.
func takeSeriesKeyComponent(key []byte) (component, rest []byte, err error) {
	n, rest, err := takeUvarint(key)
	if err != nil {
		return nil, nil, err
	}
	if n > uint64(len(rest)) {
		return nil, nil, fmt.Errorf("invalid series key: component length %d exceeds %d remaining bytes", n, len(rest))
	}
	return rest[:n], rest[n:], nil
}

// appendUvarint appends x to buf as a uvarint.
func appendUvarint(buf []byte, x uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], x)
	return append(buf, tmp[:n]...)
}

// takeUvarint reads a uvarint from the front of buf.
func takeUvarint(buf []byte) (x uint64, rest []byte, err error) {
	x, n := binary.Uvarint(buf)
	if n <= 0 {
		return 0, nil, fmt.Errorf("invalid series key: bad length prefix")
	}
	return x, buf[n:], nil
}
