// Package batchmerge implements a bounded, stateful merger for offline
// collection batches where one logical point can arrive split across
// several lines that each fill in different fields.
//
// Points that share the same canonical series identity (measurement and
// lexicographically ordered tags) and the same exact instant are merged
// into one point carrying the union of their fields. The exact instant is
// derived from each point's timestamp and its input precision, so points
// expressed in different precisions merge whenever they denote the same
// instant.
//
// Points without a timestamp never merge automatically, not even with each
// other; they are retained as independent inputs.
//
// Output order is stable and independent of arrival order: timed points are
// emitted by exact time and then series identity, fields and tags are sorted
// by key, and untimestamped points follow in input order.
package batchmerge

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/influxdata/line-protocol/v2/lineprotocol"
)

// FieldConflict selects how a field that appears in more than one input
// point with the same value kind is resolved.
type FieldConflict int

const (
	// FirstField keeps the value from the earliest input, according to
	// input sequence (the order in which Add is called), not map order.
	FirstField FieldConflict = iota
	// LastField replaces the value with that of the latest input,
	// according to input sequence.
	LastField
	// ErrorField reports a FieldConflictError whenever a later field differs
	// from the already merged value. Equal values never conflict.
	ErrorField
)

// TypeChange selects how a field is handled when a later input provides a
// value of a different kind than the already merged value.
type TypeChange int

const (
	// RejectType reports a TypeConflictError and leaves the merged
	// value untouched.
	RejectType TypeChange = iota
	// AllowType replaces the existing value regardless of kind, in
	// accordance with the configured FieldConflict policy.
	AllowType
)

// Sentinel errors returned by Merger methods. Use errors.Is to test for them.
var (
	// ErrCapacity is returned when adding a point would retain more than
	// MaxPoints entries. No state is evicted to make room: the caller must
	// drain the merger (for example by advancing the watermark) or raise its
	// bound. This is the back-pressure signal.
	ErrCapacity = errors.New("batchmerge: retained point limit reached")
	// ErrInvalidPoint is returned for structurally invalid points: an empty
	// measurement or field set, empty tag/field keys, or a timestamp that
	// overflows nanosecond range.
	ErrInvalidPoint = errors.New("batchmerge: invalid point")
	// ErrLate is wrapped by LateError for points whose exact time is not
	// later than the current watermark.
	ErrLate = errors.New("batchmerge: point is late")
)

// FieldConflictError describes a same-kind field whose later value differs
// from the merged value when FieldConflict is ErrorField.
type FieldConflictError struct {
	Measurement string
	Time        time.Time
	Field       string
	First       lineprotocol.Value
	Incoming    lineprotocol.Value
}

func (e *FieldConflictError) Error() string {
	return fmt.Sprintf("batchmerge: conflicting values for field %q of %s at %v: %v vs %v",
		e.Field, e.Measurement, e.Time, e.First, e.Incoming)
}

// TypeConflictError describes a field whose value kind changes between
// inputs when TypeChange is RejectType.
type TypeConflictError struct {
	Measurement string
	Time        time.Time
	Field       string
	Existing    lineprotocol.ValueKind
	Incoming    lineprotocol.ValueKind
}

func (e *TypeConflictError) Error() string {
	return fmt.Sprintf("batchmerge: field %q of %s at %v changed type: %s vs %s",
		e.Field, e.Measurement, e.Time, e.Existing, e.Incoming)
}

// LateError reports that an added point is at or before the watermark. The
// point is not merged, buffered or used to rewrite already emitted output.
type LateError struct {
	Measurement string
	Time        time.Time
	Watermark   time.Time
}

// Is reports the error as ErrLate.
func (e *LateError) Is(target error) bool { return target == ErrLate }

func (e *LateError) Error() string {
	return fmt.Sprintf("batchmerge: point %s at %v is not after the watermark %v",
		e.Measurement, e.Time, e.Watermark)
}

// Tag is one measurement tag.
type Tag struct {
	Key   []byte
	Value []byte
}

// Field is one measurement field with its decoded value.
type Field struct {
	Key   []byte
	Value lineprotocol.Value
}

// Point is one input point.
//
// HasTime reports whether the point carries a timestamp; when it is false the
// RawTime and Precision fields are ignored and the point is retained as an
// independent input that never merges.
//
// RawTime is the timestamp expressed in Precision units; Precision is the
// input precision used to convert RawTime to the exact instant used as the
// merge key.
type Point struct {
	Measurement []byte
	Tags        []Tag
	Fields      []Field
	HasTime     bool
	RawTime     int64
	Precision   lineprotocol.Precision
}

