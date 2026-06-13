package adapters

// Dependency pin (P0 scaffold): live telemetry uses Redis streams via
// redis/go-redis, mirroring the Python reference's Redis stream publishing.
// The blank import keeps the module pinned in go.mod until the Redis
// adapter lands, avoiding dependency races across parallel build phases.
import (
	_ "github.com/redis/go-redis/v9"
)
