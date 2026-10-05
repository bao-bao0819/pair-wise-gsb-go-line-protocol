// Package merger provides a bounded merger for offline collection
// batches.
//
// When points are collected offline, a single logical point (one series
// at one instant) can arrive split over several input lines, each line
// filling in a different subset of fields. Merger combines those lines
// into a single point.
//
// The merge key is the canonical series identity (measurement and the
// tag set sorted by key) together with the exact instant the timestamp
// represents after it has been converted from its input precision to
// nanoseconds. Timestamps expressed at different precisions merge when
// they denote the same instant.
//
// Points without a timestamp can never be merged with one another, as
// there is no instant to correlate them on; each such point is retained
// as an independent input and emitted unchanged.
//
// The number of retained points is bounded by Config.MaxPoints. When a
// new point would exceed that bound, Add returns ErrBackpressure; it
// never silently evicts retained state. Callers free state by advancing
// the watermark (Drain) or by calling Flush.
package merger

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/influxdata/line-protocol/v2/lineprotocol"
)

// Policy selects how a field that is already present in a merged point
// is handled when another input supplies the same field name.
type Policy int

const (
	// KeepFirst keeps the value supplied by the earliest input (in input
	// order); later values are ignored.
	KeepFirst Policy = iota
	// KeepLast replaces the existing value with the value supplied by the
	// latest input (in input order).
	KeepLast
	// RejectConflict makes Add return ErrFieldConflict when the same
	// field is supplied more than once with differing values of the same
	// type.
	RejectConflict
)

// TypePolicy selects how a field whose type changes between inputs is
// handled. It is evaluated independently of Policy.
type TypePolicy int

const (
	// RejectTypeChange makes Add return ErrTypeChange when a field is
	// supplied with a different ValueKind than the value already held.
	RejectTypeChange TypePolicy = iota
	// AllowTypeChange permits the new value to replace the existing one
	// even when its ValueKind differs. Policy still decides which value
	// wins.
	AllowTypeChange
)

var (
	// ErrFieldConflict is wrapped when the same field name is supplied by
	// more than one input with differing values and Policy is
	// RejectConflict.
	ErrFieldConflict = errors.New("conflicting field value")
	// ErrTypeChange is wrapped when a field changes type between inputs
	// and TypePolicy is RejectTypeChange.
	ErrTypeChange = errors.New("field changed type")
	// ErrBackpressure is returned when accepting a new point would exceed
	// the configured MaxPoints bound. Retained state is never evicted.
	ErrBackpressure = errors.New("retained point limit reached")
	// ErrLate is returned by Add when a timestamped point arrives at or
	// before the current watermark, or after Flush. Such a point is
	// dropped without merging and never alters already emitted output.
	ErrLate = errors.New("late point")
	// ErrOverflow is returned when a timestamp cannot be converted to
	// nanoseconds without overflowing.
	ErrOverflow = errors.New("timestamp out of range")
	// ErrInvalid is wrapped for malformed configuration or input.
	ErrInvalid = errors.New("invalid input")
)

// Tag is one tag pair. Keys and values are expected to be already
// unescaped, as returned by the line-protocol decoder.
type Tag struct {
	Key   string
	Value string
}

// Field is one field pair.
type Field struct {
	Key   string
	Value lineprotocol.Value
}

// Point is one input (or emitted) point.
type Point struct {
	// Measurement is the measurement name.
	Measurement string
	// Tags holds the tag set. Order on input is irrelevant to series
	// identity; emitted points carry tags sorted by key.
	Tags []Tag
	// Fields holds the field set. Emitted fields are sorted by key.
	Fields []Field
	// Time is the timestamp, interpreted at Precision.
	Time time.Time
	// HasTime reports whether Time is set. Points without a timestamp are
	// never merged together.
	HasTime bool
	// Precision is the precision at which Time was provided. It is used to
	// derive the exact instant the timestamp denotes.
	Precision lineprotocol.Precision
}

