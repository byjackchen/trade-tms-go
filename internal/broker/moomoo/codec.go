package moomoo

// codec.go provides a streaming frame reader/writer over an io.ReadWriter
// (a net.Conn in production, an in-memory pipe in tests). It is shared by the
// native client and the mock OpenD server so both speak the identical wire
// format.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// MaxBodyLen caps an accepted body length to guard against a corrupt/hostile
// length prefix forcing an unbounded allocation. moomoo K-line / quote replies
// are well under this; FutuOpenD itself fragments large history pulls.
const MaxBodyLen = 64 << 20 // 64 MiB

// Frame is a fully-read protocol frame: header plus its body bytes.
type Frame struct {
	Header Header
	Body   []byte
}

// FrameReader reads length-delimited frames from a stream. It is NOT safe for
// concurrent use; a single reader goroutine should own it.
type FrameReader struct {
	r   *bufio.Reader
	hdr [HeaderLen]byte
}

// NewFrameReader wraps r with internal buffering.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// ReadFrame blocks until a full frame is read, the stream ends, or an error
// occurs. On clean EOF it returns io.EOF. The returned body's SHA-1 is
// verified against the header (the protocol's sha20 guard); a mismatch
// returns ErrSHAMismatch with the frame discarded.
func (fr *FrameReader) ReadFrame() (Frame, error) {
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		// io.ReadFull maps a clean stream end before any byte to io.EOF and a
		// mid-header end to io.ErrUnexpectedEOF; surface both for the caller.
		return Frame{}, err
	}
	h, err := DecodeHeader(fr.hdr[:])
	if err != nil {
		return Frame{}, err
	}
	if h.BodyLen > MaxBodyLen {
		return Frame{}, fmt.Errorf("moomoo: body length %d exceeds cap %d (proto=%s)", h.BodyLen, MaxBodyLen, h.ProtoID)
	}
	body := make([]byte, h.BodyLen)
	if _, err := io.ReadFull(fr.r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	if err := h.VerifyBody(body); err != nil {
		return Frame{}, err
	}
	return Frame{Header: h, Body: body}, nil
}

// peekProtoID is a helper used by tests/diagnostics: it reads only the proto
// id field positions from a raw header slice without full validation.
func peekProtoID(hdr []byte) (ProtoID, bool) {
	if len(hdr) < 6 {
		return 0, false
	}
	return ProtoID(binary.LittleEndian.Uint32(hdr[2:6])), true
}
