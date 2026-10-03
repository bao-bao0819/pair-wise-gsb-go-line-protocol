package batchplan

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/influxdata/line-protocol/v2/lineprotocol"
)

func pt(measurement string, fields ...Field) Point {
	return Point{Measurement: measurement, Fields: fields}
}

func fld(key string, kind lineprotocol.ValueKind) Field {
	return Field{Key: key, Kind: kind}
}

// TestGreedyFirstFitIsNotOptimal builds the crown graph K_{3,3} minus a
// perfect matching: point-by-point greedy first-fit (in input order)
// needs 3 batches, but the exact minimum is 2.
func TestGreedyFirstFitIsNotOptimal(t *testing.T) {
	points := []Point{
		pt("m", fld("f1", lineprotocol.Int)),                                  // v1
		pt("m", fld("f2", lineprotocol.Float), fld("f3", lineprotocol.Float)), // w1
		pt("m", fld("f2", lineprotocol.Int)),                                  // v2
		pt("m", fld("f1", lineprotocol.Float), fld("f3", lineprotocol.Float)), // w2
		pt("m", fld("f3", lineprotocol.Int)),                                  // v3
		pt("m", fld("f1", lineprotocol.Float), fld("f2", lineprotocol.Float)), // w3
	}

	if got := greedyFirstFitBatches(points); got != 3 {
		t.Fatalf("greedy first-fit used %d batches, want 3 (test premise broken)", got)
	}

	res, err := Plan(points)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if res.NumBatches != 2 {
		t.Fatalf("NumBatches = %d, want exact minimum 2", res.NumBatches)
	}
	want := []int{0, 1, 0, 1, 0, 1}
	if !reflect.DeepEqual(res.Assignment, want) {
		t.Fatalf("Assignment = %v, want stable %v", res.Assignment, want)
	}
}

// greedyFirstFitBatches is the naive point-by-point heuristic: each
// point joins the first batch it does not conflict with.
func greedyFirstFitBatches(points []Point) int {
	var batches [][]int
	for i := range points {
		placed := false
		for b := range batches {
			ok := true
			for _, j := range batches[b] {
				if pointsConflict(points[i], points[j]) {
					ok = false
					break
				}
			}
			if ok {
				batches[b] = append(batches[b], i)
				placed = true
				break
			}
		}
		if !placed {
			batches = append(batches, []int{i})
		}
	}
	return len(batches)
}

func pointsConflict(a, b Point) bool {
	if a.Measurement != b.Measurement {
		return false
	}
	for _, fa := range a.Fields {
		for _, fb := range b.Fields {
			if fa.Key == fb.Key && fa.Kind != fb.Kind {
				return true
			}
		}
	}
	return false
}

func TestMissingFieldDoesNotConflict(t *testing.T) {
	points := []Point{
		pt("m", fld("temp", lineprotocol.Int)),
		pt("m", fld("other", lineprotocol.Float)),
		pt("m", fld("temp", lineprotocol.Int), fld("other", lineprotocol.Float)),
	}
	res, err := Plan(points)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if res.NumBatches != 1 {
		t.Fatalf("NumBatches = %d, want 1 (missing fields must not conflict)", res.NumBatches)
	}
	if len(res.Conflicts) != 0 {
		t.Fatalf("Conflicts = %v, want none", res.Conflicts)
	}
}

func TestDistinctMeasurementsDoNotConflict(t *testing.T) {
	points := []Point{
		pt("cpu", fld("v", lineprotocol.Int)),
		pt("mem", fld("v", lineprotocol.Float)),
	}
	res, err := Plan(points)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if res.NumBatches != 1 {
		t.Fatalf("NumBatches = %d, want 1", res.NumBatches)
	}
}