// Config configures a Merger.
type Config struct {
	// OnFieldConflict selects behavior when the same field name is
	// supplied by more than one input with a differing value of the same
	// type.
	OnFieldConflict Policy
	// OnTypeChange selects behavior when a field changes ValueKind
	// between inputs.
	OnTypeChange TypePolicy
	// MaxPoints bounds the number of retained points. It must be positive.
	MaxPoints int
}

// fieldState is a merged field value plus the input sequence number of
// the input that currently supplies it.
type fieldState struct {
	value lineprotocol.Value
	order int64
}

// group is the accumulated state for one merge key.
type group struct {
	measurement string
	tags        []Tag
	// names lists field keys in first-seen encounter order.
	names []string
	// fields holds merged field state keyed by field name.
	fields    map[string]fieldState
	ns        int64
	hasTime   bool
	seriesKey string
	// firstOrder is the input sequence that created the group; it is a
	// stable tie breaker when times and series identities are equal.
	firstOrder int64
}

// Merger merges offline collection points. The zero value is not usable;
// create one with New.
type Merger struct {
	cfg       Config
	watermark int64
	hasWm     bool
	flushed   bool
	seq       int64
	groups    map[string]*group
}

// New returns a Merger using the given configuration.
func New(cfg Config) (*Merger, error) {
	if cfg.MaxPoints <= 0 {
		return nil, fmt.Errorf("merger: MaxPoints must be positive: %w", ErrInvalid)
	}
	return &Merger{
		cfg:    cfg,
		groups: make(map[string]*group),
	}, nil
}

// Len returns the number of points currently retained.
func (m *Merger) Len() int {
	return len(m.groups)
}

// Watermark returns the current watermark in nanoseconds and whether it
// has ever been advanced. After Flush the watermark is the maximum
// representable instant and ok is true.
func (m *Merger) Watermark() (ns int64, ok bool) {
	return m.watermark, m.hasWm
}

// Add incorporates one point in input order. Override precedence follows
// the sequence of Add calls, never map iteration order.
//
// A timestamped point whose instant is at or before the current
// watermark, or that arrives after Flush, is late: Add returns an error
// wrapping ErrLate and the point is neither merged nor retained. Already
// emitted output is never rewritten.
//
// Points without a timestamp are always retained independently and are
// never merged with anything.
//
// On any error the retained state is left unchanged.
func (m *Merger) Add(p Point) error {
	if p.Measurement == "" {
		return fmt.Errorf("merger: empty measurement: %w", ErrInvalid)
	}
	if len(p.Fields) == 0 {
		return fmt.Errorf("merger: point %q has no fields: %w", p.Measurement, ErrInvalid)
	}
	tags, err := canonicalTags(p.Tags)
	if err != nil {
		return err
	}

	var ns int64
	if p.HasTime {
		ns, err = toNanoseconds(p.Time, p.Precision)
		if err != nil {
			return err
		}
		if m.flushed || (m.hasWm && ns <= m.watermark) {
			return fmt.Errorf("merger: point %q at %d: %w", p.Measurement, ns, ErrLate)
		}
	}

	m.seq++
	order := m.seq
	seriesKey := seriesIdentity(p.Measurement, tags)
	key := groupKey(seriesKey, ns, p.HasTime, order)

	g, exists := m.groups[key]
	if !exists {
		if len(m.groups) >= m.cfg.MaxPoints {
			return fmt.Errorf("merger: %w (limit %d)", ErrBackpressure, m.cfg.MaxPoints)
		}
		g = &group{
			measurement: p.Measurement,
			tags:        cloneTags(tags),
			fields:      make(map[string]fieldState),
			ns:          ns,
			hasTime:     p.HasTime,
			seriesKey:   seriesKey,
			firstOrder:  order,
		}
	}

	// Merge onto a working copy so a conflict/type error leaves retained
	// state untouched. The candidate group is only committed on success.
	work := *g
	work.fields = make(map[string]fieldState, len(g.fields)+len(p.Fields))
	for k, v := range g.fields {
		work.fields[k] = v
	}
	names := append([]string(nil), g.names...)

	for _, f := range p.Fields {
		if f.Key == "" {
			return fmt.Errorf("merger: empty field key in %q: %w", p.Measurement, ErrInvalid)
		}
		cur, present := work.fields[f.Key]
		if !present {
			work.fields[f.Key] = fieldState{value: f.Value, order: order}
			names = append(names, f.Key)
			continue
		}

		if cur.value.Kind() != f.Value.Kind() {
			if m.cfg.OnTypeChange == RejectTypeChange {
				return fmt.Errorf("merger: field %q changed from %s to %s: %w",
					f.Key, cur.value.Kind(), f.Value.Kind(), ErrTypeChange)
			}
		} else if cur.value.Equal(f.Value) {
			continue
		}

		switch m.cfg.OnFieldConflict {
		case KeepFirst:
			if order < cur.order {
				work.fields[f.Key] = fieldState{value: f.Value, order: order}
			}
		case KeepLast:
			if order >= cur.order {
				work.fields[f.Key] = fieldState{value: f.Value, order: order}
			}
		case RejectConflict:
			return fmt.Errorf("merger: field %q supplied more than once: %w", f.Key, ErrFieldConflict)
		default:
			return fmt.Errorf("merger: unknown field conflict policy: %w", ErrInvalid)
		}
	}

	work.names = names
	m.groups[key] = &work
	return nil
}

