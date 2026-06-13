package moomoo

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeFrameRoundTrip(t *testing.T) {
	body := []byte("hello moomoo body \x00\x01\x02")
	frame := EncodeFrame(ProtoQotSub, 12345, body)

	require.Len(t, frame, HeaderLen+len(body))
	require.Equal(t, byte('F'), frame[0])
	require.Equal(t, byte('T'), frame[1])
	require.Equal(t, uint32(ProtoQotSub), binary.LittleEndian.Uint32(frame[2:6]))
	require.Equal(t, protoFmtProtobuf, frame[6])
	require.Equal(t, apiProtoVer, frame[7])
	require.Equal(t, uint32(12345), binary.LittleEndian.Uint32(frame[8:12]))
	require.Equal(t, uint32(len(body)), binary.LittleEndian.Uint32(frame[12:16]))

	sum := sha1.Sum(body)
	require.True(t, bytes.Equal(sum[:], frame[16:36]), "header SHA-1 must equal SHA-1(body)")
	for _, b := range frame[36:44] {
		require.Equal(t, byte(0), b, "reserved must be zero")
	}

	h, err := DecodeHeader(frame[:HeaderLen])
	require.NoError(t, err)
	require.Equal(t, ProtoQotSub, h.ProtoID)
	require.Equal(t, uint32(12345), h.SerialNo)
	require.Equal(t, uint32(len(body)), h.BodyLen)
	require.NoError(t, h.VerifyBody(body))
	require.ErrorIs(t, h.VerifyBody([]byte("tampered")), ErrSHAMismatch)
}

func TestDecodeHeaderErrors(t *testing.T) {
	_, err := DecodeHeader(make([]byte, HeaderLen-1))
	require.ErrorIs(t, err, ErrShortHeader)

	frame := EncodeFrame(ProtoKeepAlive, 1, []byte("x"))
	frame[0] = 'X'
	_, err = DecodeHeader(frame[:HeaderLen])
	require.ErrorIs(t, err, ErrBadMagic)
}

func TestFrameReaderStream(t *testing.T) {
	var buf bytes.Buffer
	f1 := EncodeFrame(ProtoInitConnect, 1, []byte("one"))
	f2 := EncodeFrame(ProtoQotUpdateKL, 0, []byte("two-push"))
	buf.Write(f1)
	buf.Write(f2)

	fr := NewFrameReader(&buf)
	got1, err := fr.ReadFrame()
	require.NoError(t, err)
	require.Equal(t, ProtoInitConnect, got1.Header.ProtoID)
	require.Equal(t, []byte("one"), got1.Body)

	got2, err := fr.ReadFrame()
	require.NoError(t, err)
	require.Equal(t, ProtoQotUpdateKL, got2.Header.ProtoID)
	require.Equal(t, uint32(0), got2.Header.SerialNo)
	require.Equal(t, []byte("two-push"), got2.Body)

	_, err = fr.ReadFrame()
	require.ErrorIs(t, err, io.EOF)
}

func TestFrameReaderRejectsCorruptSHA(t *testing.T) {
	frame := EncodeFrame(ProtoQotGetKL, 7, []byte("payload"))
	frame[16] ^= 0xFF // corrupt the SHA-1
	fr := NewFrameReader(bytes.NewReader(frame))
	_, err := fr.ReadFrame()
	require.ErrorIs(t, err, ErrSHAMismatch)
}

func TestFrameReaderRejectsOversizeBody(t *testing.T) {
	var hdr [HeaderLen]byte
	hdr[0], hdr[1] = 'F', 'T'
	binary.LittleEndian.PutUint32(hdr[2:6], uint32(ProtoQotGetKL))
	binary.LittleEndian.PutUint32(hdr[12:16], MaxBodyLen+1)
	fr := NewFrameReader(bytes.NewReader(hdr[:]))
	_, err := fr.ReadFrame()
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds cap")
}

func TestPeekProtoID(t *testing.T) {
	frame := EncodeFrame(ProtoGetGlobalState, 9, nil)
	id, ok := peekProtoID(frame)
	require.True(t, ok)
	require.Equal(t, ProtoGetGlobalState, id)
	_, ok = peekProtoID(frame[:3])
	require.False(t, ok)
}

func TestProtoIDString(t *testing.T) {
	require.Equal(t, "InitConnect(1001)", ProtoInitConnect.String())
	require.Equal(t, "Qot_UpdateKL(3007)", ProtoQotUpdateKL.String())
	require.Contains(t, ProtoID(9999).String(), "9999")
}
