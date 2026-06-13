package nsga2

// dominance.go implements the multi-objective primitives: Pareto dominance,
// fast-non-dominated-sort, and crowding-distance — matching the semantics of
// Optuna's optuna.study._multi_objective and
// nsgaii._elite_population_selection_strategy.
//
// All objectives are internally normalized to MINIMIZATION (a maximize
// objective is negated, exactly as Optuna multiplies maximize directions by
// -1 in _rank_population). Downstream code reasons purely about "loss" values
// where smaller is better.

import (
	"math"
	"sort"
)

// individual is one population member: its genome, the decoded params, the
// objective values as REPORTED by the evaluator (in user direction), and the
// internal minimization "loss" vector. id is a stable, monotonically assigned
// identifier used for deterministic tie-breaking and crowding bookkeeping.
type individual struct {
	id       int
	gen      int // generation in which this individual was created
	genome   Genome
	params   Params
	values   []float64 // objective values as returned by the evaluator
	loss     []float64 // normalized-to-minimize values
	rank     int       // non-domination rank (0 = best front), filled by survival
	crowd    float64   // crowding distance, filled during truncation
	evalOK   bool      // false if evaluation failed (excluded from population)
	evalErr  error
	feasible bool // reserved for constraint handling; true when no constraint set
}

// toLoss converts user-direction values to minimization losses for the given
// objective specs.
func toLoss(values []float64, objs []ObjectiveSpec) []float64 {
	out := make([]float64, len(values))
	for i := range values {
		if i < len(objs) && objs[i].Maximize {
			out[i] = -values[i]
		} else {
			out[i] = values[i]
		}
	}
	return out
}

// dominatesLoss reports whether loss-vector a dominates b (minimization):
// a is no worse in every objective and strictly better in at least one.
// NaN losses never participate in domination (treated as incomparable-worse),
// matching the practical behavior that failed/degenerate trials do not dominate.
func dominatesLoss(a, b []float64) bool {
	betterInOne := false
	for i := range a {
		av, bv := a[i], b[i]
		if math.IsNaN(av) || math.IsNaN(bv) {
			return false
		}
		if av > bv {
			return false // worse in this objective => cannot dominate
		}
		if av < bv {
			betterInOne = true
		}
	}
	return betterInOne
}

// fastNonDominatedSort partitions individuals into fronts by non-domination
// rank using the classic Deb et al. O(M·N²) algorithm. Front 0 is the
// Pareto-optimal set. The returned fronts preserve, within each front, the
// input order of pop (which callers keep stable by id) so the overall procedure
// is deterministic.
func fastNonDominatedSort(pop []*individual) [][]*individual {
	n := len(pop)
	if n == 0 {
		return nil
	}
	dominatedBy := make([][]int, n) // dominatedBy[i] = indices p dominates
	dominationCount := make([]int, n)
	var fronts [][]*individual
	var first []*individual

	for p := 0; p < n; p++ {
		for q := 0; q < n; q++ {
			if p == q {
				continue
			}
			if dominatesLoss(pop[p].loss, pop[q].loss) {
				dominatedBy[p] = append(dominatedBy[p], q)
			} else if dominatesLoss(pop[q].loss, pop[p].loss) {
				dominationCount[p]++
			}
		}
		if dominationCount[p] == 0 {
			pop[p].rank = 0
			first = append(first, pop[p])
		}
	}
	fronts = append(fronts, first)

	// Track current-front membership by index for the count-decrement step.
	curIdx := make([]int, 0, len(first))
	for i := 0; i < n; i++ {
		if dominationCount[i] == 0 {
			curIdx = append(curIdx, i)
		}
	}
	rank := 0
	for len(curIdx) > 0 {
		var nextIdx []int
		var next []*individual
		for _, p := range curIdx {
			for _, q := range dominatedBy[p] {
				dominationCount[q]--
				if dominationCount[q] == 0 {
					pop[q].rank = rank + 1
					nextIdx = append(nextIdx, q)
					next = append(next, pop[q])
				}
			}
		}
		rank++
		if len(next) == 0 {
			break
		}
		fronts = append(fronts, next)
		curIdx = nextIdx
	}
	return fronts
}

// crowdingDistances computes the NSGA-II crowding distance for each individual
// in front, returning a map id->distance. It replicates Optuna's
// _calc_crowding_distance edge-case handling:
//
//   - per-objective: sort by that objective; if all equal, the dimension
//     contributes nothing;
//   - boundary points get the gap to their single finite neighbour (the -inf /
//     +inf sentinels collapse: inf-inf and (-inf)-(-inf) count as 0);
//   - each dimension's gap is normalized by (v_max - v_min) of the finite
//     values; a non-positive width is replaced by 1.0.
//
// Distances are accumulated across objectives. Larger distance = more isolated
// = preferred during truncation.
func crowdingDistances(front []*individual) map[int]float64 {
	dist := make(map[int]float64, len(front))
	if len(front) == 0 {
		return dist
	}
	for _, ind := range front {
		dist[ind.id] = 0
	}
	m := len(front[0].loss)
	// Work on a copy of the slice so we can reorder per objective without
	// disturbing the caller's ordering.
	work := make([]*individual, len(front))
	copy(work, front)

	for obj := 0; obj < m; obj++ {
		sort.SliceStable(work, func(a, b int) bool {
			return work[a].loss[obj] < work[b].loss[obj]
		})
		lo := work[0].loss[obj]
		hi := work[len(work)-1].loss[obj]
		if lo == hi {
			continue // all equal in this dimension
		}
		// vs = [-inf] + losses + [+inf]; finite min/max are lo/hi here.
		width := hi - lo
		if width <= 0 {
			width = 1.0
		}
		k := len(work)
		for j := 0; j < k; j++ {
			var below, above float64
			if j == 0 {
				below = math.Inf(-1)
			} else {
				below = work[j-1].loss[obj]
			}
			if j == k-1 {
				above = math.Inf(1)
			} else {
				above = work[j+1].loss[obj]
			}
			gap := 0.0
			if below != above { // inf-inf / (-inf)-(-inf) collapse to 0
				gap = above - below
			}
			dist[work[j].id] += gap / width
		}
	}
	return dist
}
