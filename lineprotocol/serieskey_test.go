package lineprotocol

import (
	"errors"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
)

// seriesKeyForLine decodes a single line and returns its series key.
func seriesKeyForLine(c *qt.C, line string) []byte {
	d := NewDecoderWithBytes([]byte(line + "\n"))
	c.Assert(d.Next(), qt.IsTrue)
	key, err := d.SeriesKey()
	c.Assert(err, qt.IsNil)
	return key
}

// equivalentSeries holds pairs of lines that look different on the
// wire but identify the same series: tag order differs, escape
// spellings differ while decoding to the same bytes, or the fields
// and timestamp (which never take part in the key) differ.
var equivalentSeries = [][2]string{
	// Tag order doesn't matter.
	{
		`m,a=1,b=2 f=1`,
		`m,b=2,a=1 f=1`,
	},
	// Same tags, different field values and timestamps.
	{
		`m,a=1,b=2 f=1 1465839830100400200`,
		`m,a=1,b=2 f=99,x="other" 1`,
	},
	// Escaped characters (comma, equals, space, backslash) and
	// multi-byte characters in measurement and tags, reordered.
	{
		`wea\,\ ther,loc\=ation=us\ \ west,path=a\\b,city=北京 temp=82`,
		`wea\,\ ther,city=北京,path=a\\b,loc\=ation=us\ \ west temp=1i 5`,
	},
	// Escape spellings that differ but decode to the same bytes: in
	// string field values a backslash can be written raw or escaped,
	// and a newline can be written literally or as \n. Fields don't
	// take part in the key, so the keys match regardless.
	{
		"m,a=1 f=\"a\\b\" 100",
		"m,a=1 f=\"a\\\\b\" 200",
	},
	// Measurement/tag unescaping is canonical, so an escaped form
	// and its decoded value can never collide; but the same escaped
	// form spelled in different tag positions still matches.
	{
		`m\ x,k\=1=v\,\ w f=1`,
		`m\ x,k\=1=v\,\ w f=2`,
	},
}

func TestSeriesKeyEquivalentLines(t *testing.T) {
	c := qt.New(t)
	for _, pair := range equivalentSeries {
		k1 := seriesKeyForLine(c, pair[0])
		k2 := seriesKeyForLine(c, pair[1])
		c.Assert(k2, qt.DeepEquals, k1, qt.Commentf("%q vs %q", pair[0], pair[1]))
	}
}

// inequivalentSeries holds pairs of lines that identify different
// series even though they look similar.
var inequivalentSeries = [][2]string{
	// Missing tag vs present tag.
	{
		`m,a=1 f=1`,
		`m,a=1,b=2 f=1`,
	},
	// Different tag value.
	{
		`m,a=1 f=1`,
		`m,a=2 f=1`,
	},
	// Separator characters inside values change the series.
	{
		`m,a=x\,\ y f=1`,
		`m,a=x f=1`,
	},
	// Case differs: no case folding.
	{
		`m,host=A f=1`,
		`m,host=a f=1`,
	},
	// Unicode canonical equivalents (composed vs decomposed):
	// no normalization.
	{
		"m,caf\u00e9=1 f=1",
		"m,cafe\u0301=1 f=1",
	},
	// Different measurement, same tags.
	{
		`m,a=1 f=1`,
		`mm,a=1 f=1`,
	},
	// A backslash that isn't part of an escape sequence stays in
	// the decoded value, so it changes the series.
	{
		`m,a=b\c f=1`,
		`m,a=bc f=1`,
	},
}

func TestSeriesKeyInequivalentLines(t *testing.T) {
	c := qt.New(t)
	for _, pair := range inequivalentSeries {
		k1 := seriesKeyForLine(c, pair[0])
		k2 := seriesKeyForLine(c, pair[1])
		c.Assert(string(k1), qt.Not(qt.Equals), string(k2), qt.Commentf("%q vs %q", pair[0], pair[1]))
	}
}

func TestSeriesKeyEmptyVsMissingTag(t *testing.T) {
	c := qt.New(t)
	// The decoder rejects empty tag values syntactically, but a
	// gateway constructing keys from decoded values can see them:
	// an empty value must differ from a missing tag, and both must
	// differ from a value that happens to contain separator bytes.
	withEmpty, err := NewSeriesKey([]byte("m"), []Tag{{Key: []byte("a"), Value: []byte("")}})
	c.Assert(err, qt.IsNil)
	missing, err := NewSeriesKey([]byte("m"), nil)
	c.Assert(err, qt.IsNil)
	c.Assert(string(withEmpty), qt.Not(qt.Equals), string(missing))

	// A value containing separators must not collide with multiple
	// tags or with a shifted measurement/tag boundary.
	sepInValue, err := NewSeriesKey([]byte("m"), []Tag{{Key: []byte("a"), Value: []byte("b,c")}})
	c.Assert(err, qt.IsNil)
	twoTags, err := NewSeriesKey([]byte("m"), []Tag{{Key: []byte("a"), Value: []byte("b")}, {Key: []byte("c"), Value: []byte("")}})
	c.Assert(err, qt.IsNil)
	c.Assert(string(sepInValue), qt.Not(qt.Equals), string(twoTags))

	// Measurement/tag boundary shift: "m" + tag a=b vs "mb" + no
	// tags would collide in a naive concatenation.
	shifted, err := NewSeriesKey([]byte("mb"), nil)
	c.Assert(err, qt.IsNil)
	ab, err := NewSeriesKey([]byte("m"), []Tag{{Key: []byte("a"), Value: []byte("b")}})
	c.Assert(err, qt.IsNil)
	c.Assert(string(ab), qt.Not(qt.Equals), string(shifted))
}

