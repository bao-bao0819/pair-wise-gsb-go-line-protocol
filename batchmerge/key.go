package batchmerge

import (
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"github.com/influxdata/line-protocol/v2/lineprotocol"
)

// canonicalTags returns the canonical tag set for a series identity: keys
// are lexicographically sorted and duplicate keys resolve to the value of
// the last occurrence, matching input sequence. All key and value bytes are
// copied so they do not alias the decoder's buffers.
func canonicalTags(tags []Tag) ([]Tag, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	idx := make([]int, len(tags))
	for i := range tags {
		if len(tags[i].Key) == 0 {
			return nil, fmt.Errorf("%w: empty tag key", ErrInvalidPoint)
		}
		idx[i] = i
	}
	// Stable sort keeps the later occurrence last for equal keys, so the
	// final pass keeps the last value.
	sort.SliceStable(idx, func(a, b int) bool {
		return string(tags[idx[a]].Key) < string(tags[idx[b]].Key)
	})
	out := make([]Tag, 0, len(tags))
	for i, orig := range idx {
		if i+1 < len(idx) && string(tags[orig].Key) == string(tags[idx[i+1]].Key) {
			continue
		}
		out = append(out, Tag{
			Key:   cloneBytes(tags[orig].Key),
			Value: cloneBytes(tags[orig].Value),
		})
	}
	return out, nil
}

// normalizeFields copies a point's fields into a map keyed by field name and
// resolves fields repeated within one input point by last occurrence. A
// string-valued lineprotocol.Value may reference the decoder buffer, so its
// bytes are copied.
func normalizeFields(fields []Field) (map[string]Field, error) {
	out := make(map[string]Field, len(fields))
	for _, f := range fields {
		if len(f.Key) == 0 {
			return nil, fmt.Errorf("%w: empty field key", ErrInvalidPoint)
		}
		f.Key = cloneBytes(f.Key)
		if f.Value.Kind() == lineprotocol.String {
			f.Value = stringValueCopy(f.Value.BytesV())
		}
		out[string(f.Key)] = f
	}
	return out, nil
}

func stringValueCopy(b []byte) lineprotocol.Value {
	v, ok := lineprotocol.StringValueFromBytes(b)
	if !ok {
		// b came from a previously validated Value, so it is valid UTF-8.
		panic("batchmerge: invalid utf-8 in string field")
	}
	return v
}

// seriesKey builds the canonical, collision-free identity of a series.
//
// Each component is length prefixed with an 8-byte big-endian length, which
// keeps lexicographic ordering consistent with component ordering and makes
// one key never a prefix of another: parsing a key is unambiguous.
func seriesKey(measurement []byte, tags []Tag) string {
	buf := make([]byte, 0, len(measurement)+len(tags)*16)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(measurement)))
	buf = append(buf, n[:]...)
	buf = append(buf, measurement...)
	binary.BigEndian.PutUint64(n[:], uint64(len(tags)))
	buf = append(buf, n[:]...)
	for _, t := range tags {
		binary.BigEndian.PutUint64(n[:], uint64(len(t.Key)))
		buf = append(buf, n[:]...)
		buf = append(buf, t.Key...)
		binary.BigEndian.PutUint64(n[:], uint64(len(t.Value)))
		buf = append(buf, n[:]...)
		buf = append(buf, t.Value...)
	}
	return string(buf)
}

// toNanoseconds converts a timestamp expressed in prec to an exact instant
// in nanoseconds, reporting false when the multiplication overflows.
func toNanoseconds(raw int64, prec lineprotocol.Precision) (int64, bool) {
	if prec > lineprotocol.Second {
		return 0, false
	}
	d := int64(prec.Duration())
	if d == 1 {
		return raw, true
	}
	ns := raw * d
	if ns/d != raw {
		return 0, false
	}
	return ns, true
}

func toMergedPoint(g *group) MergedPoint {
	keys := make([]string, 0, len(g.fields))
	for k := range g.fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fields := make([]Field, 0, len(keys))
	for _, k := range keys {
		fields = append(fields, g.fields[k])
	}
	tags := make([]Tag, len(g.tags))
	copy(tags, g.tags)
	p := MergedPoint{
		Measurement: g.measurement,
		Tags:        tags,
		Fields:      fields,
		HasTime:     g.hasTime,
	}
	if g.hasTime {
		p.Time = time.Unix(0, g.timeNS).UTC()
	}
	return p
}

func cloneBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	c := make([]byte, len(b))
	copy(c, b)
	return c
}
