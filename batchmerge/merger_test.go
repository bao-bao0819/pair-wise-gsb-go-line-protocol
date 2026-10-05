package batchmerge_test

import (
	"errors"
	"testing"
	"time"

	"github.com/influxdata/line-protocol/v2/batchmerge"
	"github.com/influxdata/line-protocol/v2/lineprotocol"
)

func tag(k, v string) batchmerge.Tag {
	return batchmerge.Tag{Key: []byte(k), Value: []byte(v)}
}

func field(k string, v interface{}) batchmerge.Field {
	return batchmerge.Field{Key: []byte(k), Value: lineprotocol.MustNewValue(v)}
}

func timedPoint(measurement string, tags []batchmerge.Tag, fields []batchmerge.Field, raw int64, prec lineprotocol.Precision) batchmerge.Point {
	return batchmerge.Point{
		Measurement: []byte(measurement),
		Tags:        tags,
		Fields:      fields,
		HasTime:     true,
		RawTime:     raw,
		Precision:   prec,
	}
}

func untimedPoint(measurement string, tags []batchmerge.Tag, fields []batchmerge.Field) batchmerge.Point {
	return batchmerge.Point{
		Measurement: []byte(measurement),
		Tags:        tags,
		Fields:      fields,
	}
}

func mustAdd(t *testing.T, m *batchmerge.Merger, p batchmerge.Point) {
	t.Helper()
	if err := m.Add(p); err != nil {
		t.Fatalf("unexpected Add error: %v", err)
	}
}

// render emits points as canonical line protocol, one point per line, so
// merged results can be compared as plain strings.
func render(t *testing.T, points []batchmerge.MergedPoint) string {
	t.Helper()
	var enc lineprotocol.Encoder
	var buf []byte
	for _, p := range points {
		enc.SetBuffer(buf)
		enc.StartLineRaw(p.Measurement)
		for _, tg := range p.Tags {
			enc.AddTagRaw(tg.Key, tg.Value)
		}
		for _, f := range p.Fields {
			enc.AddFieldRaw(f.Key, f.Value)
		}
		if p.HasTime {
			enc.EndLine(p.Time)
		} else {
			enc.EndLine(time.Time{})
		}
		if err := enc.Err(); err != nil {
			t.Fatalf("encoding error: %v", err)
		}
		buf = enc.Bytes()
	}
	return string(buf)
}

// Out-of-order arrival of split rows merges into one point.
func TestOutOfOrderMerges(t *testing.T) {
	m := batchmerge.New(batchmerge.Config{})
	mustAdd(t, m, timedPoint("cpu", []batchmerge.Tag{tag("host", "a")},
		[]batchmerge.Field{field("b", int64(2))}, 1000, lineprotocol.Millisecond))
	mustAdd(t, m, timedPoint("cpu", []batchmerge.Tag{tag("host", "a")},
		[]batchmerge.Field{field("a", int64(1))}, 1, lineprotocol.Second))

	out := m.Flush()
	if got, want := m.Retained(), 0; got != want {
		t.Fatalf("retained after flush = %d, want %d", got, want)
	}
	if got, want := render(t, out), "cpu,host=a a=1i,b=2i 1000000000\n"; got != want {
		t.Fatalf("merged output:\n%q\nwant:\n%q", got, want)
	}
}

// Duplicate and unsorted tags canonicalize to the same series identity.
func TestDuplicateAndUnsortedTags(t *testing.T) {
	m := batchmerge.New(batchmerge.Config{})
	mustAdd(t, m, timedPoint("cpu",
		[]batchmerge.Tag{tag("z", "1"), tag("a", "x"), tag("a", "y")},
		[]batchmerge.Field{field("a", int64(1))}, 5, lineprotocol.Second))
	mustAdd(t, m, timedPoint("cpu",
		[]batchmerge.Tag{tag("a", "y"), tag("z", "1")},
		[]batchmerge.Field{field("b", true)}, 5, lineprotocol.Second))

	out := m.Flush()
	if got, want := render(t, out), "cpu,a=y,z=1 a=1i,b=true 5000000000\n"; got != want {
		t.Fatalf("merged output:\n%q\nwant:\n%q", got, want)
	}
}

