package batchplanner

import (
	"errors"
	"testing"

	"github.com/influxdata/line-protocol/v2/lineprotocol"
)

func mustPoint(measurement string, fields map[string]lineprotocol.ValueKind) Point {
	return Point{Measurement: measurement, Fields: fields}
}

func TestPlanBatches(t *testing.T) {
	tests := []struct {
		name       string
		points     []Point
		wantCount  int
		wantGroups [][]int
	}{{
		name: "missing field does not conflict",
		points: []Point{
			mustPoint("m", map[string]lineprotocol.ValueKind{"a": lineprotocol.Int}),
			mustPoint("m", map[string]lineprotocol.ValueKind{"b": lineprotocol.String}),
		},
		wantCount:  1,
		wantGroups: [][]int{{0, 1}},
	}, {
		name: "different measurement does not conflict",
		points: []Point{
			mustPoint("m1", map[string]lineprotocol.ValueKind{"a": lineprotocol.Int}),
			mustPoint("m2", map[string]lineprotocol.ValueKind{"a": lineprotocol.String}),
		},
		wantCount:  1,
		wantGroups: [][]int{{0, 1}},
	}, {
		name: "same kind does not conflict",
		points: []Point{
			mustPoint("m", map[string]lineprotocol.ValueKind{"a": lineprotocol.Int}),
			mustPoint("m", map[string]lineprotocol.ValueKind{"a": lineprotocol.Int}),
		},
		wantCount:  1,
		wantGroups: [][]int{{0, 1}},
	}, {
		name: "all five kinds are distinct",
		points: []Point{
			mustPoint("m", map[string]lineprotocol.ValueKind{"a": lineprotocol.String}),
			mustPoint("m", map[string]lineprotocol.ValueKind{"a": lineprotocol.Bool}),
			mustPoint("m", map[string]lineprotocol.ValueKind{"a": lineprotocol.Int}),
			mustPoint("m", map[string]lineprotocol.ValueKind{"a": lineprotocol.Uint}),
			mustPoint("m", map[string]lineprotocol.ValueKind{"a": lineprotocol.Float}),
		},
		wantCount:  5,
		wantGroups: [][]int{{0}, {1}, {2}, {3}, {4}},
	}, {
		name: "triangle with tail",
		points: []Point{
			mustPoint("m", map[string]lineprotocol.ValueKind{
				"e01": lineprotocol.Int,
				"e02": lineprotocol.Int,
			}),
			mustPoint("m", map[string]lineprotocol.ValueKind{
				"e01": lineprotocol.Float,
				"e12": lineprotocol.Int,
			}),
			mustPoint("m", map[string]lineprotocol.ValueKind{
				"e02": lineprotocol.Float,
				"e12": lineprotocol.Float,
				"e23": lineprotocol.Int,
			}),
			mustPoint("m", map[string]lineprotocol.ValueKind{
				"e23": lineprotocol.Float,
				"e34": lineprotocol.Int,
			}),
			mustPoint("m", map[string]lineprotocol.ValueKind{
				"e34": lineprotocol.Float,
				"e45": lineprotocol.Int,
			}),
			mustPoint("m", map[string]lineprotocol.ValueKind{
				"e45": lineprotocol.Float,
				"e56": lineprotocol.Int,
			}),
			mustPoint("m", map[string]lineprotocol.ValueKind{
				"e56": lineprotocol.Float,
				"e67": lineprotocol.Int,
			}),
			mustPoint("m", map[string]lineprotocol.ValueKind{
				"e67": lineprotocol.Float,
			}),
		},
		wantCount:  3,
		wantGroups: [][]int{{0, 3, 5, 7}, {1, 4, 6}, {2}},
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := PlanBatches(test.points)
			if err != nil {
				t.Fatalf("PlanBatches returned error: %v", err)
			}
			if plan.BatchCount != test.wantCount {
				t.Fatalf("BatchCount = %d, want %d", plan.BatchCount, test.wantCount)
			}
			if got := groupsFromAssignment(plan.PointBatches); !equalGroups(got, test.wantGroups) {
				t.Fatalf("groups = %v, want %v", got, test.wantGroups)
			}
			if err := validatePlan(test.points, plan); err != nil {
				t.Fatalf("returned plan is invalid: %v", err)
			}
			if want := exhaustiveBatchCount(test.points); want != plan.BatchCount {
				t.Fatalf("BatchCount = %d, exhaustive result is %d", plan.BatchCount, want)
			}
		})
	}
}

