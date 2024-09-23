package wsev

import (
	"bufio"
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
	readbuf       *bufio.Reader
	readbufpool   *sync.Pool
	writebufpool  *sync.Pool
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

func newListener(epollFd int, handler *Handler, readbufpool, writebufpool *sync.Pool, readDeadline, flushDeadline time.Duration, writebufsize int) *listener {
	l := &listener{
		fd:            epollFd,
		readDeadline:  readDeadline,
		flushDeadline: flushDeadline,
		writebufsize:  writebufsize,
		readbufpool:   readbufpool,
		writebufpool:  writebufpool,
		readbuf:       bufio.NewReaderSize(nil, DefaultBufferSize),
		codec:         newCodec(),
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

			if events[i].Events&unix.POLLHUP > 0 {
				l.disconnect(int(events[i].Fd), cn, io.EOF)
				continue
			}

			buf := cn.acquireReadBuffer()

			// read until there is no more data in the read buffer
			// limit this to a maximum amount so we don't get
			// dominated by a single connection
			for i := 0; i < 20; i++ {
				// buffer data from the underlying connection
				err = cn.buffer()
				if err != nil {
					if errors.Is(err, syscall.EAGAIN) {
						// there's no data left to read
						break
					}
				}

				// upgrade the connection and write upgrade negotiation
				// response directly to underlying connection
				if cn.upgrade() {
					err = acceptWs(cn, buf, l.readbuf)
					if err != nil {
						l.disconnect(int(events[i].Fd), cn, err)
						break
					}

					if l.handler.OnConnect != nil {
						l.handler.OnConnect(cn)
					}
				}

				err = l.read(events[i].Fd, cn, buf)
				if err != nil {
					l.disconnect(int(events[i].Fd), cn, err)
					break
				}
			}

			cn.tryReleaseReadBuffer()

			if cn.heapindex > HeapRemoved {
				// reset the timer on the connection
				l.timermu.Lock()
				l.timerheap.decrease(cn, now)
				l.timermu.Unlock()
			}
		}
	}
}

