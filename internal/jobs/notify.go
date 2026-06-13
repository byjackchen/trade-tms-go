package jobs

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

// DefaultEventsChannel is the Redis pub/sub channel carrying job Events for
// live UI consumption. One channel for all jobs; consumers filter by id/kind.
const DefaultEventsChannel = "tms:jobs:events"

// Event is the wire shape published on the jobs event channel (JSON).
type Event struct {
	// JobID is the tms.jobs primary key.
	JobID int64 `json:"job_id"`
	// Kind is the job's dispatch key (e.g. "data.refresh").
	Kind string `json:"kind"`
	// Event is the transition name: enqueued | claimed | progress |
	// succeeded | failed | requeued | released | canceled |
	// cancel_requested | reaped.
	Event string `json:"event"`
	// Status is the job status after the transition.
	Status Status `json:"status"`
	// Worker is the worker id involved, when applicable.
	Worker string `json:"worker,omitempty"`
	// Progress carries the latest progress object for progress events.
	Progress json.RawMessage `json:"progress,omitempty"`
	// Error carries the failure/cancel reason, when applicable.
	Error string `json:"error,omitempty"`
	// TS is the publish wall-clock time (UTC).
	TS time.Time `json:"ts"`
}

// Notifier receives job events. Implementations must be non-blocking-ish
// and must never return queue-state errors to callers: event delivery is
// strictly best-effort by contract.
type Notifier interface {
	Notify(ctx context.Context, ev Event)
}

// RedisNotifier publishes Events as JSON to a Redis pub/sub channel.
type RedisNotifier struct {
	client  *redis.Client
	channel string
	log     zerolog.Logger
	timeout time.Duration
}

// NewRedisNotifier builds a notifier over an already-connected client.
// channel "" means DefaultEventsChannel.
func NewRedisNotifier(client *redis.Client, channel string, log zerolog.Logger) *RedisNotifier {
	if channel == "" {
		channel = DefaultEventsChannel
	}
	return &RedisNotifier{
		client:  client,
		channel: channel,
		log:     log.With().Str("component", "jobs-notifier").Logger(),
		timeout: 2 * time.Second,
	}
}

// Notify publishes the event with a short independent timeout. Failures are
// logged at warn and swallowed — a Redis outage must never affect the
// durable queue. The independent timeout also lets terminal events flush
// during shutdown after the caller's context is canceled.
func (n *RedisNotifier) Notify(ctx context.Context, ev Event) {
	payload, err := json.Marshal(ev)
	if err != nil { // never expected: Event is a plain struct
		n.log.Error().Err(err).Int64("job_id", ev.JobID).Msg("marshal job event")
		return
	}
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), n.timeout)
	defer cancel()
	if err := n.client.Publish(pubCtx, n.channel, payload).Err(); err != nil {
		n.log.Warn().Err(err).
			Int64("job_id", ev.JobID).
			Str("event", ev.Event).
			Msg("publish job event failed (best-effort, continuing)")
	}
}
