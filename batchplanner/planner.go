// Package batchplanner groups already-parsed line-protocol points into the
// minimum number of batches in which every measurement/field pair uses one
// value kind.
package batchplanner

import (
	"errors"
	"sort"

	"github.com/influxdata/line-protocol/v2/lineprotocol"
)

// MaxPoints is the largest number of points accepted by PlanBatches.
const MaxPoints = 8

// ErrUnsupported is returned when a planning request is too large for the
// exact small-input planner.
var ErrUnsupported = errors.New("batch planner: unsupported input")

// Point is an already-parsed line-protocol point. Only its measurement and
// field value kinds are used for planning.
type Point struct {
	Measurement string
	Fields      map[string]lineprotocol.ValueKind
}

// Conflict identifies two input points that cannot share a batch. Fields
// contains every field responsible for that edge.
type Conflict struct {
	Point1 int
	Point2 int
	Fields []string
}

// Plan is an exact batch assignment.
type Plan struct {
	// BatchCount is the minimum possible number of non-empty batches.
	BatchCount int
	// PointBatches gives the zero-based batch number of every input point.
	PointBatches []int
	// Conflicts contains the undirected conflict graph, ordered by point index
	// and then field name.
	Conflicts []Conflict
}

// PlanBatches returns the minimum number of batches required for points.
//
// A missing field does not conflict with any other value for that field.
// String, bool, signed integer, unsigned integer, and float kinds are all
// distinct. Exact exhaustive search is supported only for MaxPoints points.
func PlanBatches(points []Point) (*Plan, error) {
	if len(points) > MaxPoints {
		return nil, ErrUnsupported
	}
	if err := validate(points); err != nil {
		return nil, err
	}

	conflicts := buildConflicts(points)
	adjacency := buildAdjacency(len(points), conflicts)

	assignment := make([]int, len(points))
	batchCount := 0
	if len(points) > 0 {
		batchCount = minimumColorCount(adjacency)
		colorInOrder(adjacency, batchCount, assignment)
		canonicalize(assignment)
	}

	return &Plan{
		BatchCount:   batchCount,
		PointBatches: assignment,
		Conflicts:    conflicts,
	}, nil
}

func validate(points []Point) error {
	for i := range points {
		for _, kind := range points[i].Fields {
			if kind < lineprotocol.String || kind > lineprotocol.Bool {
				return errors.New("batch planner: unknown field value kind")
			}
		}
	}
	return nil
}

func buildConflicts(points []Point) []Conflict {
	var conflicts []Conflict
	for i := 0; i < len(points); i++ {
		for j := i + 1; j < len(points); j++ {
			if points[i].Measurement != points[j].Measurement {
				continue
			}

			fields := make([]string, 0)
			for field, kind1 := range points[i].Fields {
				kind2, ok := points[j].Fields[field]
				if ok && kind1 != kind2 {
					fields = append(fields, field)
				}
			}
			if len(fields) > 0 {
				sort.Strings(fields)
				conflicts = append(conflicts, Conflict{
					Point1: i,
					Point2: j,
					Fields: fields,
				})
			}
		}
	}
	return conflicts
}

func buildAdjacency(pointCount int, conflicts []Conflict) [][]int {
	adjacency := make([][]int, pointCount)
	for _, conflict := range conflicts {
		adjacency[conflict.Point1] = append(adjacency[conflict.Point1], conflict.Point2)
		adjacency[conflict.Point2] = append(adjacency[conflict.Point2], conflict.Point1)
	}
	for i := range adjacency {
		sort.Ints(adjacency[i])
	}
	return adjacency
}

func minimumColorCount(adjacency [][]int) int {
	colors := make([]int, len(adjacency))
	for batchCount := 1; batchCount <= len(adjacency); batchCount++ {
		if colorInOrder(adjacency, batchCount, colors) {
			return batchCount
		}
	}
	return len(adjacency)
}

func colorInOrder(adjacency [][]int, batchCount int, colors []int) bool {
	for i := range colors {
		colors[i] = -1
	}
	return colorVertex(adjacency, batchCount, colors, 0)
}

func colorVertex(adjacency [][]int, batchCount int, colors []int, vertex int) bool {
	if vertex == len(adjacency) {
		return true
	}

	for color := 0; color < batchCount; color++ {
		if colorAvailable(color, vertex, colors, adjacency) {
			colors[vertex] = color
			if colorVertex(adjacency, batchCount, colors, vertex+1) {
				return true
			}
		}
	}
	colors[vertex] = -1
	return false
}

func colorAvailable(color, vertex int, colors []int, adjacency [][]int) bool {
	for _, neighbor := range adjacency[vertex] {
		if neighbor < vertex && colors[neighbor] == color {
			return false
		}
	}
	return true
}

func canonicalize(colors []int) {
	replacements := make(map[int]int)
	next := 0
	for i, color := range colors {
		replacement, ok := replacements[color]
		if !ok {
			replacement = next
			replacements[color] = next
			next++
		}
		colors[i] = replacement
	}
}
