package data

// Dependency pin (P0 scaffold): Parquet read/write of the Sharadar cache
// uses apache/arrow-go. The blank import keeps the module pinned in go.mod
// until the importers land, avoiding dependency races across parallel
// build phases.
import (
	_ "github.com/apache/arrow-go/v18/arrow"
	_ "github.com/apache/arrow-go/v18/parquet"
)
