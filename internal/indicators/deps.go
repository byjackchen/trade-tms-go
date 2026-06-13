package indicators

// Dependency pin (P0 scaffold): heavy numerics (regressions, stats used by
// hyperopt scoring and some indicators) use gonum. The blank import keeps
// the module pinned in go.mod until the indicator ports land, avoiding
// dependency races across parallel build phases.
import (
	_ "gonum.org/v1/gonum/stat"
)
