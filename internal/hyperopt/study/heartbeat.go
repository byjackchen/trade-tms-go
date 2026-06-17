package study

// heartbeat.go implements the spec §6.10 liveness heartbeat: a daemon background
// ticker (interval 20s) started right after the initial RUNNING progress write
// and cancelled on study exit. Each tick stamps ONLY last_heartbeat_at and
// updated_at (to now) on progress.json and the DB study row, preserving every
// other field byte-for-byte — so the API staleness check (§9.2) sees a fresh
// heartbeat throughout a HEALTHY run even when a single walk-forward generation
// over real bars takes many minutes (longer than the trial-boundary write
// cadence).
//
// The heartbeat and the trial-boundary progress writes race benignly: both take
// progressMu and write atomically (tmp+rename), so it is always last-write-wins,
// never torn. A corrupt/missing progress.json is left untouched (the tick
// no-ops).

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/runs"
)

// startHeartbeat launches the daemon heartbeat ticker. It returns a stop func
// that the caller MUST defer; stop blocks until the goroutine has exited, so no
// goroutine outlives Run (bounded, leak-free). The ticker fires every
// heartbeatInterval; each tick stamps last_heartbeat_at/updated_at on the
// progress file and the sink. The first tick happens one interval after start
// (the initial RUNNING write already stamped a fresh heartbeat).
func (c *Coordinator) startHeartbeat(ctx context.Context) func() {
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				c.beat(ctx)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// beat performs one heartbeat: stamp last_heartbeat_at/updated_at on progress.json
// (preserving all other fields) and on the DB study row. Errors are swallowed
// (best-effort liveness; a corrupt file is preserved verbatim).
func (c *Coordinator) beat(ctx context.Context) {
	now := c.now().UTC()
	c.stampProgressHeartbeat(now)
	if c.sink != nil {
		_ = c.sink.Heartbeat(ctx, c.studyTS, now)
	}
}

// stampProgressHeartbeat reads progress.json, updates ONLY last_heartbeat_at and
// updated_at to now, and atomically rewrites it — preserving every other field
// byte-for-byte at the JSON value level (§6.10). A missing or unparseable file
// is left untouched. Serialized with full writes under progressMu.
func (c *Coordinator) stampProgressHeartbeat(now time.Time) {
	c.progressMu.Lock()
	defer c.progressMu.Unlock()

	raw, err := os.ReadFile(progressJSONPath(c.dir))
	if err != nil {
		return // missing/unreadable: no-op, preserve
	}
	// Decode into an ordered map so we rewrite with identical key order and only
	// touch the two timestamp fields.
	var top orderedMap
	if err := json.Unmarshal(raw, &top); err != nil {
		return // unparseable: preserve verbatim
	}
	ts := isoUTC(now)
	top.set("last_heartbeat_at", jsonStr(ts))
	top.set("updated_at", jsonStr(ts))
	if err := atomicWriteJSON(progressJSONPath(c.dir), runs.Marshal(top.toPyValue())); err != nil {
		return // write failure: the previous file is intact (tmp+rename)
	}
	c.lastHeartbeat = now
}
