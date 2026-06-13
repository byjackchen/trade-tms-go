// Package api hosts the HTTP/WebSocket layer (go-chi router +
// coder/websocket) that replaces the Python reference's FastAPI app
// (src/api/): backtest result endpoints, strategy/params endpoints, live
// cockpit WebSocket fan-out from Redis streams, and health/version probes.
//
// Rules:
//   - Handlers are thin: decode, call internal services, encode. No
//     business logic in this package.
//   - Server shuts down gracefully on context cancellation (drain, then
//     close WebSockets with a proper status).
package api