// MergedPoint is one emitted point. The measurement, tag and field slices and
// the byte slices they reference are copies owned by the merger; they remain
// valid until the next call that mutates Merger state.
type MergedPoint struct {
	Measurement []byte
	Tags        []Tag
	Fields      []Field
	Time        time.Time
	HasTime     bool
}

// Config configures a Merger.
type Config struct {
	// FieldConflict resolves same-kind duplicate fields.
	FieldConflict FieldConflict
	// TypeChange resolves fields whose value kind changes between inputs.
	TypeChange TypeChange
	// MaxPoints bounds the number of retained points (merged timed groups
	// plus independent untimestamped points). Zero or a negative value means
	// unbounded. Once the bound is reached, Add returns ErrCapacity instead
	// of evicting state.
	MaxPoints int
}

type group struct {
	measurement []byte
	tags        []Tag
	timeNS      int64
	fields      map[string]Field
	seriesKey   string
	hasTime     bool
	// order is the input sequence of the group, used for untimestamped
	// points which never merge.
	order uint64
}

// Merger merges points of an offline collection batch.
//
// A Merger is not safe for concurrent use.
type Merger struct {
	cfg Config

	// timed holds timed groups keyed by series identity and exact time.
	timed map[string]map[int64]*group
	// untimed holds one independent group per untimestamped input, in order.
	untimed []*group

	watermarkNS  int64
	hasWatermark bool
	seq          uint64
}

// New returns a Merger using cfg.
func New(cfg Config) *Merger {
	return &Merger{
		cfg:   cfg,
		timed: make(map[string]map[int64]*group),
	}
}

// Retained reports the number of points currently retained: merged timed
// groups plus independent untimestamped points.
func (m *Merger) Retained() int {
	n := len(m.untimed)
	for _, at := range m.timed {
		n += len(at)
	}
	return n
}

// Add buffers one point, merging it into an existing group when its canonical
// series identity and exact time match.
//
// Fields from earlier inputs are combined with the new point's fields under
// the configured FieldConflict and TypeChange policies. On error the merger
// state is left unchanged: a rejected point merges nothing.
//
// Add returns a *LateError (matching errors.Is(err, ErrLate)) when the
// point's time is at or before the watermark, and ErrCapacity when retaining
// it would exceed MaxPoints.
func (m *Merger) Add(p Point) error {
	if len(p.Measurement) == 0 || len(p.Fields) == 0 {
		return fmt.Errorf("%w: measurement and at least one field are required", ErrInvalidPoint)
	}
	tags, err := canonicalTags(p.Tags)
	if err != nil {
		return err
	}
	fields, err := normalizeFields(p.Fields)
	if err != nil {
		return err
	}

	if !p.HasTime {
		if !m.roomFor(1) {
			return ErrCapacity
		}
		m.seq++
		m.untimed = append(m.untimed, &group{
			measurement: cloneBytes(p.Measurement),
			tags:        tags,
			fields:      fields,
			hasTime:     false,
			order:       m.seq,
		})
		return nil
	}

	ns, ok := toNanoseconds(p.RawTime, p.Precision)
	if !ok {
		return fmt.Errorf("%w: timestamp overflows nanosecond range", ErrInvalidPoint)
	}
	if m.hasWatermark && ns <= m.watermarkNS {
		return &LateError{
			Measurement: string(p.Measurement),
			Time:        time.Unix(0, ns).UTC(),
			Watermark:   time.Unix(0, m.watermarkNS).UTC(),
		}
	}

	key := seriesKey(p.Measurement, tags)
	at := m.timed[key]
	g := at[ns]
	if g == nil {
		if !m.roomFor(1) {
			return ErrCapacity
		}
		if at == nil {
			at = make(map[int64]*group)
			m.timed[key] = at
		}
		g = &group{
			measurement: cloneBytes(p.Measurement),
			tags:        tags,
			timeNS:      ns,
			fields:      make(map[string]Field),
			seriesKey:   key,
			hasTime:     true,
		}
		at[ns] = g
	}
	// Validate every conflict before mutating so a rejected point changes
	// nothing in the merged state.
	for fkey, f := range fields {
		if err := m.fieldConflict(g, fkey, f); err != nil {
			return err
		}
	}
	for fkey, f := range fields {
		m.applyField(g, fkey, f)
	}
	return nil
}

