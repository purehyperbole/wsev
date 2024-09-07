package wsev

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type closeError struct {
	Reason    string
	Status    CloseStatus
	immediate bool
	shutdown  bool
}

func (e *closeError) Error() string {
	return e.Reason
}

var (
	ErrConnectionAlreadyClosed = errors.New("connection already closed")
	ErrConnectionTimeout       = &closeError{
		Status: CloseStatusAbnormalClosure,
		Reason: "connection timeout",
	}
	ErrInvalidRSVBits error = &closeError{
		Status: CloseStatusProtocolError,
		Reason: "non-zero rsv bits not supported",
		// immediate: true,
	}
	ErrInvalidOpCode error = &closeError{
		Status: CloseStatusProtocolError,
		Reason: "invalid or reserved op code",
		// immediate: true,
	}
	ErrInvalidContinuation error = &closeError{
		Status: CloseStatusProtocolError,
		Reason: "invalid continuation",
	}
	ErrInvalidPayloadEncoding error = &closeError{
		Status:    CloseStatusInvalidFramePayloadData,
		Reason:    "close frame reason invalid",
		immediate: true,
	}
	ErrInvalidControlLength error = &closeError{
		Status:    CloseStatusProtocolError,
		Reason:    "control frame payload exceeds 125 bytes",
		immediate: true,
		shutdown:  true,
	}
	ErrInvalidCloseReason error = &closeError{
		Status:    CloseStatusProtocolError,
		Reason:    "close frame reason invalid",
		immediate: true,
		shutdown:  true,
	}
	ErrInvalidCloseEncoding error = &closeError{
		Status:    CloseStatusInvalidFramePayloadData,
		Reason:    "close frame reason invalid",
		immediate: true,
		shutdown:  true,
	}
)

type listener struct {
	handler       *Handler
	codec         *codec
	bufpool       *sync.Pool
	readbuf       *bufio.Reader
	framebuf      *bytes.Buffer
	messagebuf    *bytes.Buffer
	timerheap     heap
	timermu       sync.Mutex
	conns         sync.Map
	socket        net.Listener
	_p1           [8]uint64
	readDeadline  time.Duration
	flushDeadline time.Duration
	writebufsize  int
	counter       int
	fd            int
}

func newListener(epollFd int, handler *Handler, bufpool *sync.Pool, readDeadline, flushDeadline time.Duration, writebufsize int) *listener {
	l := &listener{
		fd:            epollFd,
		readDeadline:  readDeadline,
		flushDeadline: flushDeadline,
		writebufsize:  writebufsize,
		bufpool:       bufpool,
		codec:         newCodec(),
		readbuf:       bufio.NewReaderSize(nil, DefaultBufferSize),
		framebuf:      bytes.NewBuffer(make([]byte, 0, DefaultBufferSize)),
		messagebuf:    bytes.NewBuffer(make([]byte, 0, DefaultBufferSize)),
		handler:       handler,
	}

	go l.handleEvents()

	return l
}

func (l *listener) handleEvents() {
	events := make([]syscall.EpollEvent, 128)

	// wait for epoll to return some events
	for {
		ec, err := syscall.EpollWait(l.fd, events, 10)
		if err != nil {
			// ignore interupted syscall
			if errors.Is(err, syscall.EINTR) {
				continue
			}

			l.error(err, true)
			return
		}

		if ec < 1 {
			// use this as an opportunity to clear out old connections
			// that have timed out
			l.purgeIdle()
			continue
		}

		now := time.Now().Unix()

		for i := 0; i < ec; i++ {
			conn, ok := l.conns.Load(int(events[i].Fd))
			if !ok {
				continue
			}

			cn := conn.(*Conn)
			l.readbuf.Reset(cn)

			if events[i].Events&unix.POLLHUP > 0 {
				l.disconnect(int(events[i].Fd), cn, io.EOF)
				continue
			}

			// read until there is no more data in the read buffer
			for {
				if cn.upgrade() {
					// upgrade the connection and write upgrade negotiation
					// response directly to underlying connection
					err = acceptWs(cn.Conn, l.readbuf)
					if err != nil {
						l.disconnect(int(events[i].Fd), cn, err)
						break
					}

					if l.handler.OnConnect != nil {
						l.handler.OnConnect(cn)
					}
				}

				err = l.read(events[i].Fd, cn)
				if err != nil {
					l.disconnect(int(events[i].Fd), cn, err)
					break
				}

				if l.readbuf.Buffered() < 1 {
					break
				}
			}

			if cn.h > HeapRemoved {
				// reset the timer on the connection
				l.timermu.Lock()
				l.timerheap.decrease(cn, now)
				l.timermu.Unlock()
			}
		}
	}
}

