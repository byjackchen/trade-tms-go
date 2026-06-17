package moomoo

// conformance_test.go is the PROTOCOL CONFORMANCE GATE: it asserts the Go
// client's ENCODED request frames are byte-for-byte identical to the frames the
// upstream moomoo OpenD wire protocol expects for the same request (same field
// values, same serial number). The golden bytes are captured directly from the
// upstream SDK's own pack_pb_req (so the header layout, SHA-1, and packing are
// the protocol's, not a reimplementation).
//
// testdata/conformance_frames.json is committed; the goldens are captured by
// driving the upstream SDK's pack_pb_req and written to
// internal/broker/moomoo/testdata/conformance_frames.json.
//
// Each case here MUST build the identical protobuf message the goldens encode.

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/getglobalstate"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/initconnect"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/keepalive"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotgetbasicqot"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotgetkl"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotrequesthistorykl"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotsub"
)

const conformanceSerial uint32 = 4242

type refFrame struct {
	ProtoID  uint32 `json:"proto_id"`
	SerialNo uint32 `json:"serial_no"`
	BodyHex  string `json:"body_hex"`
	FrameHex string `json:"frame_hex"`
}

func secFor(code string) *qotcommon.Security {
	return &qotcommon.Security{
		Market: proto.Int32(int32(qotcommon.QotMarket_QotMarket_US_Security)),
		Code:   proto.String(code),
	}
}

// goRequests builds the Go side of every conformance case. Field values MUST
// match dump_frames.py exactly.
func goRequests() map[string]struct {
	protoID ProtoID
	msg     proto.Message
} {
	const (
		rehabFwd = int32(qotcommon.RehabType_RehabType_Forward)
		klDay    = int32(qotcommon.KLType_KLType_Day)
		subKLDay = int32(qotcommon.SubType_SubType_KL_Day)
	)
	return map[string]struct {
		protoID ProtoID
		msg     proto.Message
	}{
		"InitConnect": {ProtoInitConnect, &initconnect.Request{C2S: &initconnect.C2S{
			ClientVer:           proto.Int32(100),
			ClientID:            proto.String("tms-go-fixed"),
			RecvNotify:          proto.Bool(true),
			PushProtoFmt:        proto.Int32(0),
			ProgrammingLanguage: proto.String("Go"),
		}}},
		"KeepAlive": {ProtoKeepAlive, &keepalive.Request{C2S: &keepalive.C2S{
			Time: proto.Int64(1700000000),
		}}},
		"GetGlobalState": {ProtoGetGlobalState, &getglobalstate.Request{C2S: &getglobalstate.C2S{
			UserID: proto.Uint64(0),
		}}},
		"Qot_Sub": {ProtoQotSub, &qotsub.Request{C2S: &qotsub.C2S{
			SecurityList:         []*qotcommon.Security{secFor("AAPL"), secFor("MSFT")},
			SubTypeList:          []int32{subKLDay},
			IsSubOrUnSub:         proto.Bool(true),
			IsRegOrUnRegPush:     proto.Bool(true),
			RegPushRehabTypeList: []int32{rehabFwd},
			IsFirstPush:          proto.Bool(true),
		}}},
		"Qot_GetBasicQot": {ProtoQotGetBasicQot, &qotgetbasicqot.Request{C2S: &qotgetbasicqot.C2S{
			SecurityList: []*qotcommon.Security{secFor("AAPL")},
		}}},
		"Qot_GetKL": {ProtoQotGetKL, &qotgetkl.Request{C2S: &qotgetkl.C2S{
			RehabType: proto.Int32(rehabFwd),
			KlType:    proto.Int32(klDay),
			Security:  secFor("AAPL"),
			ReqNum:    proto.Int32(100),
		}}},
		"Qot_RequestHistoryKL": {ProtoQotRequestHistoryKL, &qotrequesthistorykl.Request{C2S: &qotrequesthistorykl.C2S{
			RehabType: proto.Int32(rehabFwd),
			KlType:    proto.Int32(klDay),
			Security:  secFor("AAPL"),
			BeginTime: proto.String("2024-01-01"),
			EndTime:   proto.String("2024-01-31"),
		}}},
	}
}

func loadRefFrames(t *testing.T) map[string]refFrame {
	t.Helper()
	path := filepath.Join("testdata", "conformance_frames.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate via tmp/conformance/dump_frames.py)", path, err)
	}
	var m map[string]refFrame
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return m
}

// TestEncodeFrameMatchesProtocol is the conformance gate: identical bytes.
func TestEncodeFrameMatchesProtocol(t *testing.T) {
	refs := loadRefFrames(t)
	cases := goRequests()
	if len(refs) != len(cases) {
		t.Fatalf("case count mismatch: %d ref frames vs %d go cases", len(refs), len(cases))
	}

	for name, c := range cases {
		name, c := name, c
		t.Run(name, func(t *testing.T) {
			ref, ok := refs[name]
			if !ok {
				t.Fatalf("no reference frame for %q", name)
			}
			if ref.ProtoID != uint32(c.protoID) {
				t.Fatalf("proto id: ref %d vs go %d", ref.ProtoID, uint32(c.protoID))
			}

			// 1) Body bytes must match (deterministic proto serialization).
			body, err := proto.Marshal(c.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			wantBody, err := hex.DecodeString(ref.BodyHex)
			if err != nil {
				t.Fatalf("bad ref body hex: %v", err)
			}
			if hex.EncodeToString(body) != ref.BodyHex {
				t.Fatalf("BODY mismatch for %s\n go:  %s\n py:  %s", name, hex.EncodeToString(body), ref.BodyHex)
			}

			// 2) Full frame bytes (header + body) must match, using the same
			//    serial number the harness pinned.
			frame := EncodeFrame(c.protoID, ref.SerialNo, wantBody)
			if hex.EncodeToString(frame) != ref.FrameHex {
				t.Fatalf("FRAME mismatch for %s\n go:  %s\n py:  %s", name, hex.EncodeToString(frame), ref.FrameHex)
			}

			// 3) Cross-check: our own decoder round-trips the header we emitted.
			h, err := DecodeHeader(frame[:HeaderLen])
			if err != nil {
				t.Fatalf("decode our frame header: %v", err)
			}
			if h.ProtoID != c.protoID || h.SerialNo != ref.SerialNo || int(h.BodyLen) != len(wantBody) {
				t.Fatalf("decoded header mismatch: %+v", h)
			}
			if err := h.VerifyBody(wantBody); err != nil {
				t.Fatalf("our SHA-1 does not verify: %v", err)
			}
		})
	}
}
