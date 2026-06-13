// Package moomoo is a native Go client for the moomoo / FutuOpenD gateway and
// the home of a protocol-faithful in-repo mock OpenD server (subpackage mock).
//
// It speaks the OpenD wire protocol directly — no Python sidecar — implementing
// the P5 MARKET-DATA + SESSION surface only:
//
//	InitConnect (1001), GetGlobalState (1002), KeepAlive (1004),
//	Qot_Sub (3001), Qot_RegQotPush (3002), Qot_GetSubInfo (3003),
//	Qot_GetBasicQot (3004), Qot_GetKL (3006), Qot_UpdateKL (3007, push),
//	Qot_RequestHistoryKL (3103).
//
// Trading (Trd_*) is intentionally NOT implemented (deferred to P6).
//
// # Wire protocol
//
// The framing is replicated EXACTLY from the vendored Python SDK
// (.venv/.../moomoo/common/{constant,utils,network_manager}.py): a 44-byte
// little-endian header (magic 'FT', protoID uint32, protoFmtType uint8=0
// Protobuf, protoVer uint8=0, serialNo uint32, bodyLen uint32, SHA-1(body)
// [20]byte, reserved [8]byte) followed by the protobuf body. Encryption is off
// (localhost, no RSA key file), matching the SDK's default path. The protocol
// conformance test (conformance_test.go) asserts byte-for-byte parity with the
// SDK's own encoder; the reply test asserts the decoder parses SDK output.
//
// # Generated code & .proto vendoring
//
//   - internal/adapters/moomoo/proto/ — the vendored .proto subset (go_package
//     rewritten to this module). Source of truth.
//   - internal/adapters/moomoo/pb/<pkg>/ — protoc-gen-go output. Regenerate via
//     the command in docs/runbooks/live-smoke.md after editing the .proto.
//
// # Client
//
// Client (client.go, requests.go) is production-grade: a single reader
// goroutine demuxing replies (serialNo->waiter) from pushes (Qot_UpdateKL ->
// KLineHandler), serialized frame writes, periodic KeepAlive at the
// server-advertised interval, auto-reconnect with exponential backoff + jitter
// and re-subscribe of the prior set on reconnect, the 100-subscription cap
// (TMS_MOOMOO_MAX_SUB), full ctx cancellation, and a clean Close with no
// goroutine leaks. Bars are converted to domain.Bar at the boundary (UTC,
// fixed-point prices); secrets are never logged.
//
// # Mock OpenD (subpackage mock)
//
// mock.Server is PERMANENT test infrastructure and the deterministic gate
// driver: a TCP server speaking the identical framing + protos, serving from a
// pluggable BarSource (PGBarSource over tms.bars_daily/bars_intraday, or an
// in-memory MemBarSource) and pushing Qot_UpdateKL on a controllable clock
// (Server.PushKLine) so a test can replay a stored day of bars as live ticks.
// The real-vs-mock switch is config (TMS_MOOMOO_ADDR); the client cannot tell
// the difference at the wire level.
package moomoo