func TestAllValueKindsAreDistinct(t *testing.T) {
	points := []Point{
		pt("m", fld("v", lineprotocol.String)),
		pt("m", fld("v", lineprotocol.Bool)),
		pt("m", fld("v", lineprotocol.Int)),
		pt("m", fld("v", lineprotocol.Uint)),
		pt("m", fld("v", lineprotocol.Float)),
	}
	res, err := Plan(points)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if res.NumBatches != 5 {
		t.Fatalf("NumBatches = %d, want 5 (all kinds mutually conflict)", res.NumBatches)
	}
	want := []int{0, 1, 2, 3, 4}
	if !reflect.DeepEqual(res.Assignment, want) {
		t.Fatalf("Assignment = %v, want %v", res.Assignment, want)
	}
}

func TestTooManyPointsUnsupported(t *testing.T) {
	points := make([]Point, MaxPoints+1)
	for i := range points {
		points[i] = pt("m", fld("v", lineprotocol.Int))
	}
	_, err := Plan(points)
	if err != ErrTooManyPoints {
		t.Fatalf("Plan error = %v, want ErrTooManyPoints", err)
	}

	// Exactly MaxPoints must still be answered exactly.
	res, err := Plan(points[:MaxPoints])
	if err != nil {
		t.Fatalf("Plan(MaxPoints): %v", err)
	}
	if res.NumBatches != 1 {
		t.Fatalf("NumBatches = %d, want 1", res.NumBatches)
	}
}

func TestConflictEdgeTraceableToField(t *testing.T) {
	points := []Point{
		pt("m", fld("temp", lineprotocol.Int), fld("ok", lineprotocol.Bool)),
		pt("m", fld("temp", lineprotocol.Float), fld("ok", lineprotocol.Bool)),
	}
	res, err := Plan(points)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("len(Conflicts) = %d, want 1", len(res.Conflicts))
	}
	c := res.Conflicts[0]
	if c.Points != [2]int{0, 1} {
		t.Fatalf("Conflict.Points = %v, want [0 1]", c.Points)
	}
	want := []FieldConflict{{
		Measurement: "m",
		Key:         "temp",
		Kinds:       [2]lineprotocol.ValueKind{lineprotocol.Int, lineprotocol.Float},
	}}
	if !reflect.DeepEqual(c.Fields, want) {
		t.Fatalf("Conflict.Fields = %+v, want %+v", c.Fields, want)
	}
}

func TestEmptyInput(t *testing.T) {
	res, err := Plan(nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if res.NumBatches != 0 || len(res.Assignment) != 0 {
		t.Fatalf("got %+v, want zero result", res)
	}
}

func TestDeterministicAcrossRuns(t *testing.T) {
	points := []Point{
		pt("m", fld("a", lineprotocol.Int)),
		pt("m", fld("a", lineprotocol.Float)),
		pt("m", fld("a", lineprotocol.String)),
		pt("m", fld("b", lineprotocol.Bool)),
	}
	first, err := Plan(points)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for i := 0; i < 10; i++ {
		again, err := Plan(points)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("non-deterministic result: %+v vs %+v", first, again)
		}
	}
}

// referenceAssignment is an independent, deliberately simple
// brute-force reference: it finds the minimum k by exhaustive
// k-colorability search and returns the lexicographically smallest
// canonically numbered assignment for that k.
func referenceAssignment(points []Point) (int, []int) {
	n := len(points)
	adj := make([][]bool, n)
	for i := range adj {
		adj[i] = make([]bool, n)
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if pointsConflict(points[i], points[j]) {
				adj[i][j] = true
				adj[j][i] = true
			}
		}
	}
	for k := 1; k <= n; k++ {
		colors := make([]int, n)
		used := 0
		var assign func(v int) bool
		assign = func(v int) bool {
			if v == n {
				return true
			}
			limit := used
			if limit < k {
				limit++
			}
			for c := 0; c < limit; c++ {
				clash := false
				for u := 0; u < v; u++ {
					if adj[v][u] && colors[u] == c {
						clash = true
						break
					}
				}
				if clash {
					continue
				}
				isNew := c == used
				colors[v] = c
				if isNew {
					used++
				}
				if assign(v + 1) {
					return true
				}
				if isNew {
					used--
				}
			}
			return false
		}
		if assign(0) {
			return k, colors
		}
	}
	return n, nil
}

