// Package mock implements a protocol-faithful in-repo OpenD TCP server: it
// speaks the IDENTICAL moomoo wire framing and protobuf messages as the real
// FutuOpenD, serving InitConnect / GetGlobalState / KeepAlive / Qot_Sub /
// Qot_GetSubInfo / Qot_GetBasicQot / Qot_GetKL / Qot_RequestHistoryKL from a
// pluggable BarSource (our Postgres bars or an in-memory fixture), and PUSHING
// Qot_UpdateKL on a CONTROLLABLE clock so a test can deterministically replay a
// day of stored bars as if they were live ticks.
//
// This is PERMANENT test infrastructure and the deterministic gate driver. The
// real-vs-mock switch is config (TMS_MOOMOO_ADDR): point the native client at
// this server's Addr() and it cannot tell the difference at the wire level.
//
// It is intentionally minimal but faithful: same header layout (magic 'FT',
// protoID, fmt, ver, serialNo, bodyLen, SHA-1(body), reserved), same protobuf
// request/response messages, same reply-serialNo-echo semantics, and pushes
// carry serialNo 0 (the SDK does not correlate pushes).
//
// Layer: outbound adapter / test fixture (peer of internal/broker/moomoo). It
// is a wire-level fake of an external venue; production code reaches it only via
// the same client and config switch as the real OpenD.
//
// May import: internal/domain, internal/broker/moomoo (the client + framing it
// mirrors), the generated internal/broker/moomoo/pb/* protobuf messages, and
// the standard library. It must NOT import engine, strategy, runner or publish.
package mock
