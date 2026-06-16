package api

// system.go is the cross-cutting SYSTEM STATUS endpoint (P7 capstone): a single
// GET /api/v1/system that aggregates the health of every component the operator
// needs to see at a glance — Postgres, Redis, the moomoo data feed, active live
// sessions, the durable job-queue depth, and market-data freshness — so the UI
// System page renders the whole stack from ONE call (the "UI fully observable"
// requirement).
//
// Every datum here already lives in PG (decision: PG is the durable truth); this
// endpoint only READS and aggregates. It never blocks the UI: each probe is
// best-effort, a failed sub-probe degrades that component to "down"/"unknown"
// rather than failing the whole response (HTTP is always 200 with a status
// field, mirroring /healthz). The moomoo feed is INFERRED from the latest
// running session + health freshness (the API process holds no OpenD socket —
// that lives in tms-live), documented as such in the payload.

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/app"
)

// systemFeedFreshWindow bounds how recent a live health snapshot must be for the
// moomoo data feed to read "flowing". Beyond it a RUNNING session is reported as
// "stale" (running but no recent bars) — the same threshold the UI used before
// this endpoint centralized the inference.
const systemFeedFreshWindow = 5 * time.Minute

// SystemReader supplies the aggregate counts + freshness the system endpoint
// needs beyond the existing ping/live readers (satisfied by *apistore.PGStore). All
// methods are best-effort point-in-time reads.
type SystemReader interface {
	// QueueDepth returns the number of durable jobs not yet terminal, split into
	// queued (waiting for a worker) and running (claimed, in flight).
	QueueDepth(ctx context.Context) (queued, running int, err error)
	// ActiveSessions returns the count of live sessions whose status is RUNNING.
	ActiveSessions(ctx context.Context) (int, error)
	// DataFreshness returns the most recent stored daily-bar date and the most
	// recent dataset-sync completion time. Zero values mean "no data yet".
	DataFreshness(ctx context.Context) (latestBarDate string, lastSyncAt *time.Time, err error)
}

// SystemComponent is one component's health line in the system response.
type SystemComponent struct {
	// Status is "ok" | "degraded" | "down" | "unknown" | "not_configured".
	Status string `json:"status"`
	// Detail is a short human-readable elaboration (e.g. an error, a count).
	Detail string `json:"detail,omitempty"`
}

// SystemResponse is the GET /api/v1/system body: the overall rollup plus a map
// of named components, plus the structured metrics the UI surfaces directly.
type SystemResponse struct {
	// Status is the worst-of rollup: "ok" when every required component is ok,
	// "degraded" when a non-fatal component is down (Redis / feed / a failed
	// sub-probe), "down" when Postgres (the truth store) is unreachable.
	Status     string                     `json:"status"`
	Version    string                     `json:"version"`
	TS         time.Time                  `json:"ts"`
	Components map[string]SystemComponent `json:"components"`

	// Metrics carries the structured numbers behind the component lines.
	Metrics SystemMetrics `json:"metrics"`
}

// SystemMetrics is the structured numeric surface (the UI binds these directly).
type SystemMetrics struct {
	JobsQueued      int        `json:"jobs_queued"`
	JobsRunning     int        `json:"jobs_running"`
	ActiveSessions  int        `json:"active_sessions"`
	LatestBarDate   string     `json:"latest_bar_date,omitempty"`
	LastSyncAt      *time.Time `json:"last_sync_at,omitempty"`
	LiveMode        string     `json:"live_mode,omitempty"`
	LiveSessionID   *int64     `json:"live_session_id,omitempty"`
	HealthAgeSecond *float64   `json:"health_age_seconds,omitempty"`
}

