package lineprotocol

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
)

// seriesKey decodes a single line-protocol entry and returns its
// series key.
func seriesKey(c *qt.C, line string) []byte {
	dec := NewDecoderWithBytes([]byte(line))
	c.Assert(dec.Next(), qt.IsTrue)
	key, err := dec.SeriesKey()
	c.Assert(err, qt.IsNil)
	return key
}

// TestSeriesKeyEquivalentLines checks that lines that mean the same
// series produce the same key, even when they differ textually.
func TestSeriesKeyEquivalentLines(t *testing.T) {
	c := qt.New(t)
	equivalentPairs := [][2]string{
		// Tag order doesn't matter.
		{`cpu,host=a,region=us v=1`, `cpu,region=us,host=a v=1`},
		// Fields and timestamp are not part of the key.
		{`cpu,host=a v=1 100`, `cpu,host=a v=99 200`},
		{`cpu,host=a,region=us v=1`, `cpu,host=a,region=us other="x",n=2i 555`},
		// Escaped separators in measurement, tag key and tag value,
		// with tags in a different order.
		{`we\,\ ird,a\=b=x\,y,z=1 f=1`, `we\,\ ird,z=1,a\=b=x\,y f=2`},
		// Backslashes and multibyte characters, reordered.
		{"m,bs=a\\\\b,host=\xe5\xa4\x9a\xe5\xad\x97\xe8\x8a\x82 v=1", "m,host=\xe5\xa4\x9a\xe5\xad\x97\xe8\x8a\x82,bs=a\\\\b v=1"},
		// No tags at all.
		{`cpu v=1`, `cpu v=2 1000`},
	}
	for i, pair := range equivalentPairs {
		c.Run(fmt.Sprintf("pair %d", i), func(c *qt.C) {
			k1 := seriesKey(c, pair[0])
			k2 := seriesKey(c, pair[1])
			c.Check(k1, qt.DeepEquals, k2, qt.Commentf("%q vs %q", pair[0], pair[1]))
		})
	}
}

// TestSeriesKeyDistinctLines checks that lines from different series
// produce different keys.
func TestSeriesKeyDistinctLines(t *testing.T) {
	c := qt.New(t)
	distinctPairs := [][2]string{
		// Different measurement.
		{`cpu,host=a v=1`, `mem,host=a v=1`},
		// Case is significant: no case folding.
		{`cpu,host=a v=1`, `cpu,host=A v=1`},
		{`cpu,host=a v=1`, `CPU,host=a v=1`},
		// Missing tag vs present tag.
		{`cpu,host=a v=1`, `cpu,host=a,region=us v=1`},
		// Separator inside a value vs an extra tag:
		// host="a,b" must not collide with host=a,b=x.
		{`cpu,host=a\,b v=1`, `cpu,host=a,b=x v=1`},
		// Tag key vs tag value: a="b" must not collide with b="a".
		{`cpu,a=b v=1`, `cpu,b=a v=1`},
		// Measurement must not absorb a tag: m="ab" vs m="a",b="c".
		{`ab v=1`, `a,b=c v=1`},
		// Unicode normalization is not applied: composed vs
		// decomposed forms of "é" are different series.
		{"cpu,host=caf\xc3\xa9 v=1", "cpu,host=cafe\xcc\x81 v=1"},
		// Backslash is significant.
		{`cpu,host=a\\b v=1`, `cpu,host=ab v=1`},
	}
	for i, pair := range distinctPairs {
		c.Run(fmt.Sprintf("pair %d", i), func(c *qt.C) {
			k1 := seriesKey(c, pair[0])
			k2 := seriesKey(c, pair[1])
			c.Check(bytes.Equal(k1, k2), qt.IsFalse, qt.Commentf("%q vs %q", pair[0], pair[1]))
		})
	}
}

// TestSeriesKeyEmptyVsMissing checks that an empty component is
// encoded distinctly from a missing one. Empty tag values aren't
// valid line protocol, so construct the keys directly.
func TestSeriesKeyEmptyVsMissing(t *testing.T) {
	c := qt.New(t)
	// measurement "m" with tag a="" vs measurement "m" with no tags.
	withEmpty := appendUvarint(nil, 1)
	withEmpty = append(withEmpty, 'm')
	withEmpty = appendUvarint(withEmpty, 1)
	withEmpty = append(withEmpty, 'a')
	withEmpty = appendUvarint(withEmpty, 0)

	without := appendUvarint(nil, 1)
	without = append(without, 'm')

	c.Assert(withEmpty, qt.Not(qt.DeepEquals), without)

	// Both decode correctly and distinctly.
	m1, tags1, err := DecodeSeriesKey(withEmpty)
	c.Assert(err, qt.IsNil)
	c.Assert(string(m1), qt.Equals, "m")
	c.Assert(tags1, qt.HasLen, 1)
	c.Assert(string(tags1[0].Key), qt.Equals, "a")
	c.Assert(tags1[0].Value, qt.HasLen, 0)

	m2, tags2, err := DecodeSeriesKey(without)
	c.Assert(err, qt.IsNil)
	c.Assert(string(m2), qt.Equals, "m")
	c.Assert(tags2, qt.HasLen, 0)
}

