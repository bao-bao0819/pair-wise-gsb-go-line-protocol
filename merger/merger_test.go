package merger_test

import (
	"errors"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/influxdata/line-protocol/v2/lineprotocol"
	"github.com/influxdata/line-protocol/v2/merger"
)

func mkPoint(measurement string, tags []merger.Tag, fields []merger.Field, ts int64, prec lineprotocol.Precision) merger.Point {
	p := merger.Point{
		Measurement: measurement,
		Tags:        tags,
		Fields:      fields,
		Precision:   prec,
	}
	if ts != noTS {
		p.HasTime = true
		p.Time = time.Unix(0, ts*int64(prec.Duration()))
	}
	return p
}

const noTS = int64(1) << 62 // sentinel meaning "no timestamp"; never used as a real value

func f(k string, v interface{}) merger.Field {
	val, ok := lineprotocol.NewValue(v)
	if !ok {
		panic("bad test value")
	}
	return merger.Field{Key: k, Value: val}
}

func tag(k, v string) merger.Tag { return merger.Tag{Key: k, Value: v} }

func fieldMap(p merger.Point) map[string]lineprotocol.Value {
	out := make(map[string]lineprotocol.Value, len(p.Fields))
	for _, fld := range p.Fields {
		out[fld.Key] = fld.Value
	}
	return out
}

func tagList(p merger.Point) []merger.Tag { return p.Tags }

func TestOutOfOrderMerge(t *testing.T) {
	c := qt.New(t)
	m, err := merger.New(merger.Config{
		OnFieldConflict: merger.KeepLast,
		MaxPoints:       10,
	})
	c.Assert(err, qt.IsNil)

	// Same series/instant arrives as three lines, out of order in time
	// relative to other series.
	c.Assert(m.Add(mkPoint("m", []merger.Tag{tag("h", "a")},
		[]merger.Field{f("temp", int64(1))}, 2_000, lineprotocol.Millisecond)), qt.IsNil)
	c.Assert(m.Add(mkPoint("m", []merger.Tag{tag("h", "a")},
		[]merger.Field{f("load", float64(0.5))}, 1_000, lineprotocol.Millisecond)), qt.IsNil)
	c.Assert(m.Add(mkPoint("m", []merger.Tag{tag("h", "a")},
		[]merger.Field{f("temp", int64(1)), f("hum", int64(9))}, 1_000, lineprotocol.Millisecond)), qt.IsNil)

	c.Assert(m.Len(), qt.Equals, 2)
	out := m.Drain(5_000 * int64(time.Millisecond))
	c.Assert(out, qt.HasLen, 2)

	// Stable order by time.
	c.Assert(out[0].Time.UnixNano(), qt.Equals, int64(1_000_000_000))
	c.Assert(fieldMap(out[0]), qt.DeepEquals, map[string]lineprotocol.Value{
		"temp": lineprotocol.MustNewValue(int64(1)),
		"load": lineprotocol.MustNewValue(float64(0.5)),
		"hum":  lineprotocol.MustNewValue(int64(9)),
	})
	c.Assert(out[1].Time.UnixNano(), qt.Equals, int64(2_000_000_000))
}

func TestDuplicateTagOrderingCanonicalizesSeries(t *testing.T) {
	c := qt.New(t)
	m, err := merger.New(merger.Config{
		OnFieldConflict: merger.KeepLast,
		MaxPoints:       10,
	})
	c.Assert(err, qt.IsNil)

	// Same tag set supplied in different order.
	c.Assert(m.Add(mkPoint("m", []merger.Tag{tag("z", "1"), tag("a", "2")},
		[]merger.Field{f("x", int64(1))}, 10, lineprotocol.Second)), qt.IsNil)
	c.Assert(m.Add(mkPoint("m", []merger.Tag{tag("a", "2"), tag("z", "1")},
		[]merger.Field{f("y", int64(2))}, 10, lineprotocol.Second)), qt.IsNil)

	c.Assert(m.Len(), qt.Equals, 1)
	out := m.Flush()
	c.Assert(out, qt.HasLen, 1)
	// Emitted tags are canonicalized (sorted).
	c.Assert(tagList(out[0]), qt.DeepEquals, []merger.Tag{tag("a", "2"), tag("z", "1")})
	c.Assert(fieldMap(out[0]), qt.DeepEquals, map[string]lineprotocol.Value{
		"x": lineprotocol.MustNewValue(int64(1)),
		"y": lineprotocol.MustNewValue(int64(2)),
	})
}