func (l *listener) read(fd int32, conn *Conn) error {
	op, err := l.assembleFrame(conn)
	if err != nil {
		ce, ok := err.(*closeError)
		if !ok {
			return err
		}

		if conn.closed() {
			// we've already sent a closing handshake
			return nil
		}

		if ce.immediate {
			return conn.CloseImmediatelyWith(ce.Status, ce.Reason, ce.shutdown)
		}

		return conn.CloseWith(ce.Status, ce.Reason, ce.shutdown)
	}

	if conn.closed() && op != opClose {
		// if we have a non-close frame after we have started the closing handshake, skip it
		return nil
	}

	switch op {
	case opText:
		if !utf8.Valid(l.messagebuf.Bytes()) {
			return conn.CloseWith(CloseStatusInvalidFramePayloadData, "invalid utf8 payload", false)
		}

		if l.handler.OnMessage != nil {
			l.handler.OnMessage(conn, l.messagebuf.Bytes())
		} else if l.handler.OnText != nil {
			l.handler.OnText(conn, l.messagebuf.String())
		}
	case opBinary:
		if l.handler.OnMessage != nil {
			l.handler.OnMessage(conn, l.messagebuf.Bytes())
		} else if l.handler.OnBinary != nil {
			l.handler.OnBinary(conn, l.messagebuf.Bytes())
		}
	case opClose:
		if conn.closed() {
			return unix.Shutdown(int(fd), unix.SHUT_RDWR)
		} else {
			return conn.CloseWith(CloseStatusNormalClosure, "normal closure", true)
		}
	case opPing:
		if conn.closed() {
			return nil
		}

		err = l.pongWs(conn)
		if err != nil {
			return err
		}

		if l.handler.OnPing != nil {
			l.handler.OnPing(conn)
		}
	case opPong:
		if l.handler.OnPong != nil {
			l.handler.OnPong(conn)
		}
	case opContinuation:
		return nil
	}

	return nil
}

// reads a frame and outputs its payload into a frame, text or binary buffer
func (l *listener) assembleFrame(conn *Conn) (opCode, error) {
	l.framebuf.Reset()

	err := conn.SetReadDeadline(time.Now().Add(l.readDeadline))
	if err != nil {
		return 0, err
	}

	h, err := l.codec.ReadHeader(l.readbuf)
	if err != nil {
		return 0, err
	}

	err = conn.SetReadDeadline(time.Now().Add(l.readDeadline))
	if err != nil {
		return h.OpCode, err
	}

	// validate this frame after we have read data to avoid
	// epoll waking us up again to read the remaining bytes
	if h.Rsv > 0 {
		// we don't support rsv bits > 0
		return 0, l.discard(h.Length, ErrInvalidRSVBits)
	}

	if h.isReserved() {
		// reserved op code used
		return 0, l.discard(h.Length, ErrInvalidOpCode)
	}

	if h.isControl() {
		if !h.Fin {
			// control frames cannot be fragmented
			return 0, l.discard(h.Length, ErrInvalidContinuation)
		}
		if h.Length > 125 {
			// control frames cannot have payloads over 125 bytes
			return 0, l.discard(h.Length, ErrInvalidControlLength)
		}
		if h.OpCode == opClose && h.Length == 1 {
			return 0, l.discard(h.Length, ErrInvalidCloseReason)
		}
	}

	var buf *bytes.Buffer

	switch h.OpCode {
	case opText, opBinary:
		l.messagebuf.Reset()

		// try to set a continuation op code
		if !conn.setContinuation(h.OpCode) {
			// if this is a text or binary message and not final
			// and we coudln't set the op code for a continuation
			// due to an existing cotinuation, fail
			return 0, l.discard(h.Length, ErrInvalidContinuation)
		}

		if !h.Fin {
			// return this frame as a continuation
			h.OpCode = opContinuation
		} else {
			// reset the continuation as this is a final frame
			conn.resetContinuation()
		}

		buf = l.messagebuf
	case opContinuation:
		if conn.continuation() == 0 {
			// we've received a continuation frame
			// that was not started with a text or
			// binary op frame, so fail
			return 0, l.discard(h.Length, ErrInvalidContinuation)
		} else {
			// select the message buffer to write the continuation
			// payload into
			buf = l.messagebuf

			if h.Fin {
				// we've reached the last continuation frame
				// so reset the op code and give this header
				// its real opcode
				h.OpCode = conn.continuation()
				conn.resetContinuation()
			}
		}
	default:
		buf = l.framebuf
	}

	_, err = io.CopyN(buf, l.readbuf, h.Length)
	if err != nil {
		return h.OpCode, err
	}

	if h.Masked {
		// apply mask to frame we've just read
		cipher(
			buf.Bytes()[buf.Len()-int(h.Length):],
			h.Mask,
			0,
		)
	}

	// validate the payload
	switch h.OpCode {
	case opClose:
		if l.framebuf.Len() >= 2 {
			code := CloseStatus(binary.BigEndian.Uint16(l.framebuf.Bytes()[:2]))

			_, valid := validCloseStatus[code]
			if !valid {
				return 0, ErrInvalidCloseReason
			}

			if !utf8.Valid(l.framebuf.Bytes()[2:]) {
				return 0, ErrInvalidCloseEncoding
			}
		}
	case opText:
		if !utf8.Valid(buf.Bytes()[buf.Len()-int(h.Length):]) {
			return 0, ErrInvalidPayloadEncoding
		}
	}

	return h.OpCode, nil
}

