package wsev

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ------------------------------------------------------------------------------ //
// this file is derrived from parts of the excellent library github.com/gobwas/ws //
// with some parts that are modified to support reuse of the header buffers       //
// ------------------------------------------------------------------------------ //

var (
	ErrHeaderLengthMSB        = fmt.Errorf("header error: the most significant bit must be 0")
	ErrHeaderLengthUnexpected = fmt.Errorf("header error: unexpected payload length bits")
)

const (
	maxHeaderSize = 14
	minHeaderSize = 2
	bit0          = 0x80
	len7          = int64(125)
	len16         = int64(^(uint16(0)))
	len64         = int64(^(uint64(0)) >> 1)
)

// opCode represents operation code.
type opCode byte

// Operation codes defined by specification.
// See https://tools.ietf.org/html/rfc6455#section-5.2
const (
	opContinuation opCode = 0x0
	opText         opCode = 0x1
	opBinary       opCode = 0x2
	opClose        opCode = 0x8
	opPing         opCode = 0x9
	opPong         opCode = 0xa
)

type CloseStatus uint64

const (
	CloseStatusNormalClosure           CloseStatus = 1000
	CloseStatusGoingAway               CloseStatus = 1001
	CloseStatusProtocolError           CloseStatus = 1002
	CloseStatusUnsupportedData         CloseStatus = 1003
	CloseStatusNoStatusReceived        CloseStatus = 1005
	CloseStatusAbnormalClosure         CloseStatus = 1006
	CloseStatusInvalidFramePayloadData CloseStatus = 1007
	CloseStatusPolicyViolation         CloseStatus = 1008
	CloseStatusMessageTooBig           CloseStatus = 1009
	CloseStatusMandatoryExtension      CloseStatus = 1010
	CloseStatusInternalServerErr       CloseStatus = 1011
	CloseStatusServiceRestart          CloseStatus = 1012
	CloseStatusTryAgainLater           CloseStatus = 1013
	CloseStatusTLSHandshake            CloseStatus = 1015
)

var validCloseStatus = map[CloseStatus]struct{}{
	CloseStatusNormalClosure:           {},
	CloseStatusGoingAway:               {},
	CloseStatusProtocolError:           {},
	CloseStatusUnsupportedData:         {},
	CloseStatusInvalidFramePayloadData: {},
	CloseStatusPolicyViolation:         {},
	CloseStatusMessageTooBig:           {},
	CloseStatusMandatoryExtension:      {},
	CloseStatusInternalServerErr:       {},
	CloseStatusServiceRestart:          {},
	CloseStatusTryAgainLater:           {},
	CloseStatusTLSHandshake:            {},
}

func init() {
	for cs := CloseStatus(3000); cs < CloseStatus(5000); cs++ {
		validCloseStatus[cs] = struct{}{}
	}
}

// Header represents websocket frame header.
// See https://tools.ietf.org/html/rfc6455#section-5.2
type header struct {
	Fin       bool
	Rsv       byte
	OpCode    opCode
	Masked    bool
	Mask      [4]byte
	Length    int64
	remaining int
}

func (h *header) isControl() bool {
	return h.OpCode == opClose || h.OpCode == opPing || h.OpCode == opPong
}

func (h *header) isReserved() bool {
	return h.OpCode != opContinuation && h.OpCode != opText && h.OpCode != opBinary && h.OpCode != opClose && h.OpCode != opPing && h.OpCode != opPong
}

func (h *header) reset() {
	*h = header{}
}

// Codec for use in reading and writing websocket frames
type codec struct {
	wbts []byte
}

func newCodec() *codec {
	return &codec{
		wbts: make([]byte, maxHeaderSize),
	}
}

func newWriteCodec() *codec {
	return &codec{
		wbts: make([]byte, maxHeaderSize),
	}
}