func TestDifferentPrecisionSameInstantMerges(t *testing.T) {
	c := qt.New(t)
	m, err := merger.New(merger.Config{MaxPoints: 10})
	c.Assert(err, qt.IsNil)

	instant := int64(42_000_000_000) // 42s in ns
	c.Assert(m.Add(mkPoint("m", nil,
		[]merger.Field{f("a", int64(1))}, 42, lineprotocol.Second)), qt.IsNil)
	c.Assert(m.Add(mkPoint("m", nil,
		[]merger.Field{f("b", int64(2))}, 42_000, lineprotocol.Millisecond)), qt.IsNil)
	c.Assert(m.Add(mkPoint("m", nil,
		[]merger.Field{f("c", int64(3))}, 42_000_000, lineprotocol.Microsecond)), qt.IsNil)

	c.Assert(m.Len(), qt.Equals, 1)
	out := m.Flush()
	c.Assert(out, qt.HasLen, 1)
	c.Assert(out[0].Time.UnixNano(), qt.Equals, instant)
	c.Assert(fieldMap(out[0]), qt.DeepEquals, map[string]lineprotocol.Value{
		"a": lineprotocol.MustNewValue(int64(1)),
		"b": lineprotocol.MustNewValue(int64(2)),
		"c": lineprotocol.MustNewValue(int64(3)),
	})
}

func TestAdjacentPrecisionInstantsDoNotMerge(t *testing.T) {
	c := qt.New(t)
	m, err := merger.New(merger.Config{MaxPoints: 10})
	c.Assert(err, qt.IsNil)

	c.Assert(m.Add(mkPoint("m", nil,
		[]merger.Field{f("a", int64(1))}, 42_001, lineprotocol.Millisecond)), qt.IsNil)
	c.Assert(m.Add(mkPoint("m", nil,
		[]merger.Field{f("b", int64(2))}, 42, lineprotocol.Second)), qt.IsNil)
	c.Assert(m.Len(), qt.Equals, 2)
}

func TestLatePointAfterWatermarkAndNoRewrite(t *testing.T) {
	c := qt.New(t)
	m, err := merger.New(merger.Config{MaxPoints: 10})
	c.Assert(err, qt.IsNil)

	c.Assert(m.Add(mkPoint("m", nil,
		[]merger.Field{f("a", int64(1))}, 1_000, lineprotocol.Millisecond)), qt.IsNil)

	out := m.Drain(1_000 * int64(time.Millisecond))
	c.Assert(out, qt.HasLen, 1)

	// A point exactly at the watermark is late.
	err = m.Add(mkPoint("m", nil,
		[]merger.Field{f("b", int64(2))}, 1_000, lineprotocol.Millisecond))
	c.Assert(errors.Is(err, merger.ErrLate), qt.IsTrue, qt.Commentf("err=%v", err))

	// A strictly older point is also late.
	err = m.Add(mkPoint("m", nil,
		[]merger.Field{f("b", int64(2))}, 500, lineprotocol.Millisecond))
	c.Assert(errors.Is(err, merger.ErrLate), qt.IsTrue)

	// Emitted result is unchanged and no new state retained.
	c.Assert(m.Len(), qt.Equals, 0)
	c.Assert(out[0].Fields, qt.HasLen, 1)
}

func TestLatePointAfterFlush(t *testing.T) {
	c := qt.New(t)
	m, err := merger.New(merger.Config{MaxPoints: 10})
	c.Assert(err, qt.IsNil)

	c.Assert(m.Add(mkPoint("m", nil,
		[]merger.Field{f("a", int64(1))}, 1_000, lineprotocol.Millisecond)), qt.IsNil)
	c.Assert(m.Flush(), qt.HasLen, 1)

	// Even a nominally "newer" timestamp is late once flushed.
	err = m.Add(mkPoint("m", nil,
		[]merger.Field{f("b", int64(2))}, 9_999_999_999, lineprotocol.Millisecond))
	c.Assert(errors.Is(err, merger.ErrLate), qt.IsTrue)
	c.Assert(m.Len(), qt.Equals, 0)
}

