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
	CloseStatusNormalClosure CloseStatus = 1000
)

// Header represents websocket frame header.
// See https://tools.ietf.org/html/rfc6455#section-5.2
type header struct {
	Fin    bool
	Rsv    byte
	OpCode opCode
	Masked bool
	Mask   [4]byte
	Length int64
}

// Codec for use in reading and writing websocket frames
type codec struct {
	rbts []byte
	wbts []byte
}

func newCodec() *codec {
	return &codec{
		rbts: make([]byte, 2, maxHeaderSize-2),
		wbts: make([]byte, maxHeaderSize),
	}
}

func newReadCodec() *codec {
	return &codec{
		rbts: make([]byte, 2, maxHeaderSize-2),
	}
}

func newWriteCodec() *codec {
	return &codec{
		wbts: make([]byte, maxHeaderSize),
	}
}

// ReadHeader reads a frame header from r.
// ported from gobwas/ws to support reuse of the allocated frame header
func (c *codec) ReadHeader(r io.Reader) (h header, err error) {
	// Prepare to hold first 2 bytes to choose size of next read.
	_, err = io.ReadFull(r, c.rbts[:2])
	if err != nil {
		return
	}

	h.Fin = c.rbts[0]&bit0 != 0
	h.Rsv = (c.rbts[0] & 0x70) >> 4
	h.OpCode = opCode(c.rbts[0] & 0x0f)

	var extra int

	if c.rbts[1]&bit0 != 0 {
		h.Masked = true
		extra += 4
	}

	length := c.rbts[1] & 0x7f
	switch {
	case length < 126:
		h.Length = int64(length)

	case length == 126:
		extra += 2

	case length == 127:
		extra += 8

	default:
		err = ErrHeaderLengthUnexpected
		return
	}

	if extra == 0 {
		return
	}

	// Increase len of bts to extra bytes need to read.
	// Overwrite first 2 bytes that was read before.
	c.rbts = c.rbts[:extra]
	_, err = io.ReadFull(r, c.rbts)
	if err != nil {
		return
	}

	switch {
	case length == 126:
		h.Length = int64(binary.BigEndian.Uint16(c.rbts[:2]))
		if h.Masked {
			copy(h.Mask[:], c.rbts[2:])
		}

	case length == 127:
		if c.rbts[0]&0x80 != 0 {
			err = ErrHeaderLengthMSB
			return
		}
		h.Length = int64(binary.BigEndian.Uint64(c.rbts[:8]))
		if h.Masked {
			copy(h.Mask[:], c.rbts[8:])
		}
	default:
		if h.Masked {
			copy(h.Mask[:], c.rbts)
		}
	}

	return
}

// WriteHeader writes header binary representation into w.
// ported from gobwas/ws to support reuse of the allocated frame header
func (c *codec) WriteHeader(w io.Writer, h header) error {
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
		return ErrHeaderLengthUnexpected
	}

	if h.Masked {
		c.wbts[1] |= bit0
		n += copy(c.wbts[n:], h.Mask[:])
	}

	_, err := w.Write(c.wbts[:n])

	return err
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