// AdvanceWatermark moves the watermark to the given instant in
// nanoseconds. The watermark never moves backwards. It does not emit
// anything; use Drain to obtain released points.
func (m *Merger) AdvanceWatermark(ns int64) {
	if !m.hasWm || ns > m.watermark {
		m.watermark = ns
		m.hasWm = true
	}
}

// Drain advances the watermark to ns, then emits and removes every
// timestamped retained point whose instant is at or before it. Released
// points are returned in a stable order: by time, then by canonical
// series key.
//
// Points without a timestamp are never released by a watermark; call
// Flush to emit them.
func (m *Merger) Drain(ns int64) []Point {
	m.AdvanceWatermark(ns)

	var ready []*group
	for _, g := range m.groups {
		if g.hasTime && g.ns <= m.watermark {
			ready = append(ready, g)
		}
	}
	sortGroups(ready)

	out := make([]Point, 0, len(ready))
	for _, g := range ready {
		delete(m.groups, groupKey(g.seriesKey, g.ns, g.hasTime, g.firstOrder))
		out = append(out, g.toPoint())
	}
	return out
}

// Flush emits and removes every retained point, then advances the
// watermark to the maximum representable instant. Output is stable:
// timestamped points first (by time, then series key), followed by
// points without a timestamp (by their original input order).
//
// After Flush, any subsequently added timestamped point is reported late,
// and any point without a timestamp is still retained independently.
func (m *Merger) Flush() []Point {
	var timed, untimed []*group
	for _, g := range m.groups {
		if g.hasTime {
			timed = append(timed, g)
		} else {
			untimed = append(untimed, g)
		}
	}
	sortGroups(timed)
	sort.SliceStable(untimed, func(i, j int) bool {
		return untimed[i].firstOrder < untimed[j].firstOrder
	})

	ready := append(timed, untimed...)
	out := make([]Point, 0, len(ready))
	for _, g := range ready {
		delete(m.groups, groupKey(g.seriesKey, g.ns, g.hasTime, g.firstOrder))
		out = append(out, g.toPoint())
	}

	m.watermark = maxInt64
	m.hasWm = true
	m.flushed = true
	return out
}

// toPoint renders the accumulated group as an emitted Point. Tags are
// already canonical (sorted); fields are sorted by key for stable output.
func (g *group) toPoint() Point {
	names := append([]string(nil), g.names...)
	sort.Strings(names)
	fields := make([]Field, 0, len(names))
	for _, name := range names {
		fields = append(fields, Field{Key: name, Value: g.fields[name].value})
	}
	p := Point{
		Measurement: g.measurement,
		Tags:        cloneTags(g.tags),
		Fields:      fields,
	}
	if g.hasTime {
		p.HasTime = true
		p.Time = time.Unix(0, g.ns)
		p.Precision = lineprotocol.Nanosecond
	}
	return p
}