// ReadHeader reads a frame header from r.
// ported from gobwas/ws to support reuse of the allocated frame header
func (c *codec) ReadHeader(h *header, cb *circbuf) error {
	// check if there is an existing header
	if h.remaining > 0 {
		return nil
	}

	b, err := cb.peek(cb.buffered())
	if err != nil {
		return err
	}

	if len(b) < 2 {
		// If we don't have enough bytes in the buffer
		// to read the header, wait...
		//fmt.Println("NOT ENOUGH DATA (1)", len(b))
		return ErrDataNeeded
	}

	h.Fin = b[0]&bit0 != 0
	h.Rsv = (b[0] & 0x70) >> 4
	h.OpCode = opCode(b[0] & 0x0f)

	var extra int

	if b[1]&bit0 != 0 {
		h.Masked = true
		extra += 4
	}

	length := b[1] & 0x7f
	switch {
	case length < 126:
		h.Length = int64(length)

	case length == 126:
		extra += 2

	case length == 127:
		extra += 8

	default:
		return ErrHeaderLengthUnexpected
	}

	if extra == 0 {
		h.remaining = int(h.Length)
		return cb.advanceRead(2)
	}

	if len(b) < 2+extra {
		// We don't have enough data buffered to read the extra header bytes
		//fmt.Println("NOT ENOUGH DATA (2)", len(b))
		return ErrDataNeeded
	}

	switch {
	case length == 126:
		h.Length = int64(binary.BigEndian.Uint16(b[2:4]))
		if h.Masked {
			copy(h.Mask[:], b[4:])
		}

	case length == 127:
		if b[2]&0x80 != 0 {
			return ErrHeaderLengthMSB
		}
		h.Length = int64(binary.BigEndian.Uint64(b[2:10]))
		if h.Masked {
			copy(h.Mask[:], b[10:])
		}
	default:
		if h.Masked {
			copy(h.Mask[:], b[2:])
		}
	}

	h.remaining = int(h.Length)
	return cb.advanceRead(2 + extra)
}

// WriteHeader writes header binary representation into w.
// ported from gobwas/ws to support reuse of the allocated frame header
func (c *codec) WriteHeader(w io.Writer, h header) error {
	hd, err := c.BuildHeader(h)
	if err != nil {
		return err
	}

	_, err = w.Write(hd)

	return err
}

func (c *codec) BuildHeader(h header) ([]byte, error) {
	// reset the first byte
	c.wbts[0] = 0

	if h.Fin {
		c.wbts[0] |= bit0
	}
	c.wbts[0] |= h.Rsv << 4
	c.wbts[0] |= byte(h.OpCode)

	var n int
	switch {
	case h.Length <= len7:
		c.wbts[1] = byte(h.Length)
		n = 2

	case h.Length <= len16:
		c.wbts[1] = 126
		binary.BigEndian.PutUint16(c.wbts[2:4], uint16(h.Length))
		n = 4

	case h.Length <= len64:
		c.wbts[1] = 127
		binary.BigEndian.PutUint64(c.wbts[2:10], uint64(h.Length))
		n = 10

	default:
		return nil, ErrHeaderLengthUnexpected
	}

	if h.Masked {
		c.wbts[1] |= bit0
		n += copy(c.wbts[n:], h.Mask[:])
	}

	return c.wbts[:n], nil
}

// Cipher applies XOR cipher to the payload using mask.
// Offset is used to cipher chunked data (e.g. in io.Reader implementations).
//
// To convert masked data into unmasked data, or vice versa, the following
// algorithm is applied.  The same algorithm applies regardless of the
// direction of the translation, e.g., the same steps are applied to
// mask the data as to unmask the data.
func cipher(payload []byte, mask [4]byte, offset int) {
	n := len(payload)
	if n < 8 {
		for i := 0; i < n; i++ {
			payload[i] ^= mask[(offset+i)%4]
		}
		return
	}

	// Calculate position in mask due to previously processed bytes number.
	mpos := offset % 4
	// Count number of bytes will processed one by one from the beginning of payload.
	ln := remain[mpos]
	// Count number of bytes will processed one by one from the end of payload.
	// This is done to process payload by 8 bytes in each iteration of main loop.
	rn := (n - ln) % 8

	for i := 0; i < ln; i++ {
		payload[i] ^= mask[(mpos+i)%4]
	}
	for i := n - rn; i < n; i++ {
		payload[i] ^= mask[(mpos+i)%4]
	}

	// NOTE: we use here binary.LittleEndian regardless of what is real
	// endianess on machine is. To do so, we have to use binary.LittleEndian in
	// the masking loop below as well.
	var (
		m  = binary.LittleEndian.Uint32(mask[:])
		m2 = uint64(m)<<32 | uint64(m)
	)
	// Skip already processed right part.
	// Get number of uint64 parts remaining to process.
	n = (n - ln - rn) >> 3
	for i := 0; i < n; i++ {
		var (
			j     = ln + (i << 3)
			chunk = payload[j : j+8]
		)
		p := binary.LittleEndian.Uint64(chunk)
		p = p ^ m2
		binary.LittleEndian.PutUint64(chunk, p)
	}
}

// remain maps position in masking key [0,4) to number
// of bytes that need to be processed manually inside Cipher().
var remain = [4]int{0, 3, 2, 1}