func TestUntimestampedPointsStayIndependent(t *testing.T) {
	c := qt.New(t)
	m, err := merger.New(merger.Config{
		OnFieldConflict: merger.KeepLast,
		MaxPoints:       10,
	})
	c.Assert(err, qt.IsNil)

	p1 := mkPoint("m", nil, []merger.Field{f("a", int64(1))}, noTS, 0)
	p2 := mkPoint("m", nil, []merger.Field{f("a", int64(2)), f("b", int64(3))}, noTS, 0)
	c.Assert(m.Add(p1), qt.IsNil)
	c.Assert(m.Add(p2), qt.IsNil)

	// Untimestamped points never merge, even for the same series.
	c.Assert(m.Len(), qt.Equals, 2)
	out := m.Flush()
	c.Assert(out, qt.HasLen, 2)
	// Emitted in input order.
	c.Assert(fieldMap(out[0]), qt.HasLen, 1)
	c.Assert(fieldMap(out[1]), qt.DeepEquals, map[string]lineprotocol.Value{
		"a": lineprotocol.MustNewValue(int64(2)),
		"b": lineprotocol.MustNewValue(int64(3)),
	})
	// A watermark does not release untimestamped points.
	m2, _ := merger.New(merger.Config{MaxPoints: 10})
	c.Assert(m2.Add(p1), qt.IsNil)
	c.Assert(m2.Drain(maxNS), qt.HasLen, 0)
	c.Assert(m2.Len(), qt.Equals, 1)
}

func TestUntimestampedPointAfterFlushIsRetained(t *testing.T) {
	c := qt.New(t)
	m, err := merger.New(merger.Config{MaxPoints: 10})
	c.Assert(err, qt.IsNil)

	c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(1))}, 1, lineprotocol.Second)), qt.IsNil)
	c.Assert(m.Flush(), qt.HasLen, 1)

	// Untimestamped points have no instant, so they are never late and are
	// retained independently even after a flush.
	c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("b", int64(2))}, noTS, 0)), qt.IsNil)
	c.Assert(m.Len(), qt.Equals, 1)
	out := m.Flush()
	c.Assert(out, qt.HasLen, 1)
	c.Assert(out[0].HasTime, qt.IsFalse)
}

const maxNS = int64(^uint64(0) >> 1)

func TestBackpressureDoesNotEvict(t *testing.T) {
	c := qt.New(t)
	m, err := merger.New(merger.Config{MaxPoints: 2})
	c.Assert(err, qt.IsNil)

	c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(1))}, 1, lineprotocol.Second)), qt.IsNil)
	c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(1))}, 2, lineprotocol.Second)), qt.IsNil)

	// A third distinct instant exceeds the bound.
	err = m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(1))}, 3, lineprotocol.Second))
	c.Assert(errors.Is(err, merger.ErrBackpressure), qt.IsTrue)
	// Nothing was evicted and the rejected point was not retained.
	c.Assert(m.Len(), qt.Equals, 2)

	// A point that merges into an existing group needs no new slot.
	c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("b", int64(2))}, 2, lineprotocol.Second)), qt.IsNil)
	c.Assert(m.Len(), qt.Equals, 2)
}

func TestConflictPolicies(t *testing.T) {
	t.Run("first", func(t *testing.T) {
		c := qt.New(t)
		m, _ := merger.New(merger.Config{OnFieldConflict: merger.KeepFirst, MaxPoints: 10})
		c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(1))}, 1, lineprotocol.Second)), qt.IsNil)
		c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(2))}, 1, lineprotocol.Second)), qt.IsNil)
		out := m.Flush()
		c.Assert(fieldMap(out[0])["a"].IntV(), qt.Equals, int64(1))
	})
	t.Run("last", func(t *testing.T) {
		c := qt.New(t)
		m, _ := merger.New(merger.Config{OnFieldConflict: merger.KeepLast, MaxPoints: 10})
		c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(1))}, 1, lineprotocol.Second)), qt.IsNil)
		c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(2))}, 1, lineprotocol.Second)), qt.IsNil)
		out := m.Flush()
		c.Assert(fieldMap(out[0])["a"].IntV(), qt.Equals, int64(2))
	})
	t.Run("error", func(t *testing.T) {
		c := qt.New(t)
		m, _ := merger.New(merger.Config{OnFieldConflict: merger.RejectConflict, MaxPoints: 10})
		c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(1))}, 1, lineprotocol.Second)), qt.IsNil)
		err := m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(2))}, 1, lineprotocol.Second))
		c.Assert(errors.Is(err, merger.ErrFieldConflict), qt.IsTrue)
		// Rejected point leaves state unchanged.
		out := m.Flush()
		c.Assert(fieldMap(out[0])["a"].IntV(), qt.Equals, int64(1))
	})
	// Equal values never conflict even under RejectConflict.
	t.Run("equal-allowed", func(t *testing.T) {
		c := qt.New(t)
		m, _ := merger.New(merger.Config{OnFieldConflict: merger.RejectConflict, MaxPoints: 10})
		c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(7))}, 1, lineprotocol.Second)), qt.IsNil)
		c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(7))}, 1, lineprotocol.Second)), qt.IsNil)
		c.Assert(m.Flush(), qt.HasLen, 1)
	})
}