// sortGroups orders groups by time, then canonical series key, then by
// the input order that first created them.
func sortGroups(gs []*group) {
	sort.SliceStable(gs, func(i, j int) bool {
		if gs[i].ns != gs[j].ns {
			return gs[i].ns < gs[j].ns
		}
		if gs[i].seriesKey != gs[j].seriesKey {
			return gs[i].seriesKey < gs[j].seriesKey
		}
		return gs[i].firstOrder < gs[j].firstOrder
	})
}

const maxInt64 = int64(^uint64(0) >> 1)

// groupKey builds the map key. Timestamped groups are keyed by series and
// instant; untimestamped groups are keyed uniquely by sequence so they
// never merge.
func groupKey(seriesKey string, ns int64, hasTime bool, order int64) string {
	if !hasTime {
		return "u:" + strconv.FormatInt(order, 10)
	}
	var b strings.Builder
	b.Grow(len(seriesKey) + 16)
	b.WriteString("t:")
	b.WriteString(seriesKey)
	b.WriteByte('@')
	b.WriteString(strconv.FormatInt(ns, 10))
	return b.String()
}

// seriesIdentity builds the canonical series identity: the measurement
// plus the tag set sorted by key. Length-prefixed encoding removes
// ambiguity between names containing separators.
func seriesIdentity(measurement string, tags []Tag) string {
	var b strings.Builder
	writeLen(&b, measurement)
	b.WriteString(measurement)
	for _, t := range tags {
		b.WriteByte(';')
		writeLen(&b, t.Key)
		b.WriteString(t.Key)
		b.WriteByte('=')
		writeLen(&b, t.Value)
		b.WriteString(t.Value)
	}
	return b.String()
}

func writeLen(b *strings.Builder, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
}

// canonicalTags returns tags sorted by key and rejects duplicate keys.
func canonicalTags(tags []Tag) ([]Tag, error) {
	out := cloneTags(tags)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Value < out[j].Value
	})
	for i := 1; i < len(out); i++ {
		if out[i].Key == out[i-1].Key {
			return nil, fmt.Errorf("merger: duplicate tag key %q: %w", out[i].Key, ErrInvalid)
		}
	}
	return out, nil
}

func cloneTags(tags []Tag) []Tag {
	if len(tags) == 0 {
		return nil
	}
	out := make([]Tag, len(tags))
	copy(out, tags)
	return out
}

// toNanoseconds converts a timestamp supplied at prec to the exact
// instant it denotes in nanoseconds. The time is first truncated to the
// precision grid (mirroring how an integer timestamp at that precision is
// interpreted), then scaled to nanoseconds. Values that would overflow
// int64 nanoseconds are rejected with ErrOverflow.
func toNanoseconds(t time.Time, prec lineprotocol.Precision) (int64, error) {
	factor := int64(prec.Duration())
	if factor <= 0 {
		return 0, fmt.Errorf("merger: unknown precision %v: %w", prec, ErrInvalid)
	}
	raw := t.UnixNano()
	// Integer truncation toward zero. Time is expected to be well inside
	// the representable range; UnixNano itself saturates outside it, which
	// we surface as an overflow.
	if t.Before(minRepresentable) || t.After(maxRepresentable) {
		return 0, fmt.Errorf("merger: timestamp %s at %s: %w",
			t.Format(time.RFC3339Nano), prec, ErrOverflow)
	}
	units := raw / factor
	ns, overflowed := mulCheck(units, factor)
	if overflowed {
		return 0, fmt.Errorf("merger: timestamp %s at %s: %w",
			t.Format(time.RFC3339Nano), prec, ErrOverflow)
	}
	return ns, nil
}

var (
	minRepresentable = time.Unix(0, -1<<63)
	maxRepresentable = time.Unix(0, 1<<63-1)
)

// mulCheck returns a*b and whether the multiplication overflows int64.
func mulCheck(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, false
	}
	r := a * b
	if r/b != a {
		return 0, true
	}
	return r, false
}