// AdvanceWatermark moves the watermark forward to w and emits all retained
// timed points whose exact time is at or before it, in stable order
// (ascending time, then series identity), releasing their state. A w earlier
// than the current watermark is ignored.
//
// Untimestamped points are not released by the watermark; use Flush to drain
// them. Timed points added afterwards at or before the watermark are reported
// late.
func (m *Merger) AdvanceWatermark(w time.Time) []MergedPoint {
	ns := w.UnixNano()
	if !m.hasWatermark || ns > m.watermarkNS {
		m.watermarkNS = ns
		m.hasWatermark = true
	}
	return m.drainTimed(func(t int64) bool { return t <= m.watermarkNS })
}

// Flush emits every retained point and releases its state. Timed points are
// emitted first in stable order, followed by untimestamped points in input
// order. The watermark advances to the latest flushed time when there are
// timed points, so a later input for an older instant is reported late and
// cannot rewrite emitted results. The Merger can keep being used for later
// batches.
func (m *Merger) Flush() []MergedPoint {
	out := m.drainTimed(nil)
	if len(out) > 0 {
		m.watermarkNS = out[len(out)-1].Time.UnixNano()
		m.hasWatermark = true
	}
	groups := m.untimed
	m.untimed = nil
	for _, g := range groups {
		out = append(out, toMergedPoint(g))
	}
	return out
}

// Reset drops all retained points and the watermark, returning the Merger to
// its state immediately after New.
func (m *Merger) Reset() {
	m.timed = make(map[string]map[int64]*group)
	m.untimed = nil
	m.watermarkNS = 0
	m.hasWatermark = false
	m.seq = 0
}

func (m *Merger) roomFor(extra int) bool {
	return m.cfg.MaxPoints <= 0 || m.Retained()+extra <= m.cfg.MaxPoints
}

func (m *Merger) drainTimed(include func(int64) bool) []MergedPoint {
	var ready []*group
	for _, at := range m.timed {
		for ns, g := range at {
			if include == nil || include(ns) {
				ready = append(ready, g)
			}
		}
	}
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].timeNS != ready[j].timeNS {
			return ready[i].timeNS < ready[j].timeNS
		}
		return ready[i].seriesKey < ready[j].seriesKey
	})
	if len(ready) == 0 {
		return nil
	}
	out := make([]MergedPoint, 0, len(ready))
	for _, g := range ready {
		at := m.timed[g.seriesKey]
		delete(at, g.timeNS)
		if len(at) == 0 {
			delete(m.timed, g.seriesKey)
		}
		out = append(out, toMergedPoint(g))
	}
	return out
}

// fieldConflict reports whether accepting f for fkey violates the
// configured policies, without changing g.
func (m *Merger) fieldConflict(g *group, fkey string, f Field) error {
	existing, ok := g.fields[fkey]
	if !ok {
		return nil
	}
	oldKind := existing.Value.Kind()
	newKind := f.Value.Kind()
	if oldKind != newKind && m.cfg.TypeChange == RejectType {
		return &TypeConflictError{
			Measurement: string(g.measurement),
			Time:        time.Unix(0, g.timeNS).UTC(),
			Field:       fkey,
			Existing:    oldKind,
			Incoming:    newKind,
		}
	}
	if m.cfg.FieldConflict == ErrorField && oldKind == newKind &&
		!existing.Value.Equal(f.Value) {
		return &FieldConflictError{
			Measurement: string(g.measurement),
			Time:        time.Unix(0, g.timeNS).UTC(),
			Field:       fkey,
			First:       existing.Value,
			Incoming:    f.Value,
		}
	}
	return nil
}

// applyField records f for fkey. FirstField keeps the earliest value;
// LastField uses the incoming one; ErrorField keeps equal same-kind values
// and takes the incoming value only when a type change is allowed.
func (m *Merger) applyField(g *group, fkey string, f Field) {
	existing, ok := g.fields[fkey]
	if !ok {
		g.fields[fkey] = f
		return
	}
	if m.cfg.FieldConflict == FirstField {
		return
	}
	if m.cfg.FieldConflict == LastField {
		g.fields[fkey] = f
		return
	}
	if existing.Value.Kind() != f.Value.Kind() && m.cfg.TypeChange == AllowType {
		g.fields[fkey] = f
	}
}