func (l *listener) read(fd int32, conn *Conn, buf *rbuf) error {
	op, err := l.assembleFrame(conn, buf)
	if err != nil {
		if errors.Is(err, syscall.EAGAIN) {
			// we don't have a complete frame, leave buffers as is
			return nil
		}

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
		if !utf8.Valid(buf.m.Bytes()) {
			return conn.CloseWith(CloseStatusInvalidFramePayloadData, "invalid utf8 payload", false)
		}

		if l.handler.OnMessage != nil {
			l.handler.OnMessage(conn, buf.m.Bytes())
		} else if l.handler.OnText != nil {
			l.handler.OnText(conn, buf.m.String())
		}
	case opBinary:
		if l.handler.OnMessage != nil {
			l.handler.OnMessage(conn, buf.m.Bytes())
		} else if l.handler.OnBinary != nil {
			l.handler.OnBinary(conn, buf.m.Bytes())
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
func (l *listener) assembleFrame(conn *Conn, buf *rbuf) (opCode, error) {
	offset, err := l.codec.ReadHeader(&buf.h, buf.f)
	if err != nil {
		return 0, err
	}

	// validate this frame after we have read data to avoid
	// epoll waking us up again to read the remaining bytes
	if buf.h.Rsv > 0 {
		// we don't support rsv bits > 0
		return 0, ErrInvalidRSVBits
	}

	if buf.h.isReserved() {
		// reserved op code used
		return 0, ErrInvalidOpCode
	}

	if buf.h.isControl() {
		if !buf.h.Fin {
			// control frames cannot be fragmented
			return 0, ErrInvalidContinuation
		}
		if buf.h.Length > 125 {
			// control frames cannot have payloads over 125 bytes
			return 0, ErrInvalidControlLength
		}
		if buf.h.OpCode == opClose && buf.h.Length == 1 {
			return 0, ErrInvalidCloseReason
		}
	}

	switch buf.h.OpCode {
	case opText, opBinary:
		// try to set a continuation op code
		if !conn.setContinuation(buf.h.OpCode) {
			// if this is a text or binary message and not final
			// and we coudln't set the op code for a continuation
			// due to an existing cotinuation, fail
			return 0, ErrInvalidContinuation
		}

		if !buf.h.Fin {
			// return this frame as a continuation
			buf.h.OpCode = opContinuation
		} else {
			// reset the continuation as this is a final frame
			conn.resetContinuation()
		}
	case opContinuation:
		if conn.continuation() == 0 {
			// we've received a continuation frame
			// that was not started with a text or
			// binary op frame, so fail
			return 0, ErrInvalidContinuation
		} else {
			// select the message buffer to write the continuation
			// payload into
			if buf.h.Fin {
				// we've reached the last continuation frame
				// so reset the op code and give this header
				// its real opcode
				buf.h.OpCode = conn.continuation()
				conn.resetContinuation()
			}
		}
	}

	// copy our buffered frame data to our message buffer
	if buf.h.Length < int64(len(buf.f)-offset) {
		_, _ = buf.m.Write(buf.f[offset : offset+int(buf.h.Length)])
		buf.h.received = buf.h.Length

		remaining := len(buf.f) - offset + int(buf.h.Length)

		// move the remaining bytes forward to the start of the buffer
		// TODO use a circular buffer to avoid this...
		copy(buf.f, buf.f[offset+int(buf.h.Length):])
		buf.f = buf.f[:remaining]
	} else {
		_, _ = buf.m.Write(buf.f[offset:])
		buf.h.received = int64(len(buf.f) - offset)
		buf.f = buf.f[:0]
	}

	if buf.h.Length != buf.h.received {
		return 0, syscall.EAGAIN
	}

	if buf.h.Masked {
		// apply mask to frame we've just read
		cipher(
			buf.m.Bytes()[buf.m.Len()-int(buf.h.Length):],
			buf.h.Mask,
			0,
		)
	}

	// validate the payload
	switch buf.h.OpCode {
	case opClose:
		if buf.m.Len() >= 2 {
			code := CloseStatus(binary.BigEndian.Uint16(buf.m.Bytes()[:2]))

			_, valid := validCloseStatus[code]
			if !valid {
				return 0, ErrInvalidCloseReason
			}

			if !utf8.Valid(buf.m.Bytes()[2:]) {
				return 0, ErrInvalidCloseEncoding
			}
		}
	case opText:
		if !utf8.Valid(buf.m.Bytes()[buf.m.Len()-int(buf.h.Length):]) {
			return 0, ErrInvalidPayloadEncoding
		}
	}

	return buf.h.OpCode, nil
}

func (l *listener) register(fd int, conn net.Conn) {
	bc := newBufConn(
		conn,
		l.readbufpool,
		l.writebufpool,
		l.flushDeadline,
		func(bc *Conn, err error) {
			l.disconnect(fd, bc, err)
		},
	)

	l.timermu.Lock()
	l.timerheap.push(time.Now().Unix(), bc)
	l.timermu.Unlock()

	l.conns.Store(fd, bc)

	err := unix.SetNonblock(fd, true)
	if err != nil {
		l.error(fmt.Errorf("failed to set non-blocking socket: %w", err), false)
		return
	}

	err = unix.EpollCtl(l.fd, syscall.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Events: unix.POLLIN | unix.POLLHUP, Fd: int32(fd)})
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
	buf := conn.acquireReadBuffer()

	hd, err := l.codec.BuildHeader(header{
		OpCode: opPong,
		Fin:    true,
		Length: int64(buf.m.Len()),
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
		buf.m.Bytes(),
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

	if conn.heapindex > HeapRemoved {
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

func (l *listener) shutdown() error {
	err := l.socket.Close()
	if err != nil {
		return err
	}

	return unix.Close(l.fd)
}