func TestConflictFields(t *testing.T) {
	points := []Point{
		mustPoint("m", map[string]lineprotocol.ValueKind{
			"a": lineprotocol.Int,
			"b": lineprotocol.Int,
			"c": lineprotocol.Int,
		}),
		mustPoint("m", map[string]lineprotocol.ValueKind{
			"a": lineprotocol.Float,
			"b": lineprotocol.Uint,
			"c": lineprotocol.Int,
		}),
	}

	plan, err := PlanBatches(points)
	if err != nil {
		t.Fatalf("PlanBatches returned error: %v", err)
	}
	if len(plan.Conflicts) != 1 {
		t.Fatalf("Conflicts length = %d, want 1", len(plan.Conflicts))
	}
	conflict := plan.Conflicts[0]
	if conflict.Point1 != 0 || conflict.Point2 != 1 {
		t.Fatalf("conflict endpoints = (%d,%d), want (0,1)", conflict.Point1, conflict.Point2)
	}
	wantFields := []string{"a", "b"}
	if len(conflict.Fields) != len(wantFields) {
		t.Fatalf("Fields = %v, want %v", conflict.Fields, wantFields)
	}
	for i := range wantFields {
		if conflict.Fields[i] != wantFields[i] {
			t.Fatalf("Fields = %v, want %v", conflict.Fields, wantFields)
		}
	}
}

func TestPointwiseGreedyIsNotOptimal(t *testing.T) {
	points := []Point{
		mustPoint("m", map[string]lineprotocol.ValueKind{"ab": lineprotocol.Int}),
		mustPoint("m", map[string]lineprotocol.ValueKind{
			"ab": lineprotocol.Float,
			"bd": lineprotocol.Float,
		}),
		mustPoint("m", map[string]lineprotocol.ValueKind{"cd": lineprotocol.Int}),
		mustPoint("m", map[string]lineprotocol.ValueKind{"cd": lineprotocol.Float, "bd": lineprotocol.Int}),
	}

	plan, err := PlanBatches(points)
	if err != nil {
		t.Fatalf("PlanBatches returned error: %v", err)
	}
	greedyAssignment, greedyCount := pointwiseGreedy(points)
	if greedyCount <= plan.BatchCount {
		t.Fatalf("greedy count = %d, want a result worse than %d", greedyCount, plan.BatchCount)
	}
	if greedyCount != 3 || greedyAssignment[3] != 2 {
		t.Fatalf("greedy assignment = %v (%d batches), want fourth point in batch 2", greedyAssignment, greedyCount)
	}
	if plan.BatchCount != 2 || plan.PointBatches[3] != 0 {
		t.Fatalf("optimal assignment = %v (%d batches), want fourth point in batch 0", plan.PointBatches, plan.BatchCount)
	}
}

