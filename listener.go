package wsev

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type listener struct {
	fd            int           // epoll file descriptor
	readDeadline  time.Duration // read deadline
	flushDeadline time.Duration // write buffer flush deadline
	writebufsize  int           // connection write buffer size to flush at
	codec         *codec        // frame codec
	bufpool       *sync.Pool    // connection buffer pool
	readbuf       *bufio.Reader // read buffer
	framebuf      *bytes.Buffer // frame buffer
	handler       *Handler      // handler
	conns         sync.Map      // active connections
	http          http.Server   // http listener
	_p1           [8]uint64     // cache line padding
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
		framebuf:      bytes.NewBuffer(make([]byte, DefaultBufferSize)),
		handler:       handler,
	}

	go l.handleEvents()

	return l
}

func (l *listener) handleEvents() {
	events := make([]syscall.EpollEvent, 128)

	// TODO we can't rely on ReadDeadline for connections
	// that are not
	// use heap to track all connections and time them out
	// if they are not active within our deadline

	// wait for epoll to return some events
	for {
		// TODO make wait duration configurable
		ec, err := syscall.EpollWait(l.fd, events, 10)
		if err != nil {
			// ignore interupted syscall
			if errors.Is(err, syscall.EINTR) {
				continue
			}

			l.error(err, true)
			return
		}

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
				err = l.read(events[i].Fd, cn)
				if err != nil {
					l.disconnect(int(events[i].Fd), cn, err)
					break
				}

				if l.readbuf.Buffered() < 1 {
					break
				}
			}
			// TODO set last read time
		}
	}
}

func (l *listener) read(fd int32, conn *Conn) error {
	op, err := l.assembleFrames(int(fd), conn)
	if err != nil {
		if errors.Is(err, ErrRsvNotSupported) {
			_, cerr := conn.CloseImmediatelyWith(CloseStatusProtocolError, nil, false)
			if cerr != nil {
				return cerr
			}

			// we've sent the close frame, wait for the client to respond
			return nil
		}

		return err
	}

	switch op {
	case opText:
		if !utf8.Valid(l.framebuf.Bytes()) {
			_, err = conn.CloseWith(CloseStatusInvalidFramePayloadData, nil, false)
			if err != nil {
				return err
			}
		}

		if l.handler.OnMessage != nil {
			l.handler.OnMessage(conn, l.framebuf.Bytes())
		} else if l.handler.OnText != nil {
			l.handler.OnText(conn, l.framebuf.String())
		}
	case opBinary:
		if l.handler.OnMessage != nil {
			l.handler.OnMessage(conn, l.framebuf.Bytes())
		} else if l.handler.OnBinary != nil {
			l.handler.OnBinary(conn, l.framebuf.Bytes())
		}
	case opClose:
		_, err = conn.CloseWith(CloseStatusNormalClosure, nil, true)
		return err
	case opPing:
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
	default:
		l.error(fmt.Errorf("unsupported op code: %d", op), false)
		_, err = conn.CloseWith(CloseStatusProtocolError, nil, true)
		return err
	}

	return nil
}

// assemble websocket frame(s) into a buffer
func (l *listener) assembleFrames(fd int, cn *Conn) (opCode, error) {
	var h header
	var op *opCode
	var err error

	l.framebuf.Reset()

	for !h.Fin {
		err = cn.SetReadDeadline(time.Now().Add(l.readDeadline))
		if err != nil {
			return 0, err
		}

		h, err = l.codec.ReadHeader(l.readbuf)
		if err != nil {
			return 0, err
		}

		if h.Rsv > 0 {
			// we don't support rsv bits > 0
			return 0, ErrRsvNotSupported
		}

		if op == nil {
			op = &h.OpCode
		}

		err = cn.SetReadDeadline(time.Now().Add(l.readDeadline))
		if err != nil {
			return *op, err
		}

		_, err := io.CopyN(l.framebuf, l.readbuf, h.Length)
		if err != nil {
			return *op, err
		}

		if h.Masked {
			// apply mask to frame we've just read
			cipher(
				l.framebuf.Bytes()[l.framebuf.Len()-int(h.Length):],
				h.Mask,
				0,
			)
		}
	}

	return *op, nil
}

func (l *listener) register(fd int, conn net.Conn) {
	err := unix.EpollCtl(l.fd, syscall.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Events: unix.POLLIN | unix.POLLHUP, Fd: int32(fd)})
	if err != nil {
		l.error(fmt.Errorf("failed to add conn fd to epoll: %w", err), false)
		return
	}

	bc := newBufConn(
		conn,
		l.bufpool,
		l.flushDeadline,
		l.writebufsize,
		func(bc *Conn, err error) {
			l.disconnect(fd, bc, err)
		},
	)

	l.conns.Store(fd, bc)

	if l.handler.OnConnect != nil {
		l.handler.OnConnect(bc)
	}
}

func (l *listener) error(err error, isFatal bool) {
	if l.handler.OnError != nil {
		l.handler.OnError(err, isFatal)
	}
}

func (l *listener) pongWs(conn *Conn) error {
	if l.framebuf.Len() > 125 {
		_, err := conn.CloseWith(CloseStatusProtocolError, nil, false)
		return err
	}

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
		l.error(err, false)
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

	if l.handler.OnDisconnect != nil {
		l.handler.OnDisconnect(conn, derr)
	}
}