// TestSeriesKeyDuplicateTagKey checks that duplicate tag keys are
// always rejected, whether the values are the same or different.
func TestSeriesKeyDuplicateTagKey(t *testing.T) {
	c := qt.New(t)
	for _, line := range []string{
		`cpu,host=a,host=b v=1`,
		`cpu,host=a,host=a v=1`,
		`cpu,host=a,region=us,host=a v=1`,
	} {
		dec := NewDecoderWithBytes([]byte(line))
		c.Assert(dec.Next(), qt.IsTrue)
		_, err := dec.SeriesKey()
		c.Assert(err, qt.Not(qt.IsNil), qt.Commentf("line %q", line))
		c.Check(errors.Is(err, ErrDuplicateTagKey), qt.IsTrue, qt.Commentf("line %q, error %v", line, err))
	}
}

// TestSeriesKeyOwnsStorage checks that a returned key is not
// overwritten by subsequent Decoder calls.
func TestSeriesKeyOwnsStorage(t *testing.T) {
	c := qt.New(t)
	dec := NewDecoder(strings.NewReader("cpu,host=a,region=us v=1\nmem,host=b,zone=zz,extra=longtagvalue x=2\n"))
	c.Assert(dec.Next(), qt.IsTrue)
	key1, err := dec.SeriesKey()
	c.Assert(err, qt.IsNil)
	key1Copy := append([]byte(nil), key1...)

	c.Assert(dec.Next(), qt.IsTrue)
	key2, err := dec.SeriesKey()
	c.Assert(err, qt.IsNil)

	c.Check(key1, qt.DeepEquals, key1Copy)
	c.Check(bytes.Equal(key1, key2), qt.IsFalse)
	c.Assert(dec.Next(), qt.IsFalse)
	c.Assert(dec.Err(), qt.IsNil)
}

// TestSeriesKeyFieldsStillReadable checks that extracting the series
// key doesn't prevent reading the rest of the entry.
func TestSeriesKeyFieldsStillReadable(t *testing.T) {
	c := qt.New(t)
	dec := NewDecoderWithBytes([]byte(`cpu,host=a v=42 100`))
	c.Assert(dec.Next(), qt.IsTrue)
	_, err := dec.SeriesKey()
	c.Assert(err, qt.IsNil)

	fieldKey, value, err := dec.NextField()
	c.Assert(err, qt.IsNil)
	c.Assert(string(fieldKey), qt.Equals, "v")
	c.Assert(value.Kind(), qt.Equals, Float)

	ts, err := dec.TimeBytes()
	c.Assert(err, qt.IsNil)
	c.Assert(string(ts), qt.Equals, "100")
}

// TestSeriesKeyAfterMeasurementPassed checks that SeriesKey reports
// an error when called too late.
func TestSeriesKeyAfterMeasurementPassed(t *testing.T) {
	c := qt.New(t)
	dec := NewDecoderWithBytes([]byte(`cpu,host=a v=1`))
	c.Assert(dec.Next(), qt.IsTrue)
	_, _, err := dec.NextField()
	c.Assert(err, qt.IsNil)
	_, err = dec.SeriesKey()
	c.Assert(err, qt.Not(qt.IsNil))
}

// TestDecodeSeriesKeyRoundTrip checks that DecodeSeriesKey recovers
// the exact decoded bytes of the measurement and tags.
func TestDecodeSeriesKeyRoundTrip(t *testing.T) {
	c := qt.New(t)
	// Measurement contains comma and space; tag key contains "=";
	// tag values contain ",", a backslash and multibyte characters.
	key := seriesKey(c, "we\\,\\ ird,z=\xe5\xa4\x9a\xe5\xad\x97\xe8\x8a\x82,a\\=b=x\\,y,bs=a\\\\b f=1 100")
	measurement, tags, err := DecodeSeriesKey(key)
	c.Assert(err, qt.IsNil)
	c.Assert(string(measurement), qt.Equals, "we, ird")
	// Tags come back in canonical byte-wise sorted key order.
	tagStrings := make([][2]string, len(tags))
	for i, tag := range tags {
		tagStrings[i] = [2]string{string(tag.Key), string(tag.Value)}
	}
	c.Assert(tagStrings, qt.DeepEquals, [][2]string{
		{"a=b", "x,y"},
		{"bs", `a\\b`},
		{"z", "\xe5\xa4\x9a\xe5\xad\x97\xe8\x8a\x82"},
	})
}

// TestDecodeSeriesKeyInvalid checks that malformed keys are rejected.
func TestDecodeSeriesKeyInvalid(t *testing.T) {
	c := qt.New(t)
	valid := seriesKey(c, `cpu,host=a v=1`)
	trailing := append(append([]byte(nil), valid...), 3)
	for _, key := range [][]byte{
		nil,
		{},
		{0x80},               // truncated varint
		{5, 'a'},             // measurement length exceeds data
		valid[:len(valid)-1], // truncated final token
		trailing,             // trailing tag key with no value
	} {
		_, _, err := DecodeSeriesKey(key)
		c.Check(err, qt.Not(qt.IsNil), qt.Commentf("key %v", key))
		c.Check(errors.Is(err, ErrInvalidSeriesKey), qt.IsTrue)
	}
}

// TestSeriesKeySyntaxError checks that a syntax error in the tags
// section is reported rather than producing a key.
func TestSeriesKeySyntaxError(t *testing.T) {
	c := qt.New(t)
	dec := NewDecoderWithBytes([]byte(`cpu,host v=1`))
	c.Assert(dec.Next(), qt.IsTrue)
	_, err := dec.SeriesKey()
	c.Assert(err, qt.Not(qt.IsNil))
	c.Assert(err, qt.Satisfies, isDecodeError)
}
