package moomoo

// protocol.go replicates the moomoo / FutuOpenD wire framing EXACTLY as the
// vendored Python SDK does it, so that bytes produced here are
// byte-for-byte identical to the bytes the Python SDK puts on the wire.
//
// AUTHORITATIVE SOURCE (vendored, read-only):
//   .venv/.../moomoo/common/constant.py     — MESSAGE_HEAD_FMT, API_PROTO_VER, ProtoFMT, ProtoId
//   .venv/.../moomoo/common/utils.py        — _joint_head / parse_head / parse_rsp
//
// Header layout (Python struct fmt "<1s1sI2B2I20s8s", little-endian):
//
//	offset size field
//	0      1    magic[0] = 'F'
//	1      1    magic[1] = 'T'
//	2      4    protoID            uint32
//	6      1    protoFmtType       uint8   (0 = Protobuf, 1 = JSON)
//	7      1    protoVer           uint8   (API_PROTO_VER = 0)
//	8      4    serialNo           uint32
//	12     4    bodyLen            uint32
//	16     20   bodySHA1           [20]byte (SHA-1 of the *body bytes*)
//	36     8    reserved           [8]byte  (all zero)
//	44     ...  body               protobuf-serialized message
//
// Total header length = 44 bytes.
//
// Encryption: for a localhost OpenD with no RSA key file configured
// (SysConfig.INIT_RSA_FILE == '') the SDK sends ALL bodies, including
// InitConnect, in the clear. P5 targets localhost only, so this package
// implements the unencrypted path; the SHA-1 is still computed over the
// (cleartext) body, exactly as the SDK does.

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
)

// Wire framing constants, mirroring constant.py.
const (
	// HeaderLen is the fixed on-wire header size in bytes (struct.calcsize of
	// MESSAGE_HEAD_FMT == 44).
	HeaderLen = 44

	// protoFmtProtobuf is ProtoFMT.Protobuf (constant.py:220 ff). P5 always
	// uses protobuf; JSON (=1) is defined for completeness but unused.
	protoFmtProtobuf uint8 = 0
	protoFmtJSON     uint8 = 1

	// apiProtoVer is API_PROTO_VER (constant.py:237) — always 0.
	apiProtoVer uint8 = 0
)

// magic is the two-byte frame prefix b'F' b'T'.
var magic = [2]byte{'F', 'T'}

// ProtoID enumerates the moomoo protocol command ids used by P5
// (market-data + session only). Values are copied verbatim from
// constant.py's ProtoId class. Trd_* (trading) ids are intentionally
// absent — trading is deferred to P6.
type ProtoID uint32

const (
	ProtoInitConnect    ProtoID = 1001 // session handshake
	ProtoGetGlobalState ProtoID = 1002 // market/server state
	ProtoKeepAlive      ProtoID = 1004 // heartbeat keep-alive

	ProtoQotSub              ProtoID = 3001 // subscribe / unsubscribe
	ProtoQotRegQotPush       ProtoID = 3002 // register / unregister push on this conn
	ProtoQotGetSubInfo       ProtoID = 3003 // query subscription quota/info
	ProtoQotGetBasicQot      ProtoID = 3004 // pull basic quotes
	ProtoQotGetKL            ProtoID = 3006 // pull cached K-line
	ProtoQotUpdateKL         ProtoID = 3007 // PUSH: real-time K-line
	ProtoQotRequestHistoryKL ProtoID = 3103 // pull historical K-line (paged)
)

// String renders the protocol id with its symbolic name where known, for
// structured logs.
func (p ProtoID) String() string {
	switch p {
	case ProtoInitConnect:
		return "InitConnect(1001)"
	case ProtoGetGlobalState:
		return "GetGlobalState(1002)"
	case ProtoKeepAlive:
		return "KeepAlive(1004)"
	case ProtoQotSub:
		return "Qot_Sub(3001)"
	case ProtoQotRegQotPush:
		return "Qot_RegQotPush(3002)"
	case ProtoQotGetSubInfo:
		return "Qot_GetSubInfo(3003)"
	case ProtoQotGetBasicQot:
		return "Qot_GetBasicQot(3004)"
	case ProtoQotGetKL:
		return "Qot_GetKL(3006)"
	case ProtoQotUpdateKL:
		return "Qot_UpdateKL(3007)"
	case ProtoQotRequestHistoryKL:
		return "Qot_RequestHistoryKL(3103)"
	default:
		return fmt.Sprintf("Proto(%d)", uint32(p))
	}
}

// Header is the decoded 44-byte frame header.
type Header struct {
	ProtoID  ProtoID
	ProtoFmt uint8
	ProtoVer uint8
	SerialNo uint32
	BodyLen  uint32
	BodySHA1 [20]byte
	Reserved [8]byte
}

// ErrShortHeader is returned when fewer than HeaderLen bytes are available.
var ErrShortHeader = errors.New("moomoo: short header (< 44 bytes)")

// ErrBadMagic is returned when the frame does not begin with 'F”T'.
var ErrBadMagic = errors.New("moomoo: bad frame magic (expected 'FT')")

// ErrSHAMismatch is returned when a received body's SHA-1 does not match the
// header — mirrors decrypt_rsp_body's "check sha error" guard.
var ErrSHAMismatch = errors.New("moomoo: body SHA-1 mismatch")

// EncodeFrame builds a complete on-wire frame (header || body) for the given
// protocol id, serial number, and already-serialized protobuf body. It is the
// Go equivalent of utils._joint_head for the unencrypted protobuf path:
//   - sha20 = SHA-1(body)
//   - struct.pack("<1s1sI2B2I20s8s%ds", 'F','T', protoID, fmt, ver, serial,
//     bodyLen, sha20, reserve8, body)
//
// The returned slice is freshly allocated and owned by the caller.
func EncodeFrame(protoID ProtoID, serialNo uint32, body []byte) []byte {
	sum := sha1.Sum(body)
	frame := make([]byte, HeaderLen+len(body))
	frame[0] = magic[0]
	frame[1] = magic[1]
	binary.LittleEndian.PutUint32(frame[2:6], uint32(protoID))
	frame[6] = protoFmtProtobuf
	frame[7] = apiProtoVer
	binary.LittleEndian.PutUint32(frame[8:12], serialNo)
	binary.LittleEndian.PutUint32(frame[12:16], uint32(len(body)))
	copy(frame[16:36], sum[:])
	// frame[36:44] reserved — already zero.
	copy(frame[HeaderLen:], body)
	return frame
}

// DecodeHeader parses the fixed 44-byte header. It does not consume the body.
func DecodeHeader(buf []byte) (Header, error) {
	var h Header
	if len(buf) < HeaderLen {
		return h, ErrShortHeader
	}
	if buf[0] != magic[0] || buf[1] != magic[1] {
		return h, ErrBadMagic
	}
	h.ProtoID = ProtoID(binary.LittleEndian.Uint32(buf[2:6]))
	h.ProtoFmt = buf[6]
	h.ProtoVer = buf[7]
	h.SerialNo = binary.LittleEndian.Uint32(buf[8:12])
	h.BodyLen = binary.LittleEndian.Uint32(buf[12:16])
	copy(h.BodySHA1[:], buf[16:36])
	copy(h.Reserved[:], buf[36:44])
	return h, nil
}

// VerifyBody checks that body's SHA-1 matches the header (the SDK's sha20
// guard). It returns ErrSHAMismatch on a mismatch.
func (h Header) VerifyBody(body []byte) error {
	sum := sha1.Sum(body)
	if sum != h.BodySHA1 {
		return ErrSHAMismatch
	}
	return nil
}