func checkAgainstReference(t *testing.T, points []Point) {
	t.Helper()
	res, err := Plan(points)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	wantK, wantAssign := referenceAssignment(points)
	if res.NumBatches != wantK {
		t.Fatalf("points=%v: NumBatches = %d, reference = %d", points, res.NumBatches, wantK)
	}
	if !reflect.DeepEqual(res.Assignment, wantAssign) {
		t.Fatalf("points=%v: Assignment = %v, reference = %v", points, res.Assignment, wantAssign)
	}
	// Every batch must be internally conflict-free.
	for i := 0; i < len(points); i++ {
		for j := i + 1; j < len(points); j++ {
			if res.Assignment[i] == res.Assignment[j] && pointsConflict(points[i], points[j]) {
				t.Fatalf("points=%v: conflicting points %d and %d share batch %d",
					points, i, j, res.Assignment[i])
			}
		}
	}
	// Every reported conflict edge must be genuine and field-traceable.
	for _, c := range res.Conflicts {
		if len(c.Fields) == 0 {
			t.Fatalf("points=%v: conflict %v has no field detail", points, c.Points)
		}
		for _, fc := range c.Fields {
			a, b := points[c.Points[0]], points[c.Points[1]]
			if fc.Measurement != a.Measurement || a.Measurement != b.Measurement {
				t.Fatalf("points=%v: bad conflict measurement %+v", points, fc)
			}
			if !hasFieldKind(a, fc.Key, fc.Kinds[0]) || !hasFieldKind(b, fc.Key, fc.Kinds[1]) {
				t.Fatalf("points=%v: conflict field %+v not present in points", points, fc)
			}
			if fc.Kinds[0] == fc.Kinds[1] {
				t.Fatalf("points=%v: conflict field %+v has equal kinds", points, fc)
			}
		}
	}
}

func hasFieldKind(p Point, key string, kind lineprotocol.ValueKind) bool {
	for _, f := range p.Fields {
		if f.Key == key && f.Kind == kind {
			return true
		}
	}
	return false
}

// TestExhaustiveUpToEightPoints enumerates every input of up to 8
// points drawn from a small signature set that exercises same-kind,
// different-kind and missing-field combinations, and checks each
// against the brute-force reference.
func TestExhaustiveUpToEightPoints(t *testing.T) {
	sigs := []Point{
		pt("m", fld("f1", lineprotocol.Int)),
		pt("m", fld("f1", lineprotocol.Float)),
		pt("m", fld("f1", lineprotocol.Int), fld("f2", lineprotocol.Bool)),
		pt("m", fld("f2", lineprotocol.String)),
	}
	const maxN = 8
	points := make([]Point, 0, maxN)
	var enumerate func(n int)
	enumerate = func(n int) {
		if len(points) == n {
			checkAgainstReference(t, append([]Point(nil), points...))
			return
		}
		for _, s := range sigs {
			points = append(points, s)
			enumerate(n)
			points = points[:len(points)-1]
		}
	}
	for n := 1; n <= maxN; n++ {
		enumerate(n)
	}
}

// TestRandomizedAgainstBruteForce fuzzes richer inputs (multiple
// measurements, more fields and kinds) with up to 8 points.
func TestRandomizedAgainstBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	kinds := []lineprotocol.ValueKind{
		lineprotocol.String,
		lineprotocol.Bool,
		lineprotocol.Int,
		lineprotocol.Uint,
		lineprotocol.Float,
	}
	measurements := []string{"m1", "m2"}
	keys := []string{"a", "b", "c"}
	for iter := 0; iter < 2000; iter++ {
		n := 1 + rng.Intn(8)
		points := make([]Point, n)
		for i := range points {
			p := pt(measurements[rng.Intn(len(measurements))])
			for _, key := range keys {
				if rng.Intn(2) == 0 {
					p.Fields = append(p.Fields, fld(key, kinds[rng.Intn(len(kinds))]))
				}
			}
			if len(p.Fields) == 0 {
				p.Fields = append(p.Fields, fld(keys[rng.Intn(len(keys))], kinds[rng.Intn(len(kinds))]))
			}
			points[i] = p
		}
		checkAgainstReference(t, points)
	}
}
