package wsev

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func maskedFrame(op opCode, fin bool, payload []byte) []byte {
	mask := [4]byte{1, 2, 3, 4}

	hd, _ := newCodec().BuildHeader(header{
		OpCode: op,
		Fin:    fin,
		Masked: true,
		Mask:   mask,
		Length: int64(len(payload)),
	})

	frame := append([]byte{}, hd...)
	masked := append([]byte{}, payload...)
	cipher(masked, mask, 0)

	return append(frame, masked...)
}

func TestPongDoesNotInterleavePartialFrame(t *testing.T) {
	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				conn.Write(bytes.Repeat([]byte{'A'}, 100))
			},
		},
		WithWriteBufferSize(64),
		WithWriteBufferDeadline(500*time.Millisecond),
		WithReadDeadline(5*time.Second),
	)

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	conn, rb := testconn(t, address)
	defer conn.Close()

	time.Sleep(100 * time.Millisecond)

	_, err := conn.Write(maskedFrame(opPing, true, nil))
	RequireNil(t, err)

	got := make([]byte, 104)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.ReadFull(rb, got)
	RequireNil(t, err)

	want := append([]byte{0x82, 100}, bytes.Repeat([]byte{'A'}, 100)...)
	want = append(want, 0x8a, 0x00)

	RequireEqual(t, want, got)
}

func TestCloseImmediatelyDoesNotInterleavePartialFrame(t *testing.T) {
	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				conn.Write(bytes.Repeat([]byte{'A'}, 100))
				conn.CloseImmediatelyWith(CloseStatusGoingAway, "", false)
			},
		},
		WithWriteBufferSize(64),
		WithWriteBufferDeadline(500*time.Millisecond),
		WithReadDeadline(5*time.Second),
	)

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	conn, rb := testconn(t, address)
	defer conn.Close()

	got := make([]byte, 106)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := io.ReadFull(rb, got)
	RequireNil(t, err)

	want := append([]byte{0x82, 100}, bytes.Repeat([]byte{'A'}, 100)...)
	want = append(want, 0x88, 0x02, 0x03, 0xe9)

	RequireEqual(t, want, got)
}