// Different input precisions denoting the same instant share a merge key.
func TestPrecisionsSameInstant(t *testing.T) {
	cases := []struct {
		name string
		raw  int64
		prec lineprotocol.Precision
	}{
		{"ns", 2000000000, lineprotocol.Nanosecond},
		{"us", 2000000, lineprotocol.Microsecond},
		{"ms", 2000, lineprotocol.Millisecond},
		{"s", 2, lineprotocol.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := batchmerge.New(batchmerge.Config{})
			mustAdd(t, m, timedPoint("m", nil,
				[]batchmerge.Field{field("a", int64(1))}, c.raw, c.prec))
			mustAdd(t, m, timedPoint("m", nil,
				[]batchmerge.Field{field("b", int64(2))}, 2000000000, lineprotocol.Nanosecond))
			out := m.Flush()
			if got, want := render(t, out), "m a=1i,b=2i 2000000000\n"; got != want {
				t.Fatalf("merged output:\n%q\nwant:\n%q", got, want)
			}
		})
	}
}

// After flush, an older timed point is late and changes nothing.
func TestLateAfterFlush(t *testing.T) {
	m := batchmerge.New(batchmerge.Config{})
	mustAdd(t, m, timedPoint("m", nil, []batchmerge.Field{field("a", int64(1))}, 10, lineprotocol.Second))
	first := m.Flush()
	if got, want := len(first), 1; got != want {
		t.Fatalf("flush points = %d, want %d", got, want)
	}

	err := m.Add(timedPoint("m", nil, []batchmerge.Field{field("a", int64(9))}, 5, lineprotocol.Second))
	if !errors.Is(err, batchmerge.ErrLate) {
		t.Fatalf("expected ErrLate, got %v", err)
	}
	var late *batchmerge.LateError
	if !errors.As(err, &late) {
		t.Fatalf("expected *LateError, got %T", err)
	}

	// A future point starts a new batch; earlier output stays untouched.
	mustAdd(t, m, timedPoint("m", nil, []batchmerge.Field{field("a", int64(3))}, 20, lineprotocol.Second))
	second := m.Flush()
	if got, want := render(t, second), "m a=3i 20000000000\n"; got != want {
		t.Fatalf("second output:\n%q\nwant:\n%q", got, want)
	}
	if got, want := render(t, first), "m a=1i 10000000000\n"; got != want {
		t.Fatalf("first output was rewritten:\n%q\nwant:\n%q", got, want)
	}
}

// Points without timestamps never merge, even when identical.
func TestUntimestampedStayIndependent(t *testing.T) {
	m := batchmerge.New(batchmerge.Config{})
	mustAdd(t, m, untimedPoint("m", []batchmerge.Tag{tag("h", "x")},
		[]batchmerge.Field{field("a", int64(1))}))
	mustAdd(t, m, untimedPoint("m", []batchmerge.Tag{tag("h", "x")},
		[]batchmerge.Field{field("b", int64(2))}))
	out := m.Flush()
	if got, want := len(out), 2; got != want {
		t.Fatalf("untimestamped points = %d, want %d", got, want)
	}
	if got, want := render(t, out), "m,h=x a=1i\nm,h=x b=2i\n"; got != want {
		t.Fatalf("output:\n%q\nwant:\n%q", got, want)
	}
}

