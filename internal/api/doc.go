// Package api hosts the HTTP/WebSocket layer (go-chi router +
// coder/websocket) serving the UI: data coverage and freshness, ticker
// search, dataset-sync history, job submission/inspection/cancellation,
// universe snapshots, and a WebSocket fan-out of job/sync events bridged
// from Redis pub/sub. The wire contract is docs/api.md.
//
// The Python reference's FastAPI live-cockpit endpoints (backtest dumps,
// Redis stream reads, broker proxy — docs/spec/api-ws-redis.md) mount here
// in a later build phase.
//
// Rules:
//   - Handlers are thin: decode, validate, call the store seams
//     (stores.go), encode. No business logic in this package.
//   - Every /api/* route requires the TMS_API_TOKEN bearer token;
//     /healthz and /version are public.
//   - The server shuts down gracefully on context cancellation (HTTP
//     drain, then WebSocket close with a proper status).
package api
