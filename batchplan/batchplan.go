// Package batchplan implements offline planning of write batches for a
// finite set of already-parsed line-protocol points.
//
// A target store typically requires that, within a single submitted
// batch, every field of a measurement carries values of a single type.
// Two points conflict when they share a measurement and a field key but
// carry different value kinds for that field; a point that lacks the
// field entirely does not conflict. String, bool, signed integer,
// unsigned integer and float kinds are treated as distinct.
//
// The planner computes the exact minimum number of conflict-free
// batches for small inputs (at most MaxPoints points) and reports which
// batch each point belongs to. Inputs larger than MaxPoints are
// rejected with ErrTooManyPoints rather than being answered with a
// non-optimal heuristic.
//
// Planning is a pure, offline operation: it never mutates field types,
// never drops points and never performs network I/O.
package batchplan

import (
	"errors"

	"github.com/influxdata/line-protocol/v2/lineprotocol"
)

// MaxPoints is the maximum number of points for which an exact minimum
// batching is computed. Larger inputs return ErrTooManyPoints.
const MaxPoints = 20

// ErrTooManyPoints is returned by Plan when the input exceeds MaxPoints.
// No approximate or heuristic answer is substituted in that case.
var ErrTooManyPoints = errors.New("batchplan: point count exceeds MaxPoints, exact minimum batching unsupported")

// Field is a single field of a parsed point: its key and value kind.
type Field struct {
	Key  string
	Kind lineprotocol.ValueKind
}

// Point is a single parsed point to be placed into a batch.
type Point struct {
	Measurement string
	Fields      []Field
}

// FieldConflict describes one field that makes two points incompatible:
// both points define the field for the same measurement but with
// different value kinds.
type FieldConflict struct {
	Measurement string
	Key         string
	Kinds       [2]lineprotocol.ValueKind
}

// Conflict is an edge of the conflict graph: a pair of point indexes
// (into the input slice) that cannot share a batch, together with every
// field responsible for the incompatibility.
type Conflict struct {
	Points [2]int
	Fields []FieldConflict
}

// Result is the outcome of a successful Plan call.
type Result struct {
	// NumBatches is the exact minimum number of conflict-free batches.
	NumBatches int
	// Assignment holds, for each input point index, the index of the
	// batch the point belongs to. Batch indexes are canonical: they are
	// numbered in order of first use when scanning points in input
	// order. Among all minimum assignments, the lexicographically
	// smallest one (by input point order) is chosen, so the result is
	// stable and depends only on the ordered input.
	Assignment []int
	// Conflicts lists every conflicting pair of points, each traceable
	// to the concrete field(s) and value kinds that clash.
	Conflicts []Conflict
}

// Plan computes the exact minimum number of conflict-free batches for
// the given points and assigns every point to a batch. It returns
// ErrTooManyPoints if len(points) > MaxPoints.
func Plan(points []Point) (Result, error) {
	if len(points) > MaxPoints {
		return Result{}, ErrTooManyPoints
	}
	n := len(points)
	if n == 0 {
		return Result{Assignment: []int{}}, nil
	}

	adj := make([][]bool, n)
	for i := range adj {
		adj[i] = make([]bool, n)
	}
	var conflicts []Conflict
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			fields := conflictingFields(points[i], points[j])
			if len(fields) == 0 {
				continue
			}
			adj[i][j] = true
			adj[j][i] = true
			conflicts = append(conflicts, Conflict{
				Points: [2]int{i, j},
				Fields: fields,
			})
		}
	}

	lower := cliqueLowerBound(adj)
	upper := greedyColorCount(adj)
	var assignment []int
	for k := lower; k <= upper; k++ {
		if sol, ok := canonicalColoring(adj, k); ok {
			assignment = sol
			break
		}
	}
	if assignment == nil {
		// Unreachable: the greedy coloring proves upper colors suffice.
		assignment = greedyColoring(adj)
	}
	return Result{
		NumBatches: maxColor(assignment) + 1,
		Assignment: assignment,
		Conflicts:  conflicts,
	}, nil
}

// conflictingFields reports every field key that both points define for
// the same measurement with different value kinds. Points with
// different measurements, or where one point lacks the field, never
// conflict.
func conflictingFields(a, b Point) []FieldConflict {
	if a.Measurement != b.Measurement {
		return nil
	}
	var out []FieldConflict
	for _, fa := range a.Fields {
		for _, fb := range b.Fields {
			if fa.Key != fb.Key || fa.Kind == fb.Kind {
				continue
			}
			dup := false
			for _, fc := range out {
				if fc.Key == fa.Key {
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, FieldConflict{
					Measurement: a.Measurement,
					Key:         fa.Key,
					Kinds:       [2]lineprotocol.ValueKind{fa.Kind, fb.Kind},
				})
			}
		}
	}
	return out
}

// canonicalColoring finds a proper k-coloring of the conflict graph.
// Vertices are assigned in input order and each vertex tries existing
// colors before introducing a new one, so the first solution found is
// the lexicographically smallest coloring with canonically numbered
// colors. This makes ties between minimum assignments resolve
// deterministically by original point index.
func canonicalColoring(adj [][]bool, k int) ([]int, bool) {
	n := len(adj)
	colors := make([]int, n)
	for i := range colors {
		colors[i] = -1
	}
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
			colors[v] = -1
		}
		return false
	}
	if assign(0) {
		return colors, true
	}
	return nil, false
}

// greedyColoring colors the graph by first-fit in vertex order. It
// provides an upper bound on the chromatic number.
func greedyColoring(adj [][]bool) []int {
	n := len(adj)
	colors := make([]int, n)
	for i := range colors {
		colors[i] = -1
	}
	for v := 0; v < n; v++ {
		c := 0
		for {
			clash := false
			for u := 0; u < v; u++ {
				if adj[v][u] && colors[u] == c {
					clash = true
					break
				}
			}
			if !clash {
				break
			}
			c++
		}
		colors[v] = c
	}
	return colors
}

func greedyColorCount(adj [][]bool) int {
	return maxColor(greedyColoring(adj)) + 1
}

// cliqueLowerBound greedily grows a clique to lower-bound the chromatic
// number, reducing the number of infeasible k-coloring attempts.
func cliqueLowerBound(adj [][]bool) int {
	n := len(adj)
	best := 0
	for start := 0; start < n; start++ {
		clique := []int{start}
		for v := 0; v < n; v++ {
			ok := true
			for _, u := range clique {
				if !adj[v][u] {
					ok = false
					break
				}
			}
			if ok {
				clique = append(clique, v)
			}
		}
		if len(clique) > best {
			best = len(clique)
		}
	}
	if best < 1 {
		best = 1
	}
	return best
}

func maxColor(colors []int) int {
	m := -1
	for _, c := range colors {
		if c > m {
			m = c
		}
	}
	return m
}