// handleSystem serves GET /api/v1/system: the aggregated component health for
// the UI System page. Always HTTP 200 (degraded state is in the body), so the
// page renders red/yellow dots instead of throwing.
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.now().UTC()
	comps := make(map[string]SystemComponent, 6)
	var metrics SystemMetrics

	// --- Postgres + Redis (concurrent pings, bounded) --------------------
	var (
		wg       sync.WaitGroup
		pg, reds SystemComponent
	)
	probe := func(p PingFunc, name string) SystemComponent {
		if p == nil {
			return SystemComponent{Status: "not_configured", Detail: name + " ping not wired"}
		}
		pctx, cancel := context.WithTimeout(ctx, depPingTimeout)
		defer cancel()
		if err := p(pctx); err != nil {
			return SystemComponent{Status: "down", Detail: err.Error()}
		}
		return SystemComponent{Status: "ok", Detail: "reachable"}
	}
	wg.Add(2)
	go func() { defer wg.Done(); pg = probe(s.pingPG, "postgres") }()
	go func() { defer wg.Done(); reds = probe(s.pingRedis, "redis") }()
	wg.Wait()
	comps["postgres"] = pg
	comps["redis"] = reds

	// --- job queue depth -------------------------------------------------
	if s.system != nil {
		if q, run, err := s.system.QueueDepth(ctx); err != nil {
			comps["jobs"] = SystemComponent{Status: "unknown", Detail: err.Error()}
		} else {
			metrics.JobsQueued, metrics.JobsRunning = q, run
			comps["jobs"] = SystemComponent{
				Status: "ok",
				Detail: depthDetail(q, run),
			}
		}
		// --- data freshness ---------------------------------------------
		if barDate, syncAt, err := s.system.DataFreshness(ctx); err != nil {
			comps["data"] = SystemComponent{Status: "unknown", Detail: err.Error()}
		} else {
			metrics.LatestBarDate, metrics.LastSyncAt = barDate, syncAt
			detail := "no bars loaded"
			status := "degraded"
			if barDate != "" {
				detail = "latest bar " + barDate
				status = "ok"
			}
			comps["data"] = SystemComponent{Status: status, Detail: detail}
		}
		// --- active sessions --------------------------------------------
		if n, err := s.system.ActiveSessions(ctx); err != nil {
			comps["sessions"] = SystemComponent{Status: "unknown", Detail: err.Error()}
		} else {
			metrics.ActiveSessions = n
		}
	} else {
		comps["jobs"] = SystemComponent{Status: "not_configured"}
		comps["data"] = SystemComponent{Status: "not_configured"}
	}

	// --- moomoo data feed (inferred from latest session + health) --------
	feed, sessionComp := s.inferFeed(ctx, now, &metrics)
	comps["moomoo_feed"] = feed
	comps["sessions"] = sessionComp

	writeJSON(w, http.StatusOK, SystemResponse{
		Status:     rollup(comps),
		Version:    app.Version,
		TS:         now,
		Components: comps,
		Metrics:    metrics,
	})
}

// inferFeed derives the moomoo data-feed health from the latest live session and
// the freshness of its PortfolioHealth snapshot (the API holds no OpenD socket;
// the feed lives in tms-live, so its liveness is observed indirectly — the same
// inference the UI did client-side, now authoritative server-side). It also
// returns the live-sessions component summarizing the active session count +
// mode. metrics is enriched with the live mode / session id / health age.
func (s *Server) inferFeed(ctx context.Context, now time.Time, metrics *SystemMetrics) (feed, sessions SystemComponent) {
	sessions = SystemComponent{Status: "ok", Detail: pluralSessions(metrics.ActiveSessions)}
	if metrics.ActiveSessions == 0 {
		sessions.Status = "idle"
	}

	if s.trade == nil {
		return SystemComponent{Status: "not_configured", Detail: "no live reader"}, sessions
	}
	sess, err := s.trade.LatestSession(ctx)
	if err != nil {
		return SystemComponent{Status: "unknown", Detail: err.Error()}, sessions
	}
	if sess == nil {
		return SystemComponent{Status: "idle", Detail: "no live session"}, sessions
	}
	metrics.LiveMode = sess.Mode
	id := sess.ID
	metrics.LiveSessionID = &id
	sessions.Detail = pluralSessions(metrics.ActiveSessions) + " · latest " + sess.Status + " (" + sess.Mode + ")"

	if sess.Status != "RUNNING" {
		return SystemComponent{Status: "idle", Detail: "session " + sess.Status}, sessions
	}
	// RUNNING: infer feed liveness from the latest health snapshot's age.
	h, herr := s.trade.LatestHealth(ctx)
	if herr != nil || h == nil {
		return SystemComponent{Status: "degraded", Detail: "running — awaiting bars"}, sessions
	}
	age := now.Sub(h.TS.UTC())
	ageSec := age.Seconds()
	metrics.HealthAgeSecond = &ageSec
	if age >= 0 && age < systemFeedFreshWindow {
		return SystemComponent{Status: "ok", Detail: "data flowing"}, sessions
	}
	return SystemComponent{Status: "degraded", Detail: "running — bars stale"}, sessions
}

// rollup computes the overall status: Postgres down => "down" (the truth store
// is unreachable; the system cannot function). Any other component down/degraded
// => "degraded". Otherwise "ok". "idle"/"not_configured"/"unknown" do not by
// themselves degrade the rollup (no session running is normal; an unwired probe
// is a deployment choice).
func rollup(comps map[string]SystemComponent) string {
	if pg, ok := comps["postgres"]; ok && pg.Status == "down" {
		return "down"
	}
	for name, c := range comps {
		if name == "postgres" {
			continue
		}
		if c.Status == "down" || c.Status == "degraded" {
			return "degraded"
		}
	}
	return "ok"
}

func depthDetail(queued, running int) string {
	return itoa(queued) + " queued · " + itoa(running) + " running"
}

func pluralSessions(n int) string {
	if n == 1 {
		return "1 active session"
	}
	return itoa(n) + " active sessions"
}

// itoa is a tiny allocation-free int->string for the detail strings (avoids
// pulling strconv into the hot detail builders; n is always small + non-negative
// here).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
