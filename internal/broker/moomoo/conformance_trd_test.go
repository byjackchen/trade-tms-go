package moomoo

// conformance_trd_test.go is the TRADING half of the protocol conformance gate:
// it asserts the Go trading client's ENCODED Trd_* request frames are
// byte-for-byte identical to the frames the vendored Python moomoo SDK produces
// for the same request (same field values, same serial number, same PacketID).
// The reference bytes come from tmp/conformance/dump_trd_frames.py driving the
// SDK's own pack_pb_req.
//
// testdata/conformance_trd_frames.json is committed; regenerate after a proto
// change (from the trade-multi-strategies venv):
//
//	.venv/bin/python tmp/conformance/dump_trd_frames.py \
//	  > internal/broker/moomoo/testdata/conformance_trd_frames.json
//
// Each Go case here MUST build the identical protobuf message the harness builds.

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/common"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdcommon"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdgetpositionlist"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdplaceorder"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdunlocktrade"
)

// Fixed values shared with dump_trd_frames.py.
const (
	trdConfSerial uint32 = 4242
	trdConfConnID uint64 = 7
	trdConfAccID  uint64 = 281474976710656
)

func trdConfHeader(env TrdEnv, accID uint64) *trdcommon.TrdHeader {
	return &trdcommon.TrdHeader{
		TrdEnv:    proto.Int32(int32(env)),
		AccID:     proto.Uint64(accID),
		TrdMarket: proto.Int32(TrdMarketUS),
	}
}

// goTrdRequests builds the Go side of every trading conformance case. Field
// values MUST match dump_trd_frames.py exactly.
func goTrdRequests() map[string]struct {
	protoID ProtoID
	msg     proto.Message
} {
	pwd := md5.Sum([]byte("hunter2"))
	return map[string]struct {
		protoID ProtoID
		msg     proto.Message
	}{
		"Trd_PlaceOrder": {ProtoTrdPlaceOrder, &trdplaceorder.Request{C2S: &trdplaceorder.C2S{
			PacketID:    &common.PacketID{ConnID: proto.Uint64(trdConfConnID), SerialNo: proto.Uint32(trdConfSerial)},
			Header:      trdConfHeader(TrdEnvSimulate, trdConfAccID),
			TrdSide:     proto.Int32(int32(trdcommon.TrdSide_TrdSide_Buy)),
			OrderType:   proto.Int32(int32(trdcommon.OrderType_OrderType_Market)),
			Code:        proto.String("AAPL"),
			Qty:         proto.Float64(100.0),
			Price:       proto.Float64(0.0),
			SecMarket:   proto.Int32(TrdSecMarketUS),
			Remark:      proto.String("O-1"),
			TimeInForce: proto.Int32(int32(trdcommon.TimeInForce_TimeInForce_GTC)),
		}}},
		"Trd_UnlockTrade": {ProtoTrdUnlockTrade, &trdunlocktrade.Request{C2S: &trdunlocktrade.C2S{
			Unlock: proto.Bool(true),
			PwdMD5: proto.String(hex.EncodeToString(pwd[:])),
		}}},
		"Trd_GetPositionList": {ProtoTrdGetPositionList, &trdgetpositionlist.Request{C2S: &trdgetpositionlist.C2S{
			Header:       trdConfHeader(TrdEnvReal, trdConfAccID),
			RefreshCache: proto.Bool(true),
		}}},
	}
}

func loadTrdRefFrames(t *testing.T) map[string]refFrame {
	t.Helper()
	path := filepath.Join("testdata", "conformance_trd_frames.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate via tmp/conformance/dump_trd_frames.py)", path, err)
	}
	var m map[string]refFrame
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return m
}