func (l *listener) register(fd int, conn net.Conn) {
	bc := newBufConn(
		conn,
		l.bufpool,
		l.flushDeadline,
		func(bc *Conn, err error) {
			l.disconnect(fd, bc, err)
		},
	)

	l.timermu.Lock()
	l.timerheap.push(time.Now().Unix(), bc)
	l.timermu.Unlock()

	l.conns.Store(fd, bc)

	err := unix.EpollCtl(l.fd, syscall.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Events: unix.POLLIN | unix.POLLHUP, Fd: int32(fd)})
	if err != nil {
		l.error(fmt.Errorf("failed to add conn fd to epoll: %w", err), false)
		return
	}
}

func (l *listener) purgeIdle() {
	// dont check on every iteration
	l.counter++

	if l.counter%100 != 0 {
		return
	}

	now := time.Now().Add(-l.readDeadline).Unix()

	l.timermu.Lock()
	defer l.timermu.Unlock()

	for {
		conn := l.timerheap.popIf(now)
		if conn == nil {
			return
		}

		conn.CloseImmediatelyWith(CloseStatusAbnormalClosure, ErrConnectionTimeout.Reason, true)
	}
}

func (l *listener) error(err error, isFatal bool) {
	if l.handler.OnError != nil {
		l.handler.OnError(err, isFatal)
	}
}

func (l *listener) pongWs(conn *Conn) error {
	hd, err := l.codec.BuildHeader(header{
		OpCode: opPong,
		Fin:    true,
		Length: int64(l.framebuf.Len()),
	})

	if err != nil {
		return err
	}

	// create a temporary net buffer
	// so we can make use of writev
	// to send the header and ping
	// payload in one syscall
	b := net.Buffers{
		hd,
		l.framebuf.Bytes(),
	}

	// write directly to the connection
	_, err = b.WriteTo(conn.Conn)

	return err
}

func (l *listener) disconnect(fd int, conn *Conn, derr error) {
	// tell epoll we don't need to monitor this connection anymore
	err := unix.EpollCtl(l.fd, syscall.EPOLL_CTL_DEL, fd, &unix.EpollEvent{Events: unix.POLLIN | unix.POLLHUP, Fd: int32(fd)})
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			l.error(err, false)
		}
	}

	err = conn.Close()
	if err != nil {
		l.error(err, false)
	}

	// delete the connection from our connection list
	_, ok := l.conns.LoadAndDelete(fd)
	if !ok {
		return
	}

	if conn.h > HeapRemoved {
		// if this hasn't been removed from the heap
		// remove it
		l.timermu.Lock()
		l.timerheap.delete(conn)
		l.timermu.Unlock()
	}

	if l.handler.OnDisconnect != nil {
		l.handler.OnDisconnect(conn, derr)
	}
}

func (l *listener) discard(length int64, cerr error) error {
	_, err := io.CopyN(io.Discard, l.readbuf, length)
	if err != nil {
		return err
	}

	return cerr
}

func (l *listener) shutdown() error {
	err := l.socket.Close()
	if err != nil {
		return err
	}

	return unix.Close(l.fd)
}
