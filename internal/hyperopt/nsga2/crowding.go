package nsga2

import "sort"

// sortByCrowdingDesc reorders front in place by descending crowding distance,
// matching NSGA-II boundary-front truncation (keep the most isolated points).
// Ties are broken by ascending id so truncation is deterministic across runs
// with the same seed — Optuna's _crowding_distance_sort is a stable sort by
// distance then reverse, which is order-dependent on the incoming list; pinning
// the tiebreak to id removes that hidden dependence and keeps our result
// reproducible regardless of upstream ordering.
func sortByCrowdingDesc(front []*individual, dist map[int]float64) {
	sort.SliceStable(front, func(a, b int) bool {
		da, db := dist[front[a].id], dist[front[b].id]
		if da != db {
			return da > db
		}
		return front[a].id < front[b].id
	})
}