func TestRejectsTooManyPoints(t *testing.T) {
	points := make([]Point, MaxPoints+1)
	for i := range points {
		points[i] = mustPoint("m", nil)
	}

	_, err := PlanBatches(points)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

func TestRejectsUnknownKind(t *testing.T) {
	_, err := PlanBatches([]Point{{
		Measurement: "m",
		Fields:      map[string]lineprotocol.ValueKind{"a": lineprotocol.Unknown},
	}})
	if err == nil {
		t.Fatal("PlanBatches returned nil error for unknown kind")
	}
}

func TestEmptyInput(t *testing.T) {
	plan, err := PlanBatches(nil)
	if err != nil {
		t.Fatalf("PlanBatches returned error: %v", err)
	}
	if plan.BatchCount != 0 || len(plan.PointBatches) != 0 || len(plan.Conflicts) != 0 {
		t.Fatalf("empty plan = %#v, want an empty plan", plan)
	}
}

type fieldTypeKey struct {
	measurement string
	field       string
}

func pointwiseGreedy(points []Point) ([]int, int) {
	assignment := make([]int, len(points))
	batchTypes := make([]map[fieldTypeKey]lineprotocol.ValueKind, 0)

	for i, point := range points {
		assigned := -1
		for batch := range batchTypes {
			if batchFits(batchTypes[batch], point) {
				assigned = batch
				break
			}
		}
		if assigned == -1 {
			assigned = len(batchTypes)
			batchTypes = append(batchTypes, make(map[fieldTypeKey]lineprotocol.ValueKind))
		}
		for field, kind := range point.Fields {
			batchTypes[assigned][fieldTypeKey{measurement: point.Measurement, field: field}] = kind
		}
		assignment[i] = assigned
	}
	return assignment, len(batchTypes)
}

func batchFits(types map[fieldTypeKey]lineprotocol.ValueKind, point Point) bool {
	for field, kind := range point.Fields {
		key := fieldTypeKey{measurement: point.Measurement, field: field}
		if existing, ok := types[key]; ok && existing != kind {
			return false
		}
	}
	return true
}

func validatePlan(points []Point, plan *Plan) error {
	if len(plan.PointBatches) != len(points) {
		return errors.New("assignment length does not match point count")
	}

	seen := make(map[int]bool)
	for i, batch := range plan.PointBatches {
		if batch < 0 || batch >= plan.BatchCount {
			return errors.New("assignment contains an invalid batch")
		}
		seen[batch] = true
		for j := i + 1; j < len(points); j++ {
			if plan.PointBatches[j] == batch && pointsConflict(points[i], points[j]) {
				return errors.New("conflicting points share a batch")
			}
		}
	}
	if len(seen) != plan.BatchCount {
		return errors.New("batch count includes an empty batch")
	}
	return nil
}

func pointsConflict(left, right Point) bool {
	if left.Measurement != right.Measurement {
		return false
	}
	for field, leftKind := range left.Fields {
		rightKind, ok := right.Fields[field]
		if ok && leftKind != rightKind {
			return true
		}
	}
	return false
}

func groupsFromAssignment(assignment []int) [][]int {
	groups := make([][]int, 0)
	for point, batch := range assignment {
		for len(groups) <= batch {
			groups = append(groups, nil)
		}
		groups[batch] = append(groups[batch], point)
	}
	return groups
}

func equalGroups(left, right [][]int) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if len(left[i]) != len(right[i]) {
			return false
		}
		for j := range left[i] {
			if left[i][j] != right[i][j] {
				return false
			}
		}
	}
	return true
}

func exhaustiveBatchCount(points []Point) int {
	if len(points) == 0 {
		return 0
	}

	restricted := make([]int, len(points))
	best := len(points)
	var enumerate func(int, int)
	enumerate = func(next int, used int) {
		if used >= best {
			return
		}
		if next == len(points) {
			if partitionValid(points, restricted, used) {
				best = used
			}
			return
		}

		for batch := 0; batch < used; batch++ {
			restricted[next] = batch
			enumerate(next+1, used)
		}
		restricted[next] = used
		enumerate(next+1, used+1)
	}
	enumerate(0, 0)
	return best
}

func partitionValid(points []Point, assignment []int, batchCount int) bool {
	for batch := 0; batch < batchCount; batch++ {
		members := make([]int, 0)
		for point, assignedBatch := range assignment {
			if assignedBatch == batch {
				members = append(members, point)
			}
		}
		for i := 0; i < len(members); i++ {
			for j := i + 1; j < len(members); j++ {
				if pointsConflict(points[members[i]], points[members[j]]) {
					return false
				}
			}
		}
	}
	return true
}
