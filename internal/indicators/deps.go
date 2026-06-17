package indicators

// Dependency pin: the indicators themselves are stdlib-only (math) to
// guarantee bit-for-bit numerical determinism, but gonum/stat is still
// consumed by hyperopt scoring elsewhere. This blank import keeps gonum pinned
// in go.mod/go.sum so `go mod tidy` from a parallel build phase cannot drop it
// out from under those callers (and to avoid dependency races across phases).
import (
	_ "gonum.org/v1/gonum/stat"
)
