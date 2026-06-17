package moomoo

// conformance_reply_test.go is the INBOUND half of the conformance gate: it
// asserts the Go decoder parses frames the Python SDK ENCODED (a reply and a
// push), proving the native client correctly reads real-OpenD responses — not
// just that it produces correct requests. Reference bytes come from
// tmp/conformance/dump_replies.py (the SDK's own pack_pb_req over Response
// messages); regenerate them alongside conformance_frames.json after a proto
// change.

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/initconnect"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotrequesthistorykl"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotupdatekl"
)

func loadRefReplies(t *testing.T) map[string]refFrame {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "conformance_replies.json"))
	require.NoError(t, err)
	var m map[string]refFrame
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// decodeFrame fully validates an SDK-encoded frame using our reader path:
// header decode + SHA-1 verify.
func decodeFrame(t *testing.T, frameHex string) Frame {
	t.Helper()
	raw, err := hex.DecodeString(frameHex)
	require.NoError(t, err)
	h, err := DecodeHeader(raw[:HeaderLen])
	require.NoError(t, err)
	body := raw[HeaderLen:]
	require.NoError(t, h.VerifyBody(body), "SDK frame SHA-1 must verify with our decoder")
	return Frame{Header: h, Body: body}
}

func TestDecodeSDKInitConnectReply(t *testing.T) {
	ref := loadRefReplies(t)["InitConnectReply"]
	f := decodeFrame(t, ref.FrameHex)
	require.Equal(t, ProtoInitConnect, f.Header.ProtoID)

	var rsp initconnect.Response
	require.NoError(t, proto.Unmarshal(f.Body, &rsp))
	require.EqualValues(t, 0, rsp.GetRetType())
	require.EqualValues(t, 900, rsp.GetS2C().GetServerVer())
	require.EqualValues(t, 42, rsp.GetS2C().GetConnID())
	require.EqualValues(t, 10, rsp.GetS2C().GetKeepAliveInterval())
}

func TestDecodeSDKHistoryReply(t *testing.T) {
	ref := loadRefReplies(t)["HistoryReply"]
	f := decodeFrame(t, ref.FrameHex)
	require.Equal(t, ProtoQotRequestHistoryKL, f.Header.ProtoID)
	require.EqualValues(t, 77, f.Header.SerialNo)

	var rsp qotrequesthistorykl.Response
	require.NoError(t, proto.Unmarshal(f.Body, &rsp))
	s2c := rsp.GetS2C()
	require.Equal(t, "AAPL", s2c.GetSecurity().GetCode())
	require.Len(t, s2c.GetKlList(), 2)

	// Convert through the same BarFromKLine the client uses.
	b0, err := BarFromKLine("AAPL", qotcommon.KLType_KLType_Day, s2c.GetKlList()[0])
	require.NoError(t, err)
	require.Equal(t, "101", b0.Close.String())
	require.EqualValues(t, 1000, b0.Volume)

	b1, err := BarFromKLine("AAPL", qotcommon.KLType_KLType_Day, s2c.GetKlList()[1])
	require.NoError(t, err)
	require.Equal(t, "102.5", b1.Close.String())
}

func TestDecodeSDKUpdateKLPush(t *testing.T) {
	ref := loadRefReplies(t)["UpdateKLPush"]
	f := decodeFrame(t, ref.FrameHex)
	require.Equal(t, ProtoQotUpdateKL, f.Header.ProtoID)

	var rsp qotupdatekl.Response
	require.NoError(t, proto.Unmarshal(f.Body, &rsp))
	s2c := rsp.GetS2C()
	require.Equal(t, "AAPL", s2c.GetSecurity().GetCode())
	require.EqualValues(t, qotcommon.KLType_KLType_Day, s2c.GetKlType())
	require.Len(t, s2c.GetKlList(), 1)

	b, err := BarFromKLine("AAPL", qotcommon.KLType(s2c.GetKlType()), s2c.GetKlList()[0])
	require.NoError(t, err)
	require.Equal(t, "103", b.Close.String())
}