func TestSeriesKeyDuplicateTagsRejected(t *testing.T) {
	c := qt.New(t)
	// Duplicate tag keys are rejected whether or not the values agree.
	for _, line := range []string{
		`m,a=1,a=1 f=1`,
		`m,a=1,b=2,a=2 f=1`,
	} {
		d := NewDecoderWithBytes([]byte(line + "\n"))
		c.Assert(d.Next(), qt.IsTrue)
		key, err := d.SeriesKey()
		c.Assert(key, qt.IsNil)
		c.Assert(err, qt.Not(qt.IsNil))
		c.Assert(errors.Is(err, ErrDuplicateTagKey), qt.IsTrue, qt.Commentf("line %q, err %v", line, err))
	}

	// The same policy applies to keys built from decoded values.
	_, err := NewSeriesKey([]byte("m"), []Tag{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
		{Key: []byte("a"), Value: []byte("1")},
	})
	c.Assert(errors.Is(err, ErrDuplicateTagKey), qt.IsTrue)
}

func TestSeriesKeyOwnedByCaller(t *testing.T) {
	c := qt.New(t)
	d := NewDecoderWithBytes([]byte("m,a=1,b=2 f=1\nm,a=1,b=2 f=2\nother,x=9 f=3\n"))
	c.Assert(d.Next(), qt.IsTrue)
	k1, err := d.SeriesKey()
	c.Assert(err, qt.IsNil)
	k1Copy := append([]byte(nil), k1...)

	// Advance through more entries; the first key must not change.
	c.Assert(d.Next(), qt.IsTrue)
	k2, err := d.SeriesKey()
	c.Assert(err, qt.IsNil)
	c.Assert(d.Next(), qt.IsTrue)
	k3, err := d.SeriesKey()
	c.Assert(err, qt.IsNil)

	c.Assert(k1, qt.DeepEquals, k1Copy)
	c.Assert(k2, qt.DeepEquals, k1)
	c.Assert(string(k3), qt.Not(qt.Equals), string(k1))
}

func TestSeriesKeyFieldsStillReadable(t *testing.T) {
	c := qt.New(t)
	// SeriesKey consumes only the measurement and tag sections.
	d := NewDecoderWithBytes([]byte("m,a=1 f=42i 99\n"))
	c.Assert(d.Next(), qt.IsTrue)
	_, err := d.SeriesKey()
	c.Assert(err, qt.IsNil)
	key, val, err := d.NextField()
	c.Assert(err, qt.IsNil)
	c.Assert(string(key), qt.Equals, "f")
	c.Assert(val.Interface(), qt.Equals, int64(42))
	ts, err := d.Time(Nanosecond, time.Time{})
	c.Assert(err, qt.IsNil)
	c.Assert(ts.UnixNano(), qt.Equals, int64(99))
}

func TestParseSeriesKeyRoundTrip(t *testing.T) {
	c := qt.New(t)
	line := `wea\,\ ther,loc\=ation=us\ \ west,path=a\\b,city=北京 temp=82`
	key := seriesKeyForLine(c, line)
	measurement, tags, err := ParseSeriesKey(key)
	c.Assert(err, qt.IsNil)
	// Bytes are preserved exactly - no case folding or normalization.
	c.Assert(string(measurement), qt.Equals, "wea, ther")
	var got []Tag
	for _, tag := range tags {
		got = append(got, Tag{
			Key:   append([]byte(nil), tag.Key...),
			Value: append([]byte(nil), tag.Value...),
		})
	}
	// Tags come back in canonical (sorted by key bytes) order.
	c.Assert(got, qt.DeepEquals, []Tag{
		{Key: []byte("city"), Value: []byte("北京")},
		{Key: []byte("loc=ation"), Value: []byte("us  west")},
		{Key: []byte("path"), Value: []byte(`a\\b`)},
	})
}

func TestParseSeriesKeyInvalid(t *testing.T) {
	c := qt.New(t)
	valid, err := NewSeriesKey([]byte("m"), []Tag{{Key: []byte("a"), Value: []byte("b")}})
	c.Assert(err, qt.IsNil)
	for _, key := range [][]byte{
		nil,
		{0x80},                                   // bad varint
		{5, 'm'},                                 // truncated measurement
		valid[:len(valid)-1],                     // truncated value
		append(append([]byte(nil), valid...), 0), // trailing bytes
	} {
		_, _, err := ParseSeriesKey(key)
		c.Assert(err, qt.Not(qt.IsNil), qt.Commentf("key %q", key))
	}
}

func TestNewSeriesKeyDoesNotRetainCallerSlices(t *testing.T) {
	c := qt.New(t)
	measurement := []byte("m")
	tags := []Tag{{Key: []byte("b"), Value: []byte("2")}, {Key: []byte("a"), Value: []byte("1")}}
	key, err := NewSeriesKey(measurement, tags)
	c.Assert(err, qt.IsNil)
	// Mutating the caller's slices afterwards must not affect the key,
	// and the caller's tag order must be left alone.
	measurement[0] = 'X'
	tags[0].Key[0] = 'X'
	tags[1].Value[0] = 'X'
	again, err := NewSeriesKey([]byte("m"), []Tag{{Key: []byte("a"), Value: []byte("1")}, {Key: []byte("b"), Value: []byte("2")}})
	c.Assert(err, qt.IsNil)
	c.Assert(key, qt.DeepEquals, again)
	c.Assert(string(tags[0].Key), qt.Equals, "X")
}
