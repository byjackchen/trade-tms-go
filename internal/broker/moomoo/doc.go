// Package moomoo is a native Go client for the moomoo / FutuOpenD gateway and
// the home of a protocol-faithful in-repo mock OpenD server (subpackage mock).
//
// It speaks the OpenD wire protocol directly, implementing
// both the MARKET-DATA + SESSION surface and the TRADING (Trd_*) surface:
//
//	InitConnect (1001), GetGlobalState (1002), KeepAlive (1004),
//	Qot_Sub (3001), Qot_RegQotPush (3002), Qot_GetSubInfo (3003),
//	Qot_GetBasicQot (3004), Qot_GetKL (3006), Qot_UpdateKL (3007, push),
//	Qot_RequestHistoryKL (3103);
//	Trd_GetAccList (2001), Trd_UnlockTrade (2005), Trd_GetFunds (2101),
//	Trd_GetPositionList (2102), Trd_GetOrderList (2201), Trd_PlaceOrder (2202),
//	Trd_UpdateOrder (2208, push), Trd_GetOrderFillList (2211),
//	Trd_UpdateOrderFill (2218, push).
//
// Trading is implemented in trd_client.go / trd_convert.go (place-order,
// unlock-trade, funds/positions/orders queries and the order/fill push
// handling); modify/cancel-order (Trd_ModifyOrder, 2205) is not wired and its
// generated binding is intentionally absent from the regenerated set.
//
// # Wire protocol
//
// The framing follows the moomoo OpenD wire protocol EXACTLY: a 44-byte
// little-endian header (magic 'FT', protoID uint32, protoFmtType uint8=0
// Protobuf, protoVer uint8=0, serialNo uint32, bodyLen uint32, SHA-1(body)
// [20]byte, reserved [8]byte) followed by the protobuf body. Encryption is off
// (localhost, no RSA key file), the OpenD default path. The protocol
// conformance test (conformance_test.go) asserts byte-for-byte agreement with
// the reference encoder; the reply test asserts the decoder parses OpenD output.
//
// # Generated code & .proto vendoring
//
//   - internal/broker/moomoo/proto/ — the vendored .proto subset (go_package
//     rewritten to this module). Source of truth.
//   - internal/broker/moomoo/pb/<pkg>/ — protoc-gen-go output. Regenerate via
//     the command in docs/runbooks/trade-smoke.md after editing the .proto.
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