// TestEncodeTrdFrameMatchesPythonSDK is the trading conformance gate: identical
// bytes for Trd_PlaceOrder, Trd_UnlockTrade, and Trd_GetPositionList.
func TestEncodeTrdFrameMatchesPythonSDK(t *testing.T) {
	refs := loadTrdRefFrames(t)
	cases := goTrdRequests()
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
			body, err := proto.Marshal(c.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if hex.EncodeToString(body) != ref.BodyHex {
				t.Fatalf("BODY mismatch for %s\n go: %s\n py: %s", name, hex.EncodeToString(body), ref.BodyHex)
			}
			wantBody, err := hex.DecodeString(ref.BodyHex)
			if err != nil {
				t.Fatalf("bad ref body hex: %v", err)
			}
			frame := EncodeFrame(c.protoID, ref.SerialNo, wantBody)
			if hex.EncodeToString(frame) != ref.FrameHex {
				t.Fatalf("FRAME mismatch for %s\n go: %s\n py: %s", name, hex.EncodeToString(frame), ref.FrameHex)
			}
			// Cross-check our own decoder round-trips the header + SHA-1.
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

// TestStatusMappingFaithfulToPython locks the moomoo OrderStatus -> lifecycle
// class mapping against the Python reference's dispatch sets (exec_client.py),
// so a drift from the production semantics fails the build.
func TestStatusMappingFaithfulToPython(t *testing.T) {
	cases := []struct {
		raw  trdcommon.OrderStatus
		want OrderStatusClass
	}{
		{trdcommon.OrderStatus_OrderStatus_Submitted, StatusClassAccepted},
		{trdcommon.OrderStatus_OrderStatus_Filled_Part, StatusClassFilled},
		{trdcommon.OrderStatus_OrderStatus_Filled_All, StatusClassFilled},
		{trdcommon.OrderStatus_OrderStatus_Cancelled_Part, StatusClassCanceled},
		{trdcommon.OrderStatus_OrderStatus_Cancelled_All, StatusClassCanceled},
		{trdcommon.OrderStatus_OrderStatus_Deleted, StatusClassCanceled},
		{trdcommon.OrderStatus_OrderStatus_FillCancelled, StatusClassCanceled},
		{trdcommon.OrderStatus_OrderStatus_Failed, StatusClassRejected},
		{trdcommon.OrderStatus_OrderStatus_Disabled, StatusClassRejected},
		{trdcommon.OrderStatus_OrderStatus_SubmitFailed, StatusClassRejected},
		{trdcommon.OrderStatus_OrderStatus_TimeOut, StatusClassRejected},
		{trdcommon.OrderStatus_OrderStatus_Submitting, StatusClassTransient},
		{trdcommon.OrderStatus_OrderStatus_WaitingSubmit, StatusClassTransient},
		{trdcommon.OrderStatus_OrderStatus_Cancelling_Part, StatusClassTransient},
		{trdcommon.OrderStatus_OrderStatus_Cancelling_All, StatusClassTransient},
		{trdcommon.OrderStatus_OrderStatus_Unknown, StatusClassUnknown},
	}
	for _, tc := range cases {
		if got := classifyTrdStatus(int32(tc.raw)); got != tc.want {
			t.Errorf("classifyTrdStatus(%s)=%v, want %v", MoomooName(tc.raw), got, tc.want)
		}
	}
	// Filled_All -> FILLED domain status; Filled_Part -> PARTIALLY_FILLED.
	if s, ok := DomainOrderStatus(int32(trdcommon.OrderStatus_OrderStatus_Filled_All)); !ok || s.String() != "FILLED" {
		t.Errorf("Filled_All domain status = %v ok=%v, want FILLED", s, ok)
	}
	if s, ok := DomainOrderStatus(int32(trdcommon.OrderStatus_OrderStatus_Filled_Part)); !ok || s.String() != "PARTIALLY_FILLED" {
		t.Errorf("Filled_Part domain status = %v ok=%v, want PARTIALLY_FILLED", s, ok)
	}
}

// MoomooName is a test helper exposing the symbolic status name.
func MoomooName(s trdcommon.OrderStatus) string { return TrdOrderStatusName(int32(s)) }