func TestTypeChangePolicies(t *testing.T) {
	t.Run("reject", func(t *testing.T) {
		c := qt.New(t)
		m, _ := merger.New(merger.Config{
			OnFieldConflict: merger.KeepLast,
			OnTypeChange:    merger.RejectTypeChange,
			MaxPoints:       10,
		})
		c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(1))}, 1, lineprotocol.Second)), qt.IsNil)
		err := m.Add(mkPoint("m", nil, []merger.Field{f("a", float64(1))}, 1, lineprotocol.Second))
		c.Assert(errors.Is(err, merger.ErrTypeChange), qt.IsTrue)
		out := m.Flush()
		c.Assert(fieldMap(out[0])["a"].Kind(), qt.Equals, lineprotocol.Int)
	})
	t.Run("allow-replaces", func(t *testing.T) {
		c := qt.New(t)
		m, _ := merger.New(merger.Config{
			OnFieldConflict: merger.KeepLast,
			OnTypeChange:    merger.AllowTypeChange,
			MaxPoints:       10,
		})
		c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", int64(1))}, 1, lineprotocol.Second)), qt.IsNil)
		c.Assert(m.Add(mkPoint("m", nil, []merger.Field{f("a", float64(2.5))}, 1, lineprotocol.Second)), qt.IsNil)
		out := m.Flush()
		got := fieldMap(out[0])["a"]
		c.Assert(got.Kind(), qt.Equals, lineprotocol.Float)
		c.Assert(got.FloatV(), qt.Equals, 2.5)
	})
}

func TestOrderIndependenceForDisjointFields(t *testing.T) {
	c := qt.New(t)
	fieldsA := []merger.Field{f("a", int64(1)), f("b", int64(2))}
	fieldsB := []merger.Field{f("c", int64(3)), f("d", int64(4))}

	run := func(first, second []merger.Field) []merger.Field {
		m, err := merger.New(merger.Config{OnFieldConflict: merger.KeepLast, MaxPoints: 10})
		c.Assert(err, qt.IsNil)
		c.Assert(m.Add(mkPoint("m", []merger.Tag{tag("k", "v")}, first, 5, lineprotocol.Second)), qt.IsNil)
		c.Assert(m.Add(mkPoint("m", []merger.Tag{tag("k", "v")}, second, 5, lineprotocol.Second)), qt.IsNil)
		out := m.Flush()
		c.Assert(out, qt.HasLen, 1)
		return out[0].Fields
	}

	forward := run(fieldsA, fieldsB)
	reverse := run(fieldsB, fieldsA)
	// Fields are sorted on output, so content and order are identical.
	c.Assert(forward, qt.DeepEquals, reverse)
	c.Assert(forward, qt.HasLen, 4)
}

func TestStableOutputSortedBySeriesAndTime(t *testing.T) {
	c := qt.New(t)
	m, _ := merger.New(merger.Config{MaxPoints: 10})
	c.Assert(m.Add(mkPoint("m", []merger.Tag{tag("id", "b")},
		[]merger.Field{f("v", int64(1))}, 2, lineprotocol.Second)), qt.IsNil)
	c.Assert(m.Add(mkPoint("m", []merger.Tag{tag("id", "a")},
		[]merger.Field{f("v", int64(1))}, 2, lineprotocol.Second)), qt.IsNil)
	c.Assert(m.Add(mkPoint("m", []merger.Tag{tag("id", "a")},
		[]merger.Field{f("v", int64(1))}, 1, lineprotocol.Second)), qt.IsNil)

	out := m.Flush()
	c.Assert(out, qt.HasLen, 3)
	c.Assert(out[0].Time.UnixNano(), qt.Equals, int64(time.Second))
	c.Assert(out[0].Tags[0].Value, qt.Equals, "a")
	c.Assert(out[1].Time.UnixNano(), qt.Equals, int64(2*time.Second))
	c.Assert(out[1].Tags[0].Value, qt.Equals, "a")
	c.Assert(out[2].Time.UnixNano(), qt.Equals, int64(2*time.Second))
	c.Assert(out[2].Tags[0].Value, qt.Equals, "b")
}
