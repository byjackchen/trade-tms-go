package api

// ws_live.go bridges the live Redis STREAMS (trader-{id}:stream:{topic}, the
// reference key shape, api-ws-redis.md §2.1/§4.1) to the WebSocket hub so the
// cockpit sees SignalIntent / StrategyState / PortfolioHealth / Watchlist /
// Position updates in real time. It tails each topic with XREAD BLOCK starting
// at "$" (only new entries; no history replay — the §4.1 default), extracts the
// entry's `payload` field (the JSON document), and broadcasts it under a WS
// envelope type per topic.
//
// Per §4.1: one frame per stream entry, body = the entry payload; a missing
// `payload` or invalid JSON is skipped with a warning (one bad publish must not
// sever the stream); a Redis read failure keeps the WS open and retries with a
// 1 s backoff. The bridge follows ctx and needs no explicit shutdown beyond hub
// close.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/publish"
)

// WS envelope types for the live streams (additive to the job/sync types).
const (
	WSTypeSignalIntent    = "signal_intent"
	WSTypeStrategyState   = "strategy_state"
	WSTypePortfolioHealth = "portfolio_health"
	WSTypeWatchlist       = "watchlist"
	WSTypePosition        = "position"
)

// liveTopic pairs a Redis stream topic with its WS envelope type.
type liveTopic struct {
	topic  string
	wsType string
}

// liveTopics is the set of live stream topics the bridge tails.
func liveTopics() []liveTopic {
	return []liveTopic{
		{publish.TopicSignalIntent, WSTypeSignalIntent},
		{publish.TopicStrategyState, WSTypeStrategyState},
		{publish.TopicPortfolioHealth, WSTypePortfolioHealth},
		{publish.TopicWatchlist, WSTypeWatchlist},
		{publish.TopicPosition(), WSTypePosition},
	}
}

// RunLiveStreamBridge tails every live stream for traderID and fans entries to
// the hub until ctx is canceled. Each topic gets its own tail goroutine (one
// XREAD BLOCK per topic; they share nothing). A nil client is a no-op (Redis-less
// deployment). It returns when ctx is canceled.
func RunLiveStreamBridge(ctx context.Context, client *redis.Client, hub *Hub, traderID string, log zerolog.Logger) {
	if client == nil || traderID == "" {
		return
	}
	blog := log.With().Str("component", "ws-live-bridge").Str("trader_id", traderID).Logger()
	topics := liveTopics()
	done := make(chan struct{}, len(topics))
	for _, lt := range topics {
		go func(lt liveTopic) {
			defer func() { done <- struct{}{} }()
			tailStream(ctx, client, hub, traderID, lt, blog)
		}(lt)
	}
	for range topics {
		<-done
	}
	blog.Info().Msg("live stream bridge stopped")
}

// tailStream tails one topic's stream, broadcasting each entry's payload.
func tailStream(ctx context.Context, client *redis.Client, hub *Hub, traderID string, lt liveTopic, log zerolog.Logger) {
	key := publish.StreamKey(traderID, lt.topic)
	lastID := "$" // only new entries (no history replay; §4.1 default)
	consecutiveErr := 0
	for {
		if ctx.Err() != nil {
			return
		}
		res, err := client.XRead(ctx, &redis.XReadArgs{
			Streams: []string{key, lastID},
			Block:   time.Second, // BLOCK 1000ms then loop (idle steady state)
			Count:   64,
		}).Result()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if err == redis.Nil {
				consecutiveErr = 0
				continue // no new entries within the block window
			}
			// Redis read failure: keep tailing (WS stays open), backoff 1s. First
			// failure logs at error; subsequent at warn (no traceback spam, §4.1).
			if consecutiveErr == 0 {
				log.Error().Err(err).Str("key", key).Msg("live stream read failed; retrying")
			} else {
				log.Warn().Err(err).Str("key", key).Msg("live stream read still failing")
			}
			consecutiveErr++
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		consecutiveErr = 0
		for _, stream := range res {
			for _, msg := range stream.Messages {
				lastID = msg.ID
				broadcastEntry(hub, lt, msg.Values, key, log)
			}
		}
	}
}

// broadcastEntry extracts the entry's `payload` JSON and broadcasts it. A
// missing payload field or invalid JSON is skipped with a warning (§2.2/§4.1).
func broadcastEntry(hub *Hub, lt liveTopic, values map[string]any, key string, log zerolog.Logger) {
	raw, ok := values["payload"]
	if !ok {
		log.Warn().Str("key", key).Msg("live stream entry missing payload field; skipping")
		return
	}
	payloadStr, ok := raw.(string)
	if !ok {
		log.Warn().Str("key", key).Msg("live stream payload not a string; skipping")
		return
	}
	payload := json.RawMessage(payloadStr)
	if !json.Valid(payload) {
		log.Warn().Str("key", key).Msg("live stream payload not valid JSON; skipping")
		return
	}
	hub.Broadcast(lt.wsType, payload)
}