// When fields don't overlap, reordering inputs yields the same merged content.
func TestOrderIndependenceNoOverlap(t *testing.T) {
	build := func(order []int) string {
		m := batchmerge.New(batchmerge.Config{})
		points := []batchmerge.Point{
			timedPoint("m", []batchmerge.Tag{tag("h", "1")},
				[]batchmerge.Field{field("a", int64(1))}, 7, lineprotocol.Second),
			timedPoint("m", []batchmerge.Tag{tag("h", "1")},
				[]batchmerge.Field{field("b", int64(2))}, 7, lineprotocol.Second),
			timedPoint("m", []batchmerge.Tag{tag("h", "1")},
				[]batchmerge.Field{field("c", int64(3))}, 7000, lineprotocol.Millisecond),
		}
		for _, i := range order {
			mustAdd(t, m, points[i])
		}
		return render(t, m.Flush())
	}
	want := build([]int{0, 1, 2})
	for _, order := range [][]int{{2, 1, 0}, {1, 0, 2}, {2, 0, 1}} {
		if got := build(order); got != want {
			t.Fatalf("order %v:\n%q\nwant:\n%q", order, got, want)
		}
	}
}

func TestConflictPolicies(t *testing.T) {
	inputs := func() []batchmerge.Point {
		return []batchmerge.Point{
			timedPoint("m", nil, []batchmerge.Field{field("f", int64(1))}, 1, lineprotocol.Second),
			timedPoint("m", nil, []batchmerge.Field{field("f", int64(2))}, 1, lineprotocol.Second),
			timedPoint("m", nil, []batchmerge.Field{field("f", int64(3))}, 1000, lineprotocol.Millisecond),
		}
	}

	t.Run("first keeps earliest input", func(t *testing.T) {
		m := batchmerge.New(batchmerge.Config{FieldConflict: batchmerge.FirstField})
		for _, p := range inputs() {
			mustAdd(t, m, p)
		}
		if got, want := render(t, m.Flush()), "m f=1i 1000000000\n"; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("last takes latest input", func(t *testing.T) {
		m := batchmerge.New(batchmerge.Config{FieldConflict: batchmerge.LastField})
		for _, p := range inputs() {
			mustAdd(t, m, p)
		}
		if got, want := render(t, m.Flush()), "m f=3i 1000000000\n"; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("last follows input sequence not map order", func(t *testing.T) {
		m := batchmerge.New(batchmerge.Config{FieldConflict: batchmerge.LastField})
		pts := inputs()
		mustAdd(t, m, pts[2])
		mustAdd(t, m, pts[0])
		if got, want := render(t, m.Flush()), "m f=1i 1000000000\n"; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("error rejects differing values but accepts equal", func(t *testing.T) {
		m := batchmerge.New(batchmerge.Config{FieldConflict: batchmerge.ErrorField})
		pts := inputs()
		mustAdd(t, m, pts[0])
		mustAdd(t, m, timedPoint("m", nil, []batchmerge.Field{field("f", int64(1))},
			1000, lineprotocol.Millisecond))
		err := m.Add(pts[1])
		var ce *batchmerge.FieldConflictError
		if !errors.As(err, &ce) || ce.Field != "f" || ce.First.IntV() != 1 || ce.Incoming.IntV() != 2 {
			t.Fatalf("expected FieldConflictError (1 vs 2), got %v", err)
		}
		if got, want := m.Retained(), 1; got != want {
			t.Fatalf("retained after reject = %d, want %d", got, want)
		}
		if got, want := render(t, m.Flush()), "m f=1i 1000000000\n"; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
}

func TestTypeChangePolicies(t *testing.T) {
	first := timedPoint("m", nil, []batchmerge.Field{field("f", int64(1))}, 1, lineprotocol.Second)
	second := timedPoint("m", nil, []batchmerge.Field{field("f", 2.5)}, 1, lineprotocol.Second)

	t.Run("reject", func(t *testing.T) {
		m := batchmerge.New(batchmerge.Config{TypeChange: batchmerge.RejectType})
		mustAdd(t, m, first)
		err := m.Add(second)
		var te *batchmerge.TypeConflictError
		if !errors.As(err, &te) || te.Existing != lineprotocol.Int || te.Incoming != lineprotocol.Float {
			t.Fatalf("expected TypeConflictError int vs float, got %v", err)
		}
		if got, want := render(t, m.Flush()), "m f=1i 1000000000\n"; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("allow replaces under last", func(t *testing.T) {
		m := batchmerge.New(batchmerge.Config{
			FieldConflict: batchmerge.LastField,
			TypeChange:    batchmerge.AllowType,
		})
		mustAdd(t, m, first)
		mustAdd(t, m, second)
		if got, want := render(t, m.Flush()), "m f=2.5 1000000000\n"; got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
}

// Full capacity produces back-pressure; nothing is silently evicted, and
// draining via the watermark frees room.
func TestCapacityBackpressure(t *testing.T) {
	m := batchmerge.New(batchmerge.Config{MaxPoints: 2})
	mustAdd(t, m, timedPoint("m", nil, []batchmerge.Field{field("a", int64(1))}, 1, lineprotocol.Second))
	mustAdd(t, m, untimedPoint("m", nil, []batchmerge.Field{field("a", int64(1))}))
	if got := m.Retained(); got != 2 {
		t.Fatalf("retained = %d, want 2", got)
	}

	// A point merging into an existing group needs no extra slot.
	mustAdd(t, m, timedPoint("m", nil, []batchmerge.Field{field("b", int64(2))}, 1, lineprotocol.Second))

	err := m.Add(timedPoint("n", nil, []batchmerge.Field{field("a", int64(1))}, 2, lineprotocol.Second))
	if !errors.Is(err, batchmerge.ErrCapacity) {
		t.Fatalf("expected ErrCapacity, got %v", err)
	}
	if got := m.Retained(); got != 2 {
		t.Fatalf("retained after capacity error = %d, want 2", got)
	}

	out := m.AdvanceWatermark(time.Unix(1, 0).UTC())
	if got, want := len(out), 1; got != want {
		t.Fatalf("watermark points = %d, want %d", got, want)
	}
	mustAdd(t, m, timedPoint("n", nil, []batchmerge.Field{field("a", int64(1))}, 2, lineprotocol.Second))
}

// The watermark releases only timed points at or before it, emits them in
// stable time/series order, and flags later arrivals as late.
func TestWatermarkReleaseAndOrder(t *testing.T) {
	m := batchmerge.New(batchmerge.Config{})
	mustAdd(t, m, timedPoint("cpu", []batchmerge.Tag{tag("host", "b")},
		[]batchmerge.Field{field("v", int64(3))}, 3, lineprotocol.Second))
	mustAdd(t, m, timedPoint("cpu", []batchmerge.Tag{tag("host", "a")},
		[]batchmerge.Field{field("v", int64(1))}, 1, lineprotocol.Second))
	mustAdd(t, m, timedPoint("cpu", []batchmerge.Tag{tag("host", "a")},
		[]batchmerge.Field{field("w", int64(2))}, 2, lineprotocol.Second))
	mustAdd(t, m, timedPoint("mem", nil,
		[]batchmerge.Field{field("v", int64(1))}, 1, lineprotocol.Second))
	mustAdd(t, m, untimedPoint("later", nil, []batchmerge.Field{field("x", int64(0))}))

	out := m.AdvanceWatermark(time.Unix(2, 0).UTC())
	want := "cpu,host=a v=1i 1000000000\n" +
		"mem v=1i 1000000000\n" +
		"cpu,host=a w=2i 2000000000\n"
	if got := render(t, out); got != want {
		t.Fatalf("watermark output:\n%q\nwant:\n%q", got, want)
	}
	if got := m.Retained(); got != 2 {
		t.Fatalf("retained = %d, want 2", got)
	}

	err := m.Add(timedPoint("cpu", []batchmerge.Tag{tag("host", "a")},
		[]batchmerge.Field{field("z", int64(9))}, 2, lineprotocol.Second))
	if !errors.Is(err, batchmerge.ErrLate) {
		t.Fatalf("expected late point error, got %v", err)
	}

	rest := m.Flush()
	if got, want := render(t, rest), "cpu,host=b v=3i 3000000000\nlater x=0i\n"; got != want {
		t.Fatalf("remaining output:\n%q\nwant:\n%q", got, want)
	}
}

// Equal time but a different series is not merged.
func TestDifferentSeriesNotMerged(t *testing.T) {
	m := batchmerge.New(batchmerge.Config{})
	mustAdd(t, m, timedPoint("cpu", []batchmerge.Tag{tag("host", "a")},
		[]batchmerge.Field{field("v", int64(1))}, 1, lineprotocol.Second))
	mustAdd(t, m, timedPoint("cpu", []batchmerge.Tag{tag("host", "b")},
		[]batchmerge.Field{field("w", int64(2))}, 1, lineprotocol.Second))
	mustAdd(t, m, timedPoint("mem", nil,
		[]batchmerge.Field{field("v", int64(3))}, 1, lineprotocol.Second))
	out := m.Flush()
	if got, want := len(out), 3; got != want {
		t.Fatalf("points = %d, want %d", got, want)
	}
	want := "cpu,host=a v=1i 1000000000\n" +
		"cpu,host=b w=2i 1000000000\n" +
		"mem v=3i 1000000000\n"
	if got := render(t, out); got != want {
		t.Fatalf("output:\n%q\nwant:\n%q", got, want)
	}
}

func TestInvalidPoints(t *testing.T) {
	m := batchmerge.New(batchmerge.Config{})
	cases := []batchmerge.Point{
		{Fields: []batchmerge.Field{field("a", int64(1))}, HasTime: true, RawTime: 1, Precision: lineprotocol.Second},
		{Measurement: []byte("m"), HasTime: true, RawTime: 1, Precision: lineprotocol.Second},
		{Measurement: []byte("m"), Tags: []batchmerge.Tag{{Value: []byte("v")}},
			Fields:    []batchmerge.Field{field("a", int64(1))},
			HasTime:   true,
			RawTime:   1,
			Precision: lineprotocol.Second},
	}
	for i, p := range cases {
		if err := m.Add(p); !errors.Is(err, batchmerge.ErrInvalidPoint) {
			t.Fatalf("case %d: expected ErrInvalidPoint, got %v", i, err)
		}
	}
	err := m.Add(timedPoint("m", nil, []batchmerge.Field{field("a", int64(1))},
		1<<62, lineprotocol.Second))
	if !errors.Is(err, batchmerge.ErrInvalidPoint) {
		t.Fatalf("expected overflow ErrInvalidPoint, got %v", err)
	}
}

// Split rows sharing some fields merge them, adding the new field.
func TestPartialFieldUnion(t *testing.T) {
	m := batchmerge.New(batchmerge.Config{})
	mustAdd(t, m, timedPoint("m", nil,
		[]batchmerge.Field{field("a", int64(1)), field("shared", int64(7))}, 1, lineprotocol.Second))
	mustAdd(t, m, timedPoint("m", nil,
		[]batchmerge.Field{field("shared", int64(7)), field("b", "x")}, 1000, lineprotocol.Millisecond))
	want := `m a=1i,b="x",shared=7i 1000000000` + "\n"
	if got := render(t, m.Flush()); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Reset empties retained state and clears the watermark.
func TestReset(t *testing.T) {
	m := batchmerge.New(batchmerge.Config{})
	mustAdd(t, m, timedPoint("m", nil, []batchmerge.Field{field("a", int64(1))}, 1, lineprotocol.Second))
	m.AdvanceWatermark(time.Unix(1, 0).UTC())
	m.Reset()
	if got := m.Retained(); got != 0 {
		t.Fatalf("retained after reset = %d, want 0", got)
	}
	mustAdd(t, m, timedPoint("m", nil, []batchmerge.Field{field("a", int64(1))}, 1, lineprotocol.Second))
}
