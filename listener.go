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

			if events[i].Events&unix.POLLHUP > 0 {
				// this is a disconnect event
				l.disconnect(int(events[i].Fd), cn, io.EOF)
				continue
			}

			op, err := l.assembleFrames(int(events[i].Fd), cn)
			if err != nil {
				l.disconnect(int(events[i].Fd), cn, err)
				continue
			}

			switch op {
			case opText:
				if l.handler.OnMessage != nil {
					l.handler.OnMessage(cn, l.framebuf.Bytes())
				} else if l.handler.OnText != nil {
					l.handler.OnText(cn, l.framebuf.String())
				}
			case opBinary:
				if l.handler.OnMessage != nil {
					l.handler.OnMessage(cn, l.framebuf.Bytes())
				} else if l.handler.OnBinary != nil {
					l.handler.OnBinary(cn, l.framebuf.Bytes())
				}
			case opClose:
				l.disconnect(int(events[i].Fd), cn, nil)
			case opPing:
				err = pongWs(cn.Conn, l.codec)
				if err != nil {
					l.disconnect(int(events[i].Fd), cn, err)
					continue
				}

				if l.handler.OnPing != nil {
					l.handler.OnPing(cn)
				}
			case opPong:
				if l.handler.OnPong != nil {
					l.handler.OnPong(cn)
				}
			}

			// TODO set last read time
		}
	}
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

func (l *listener) close(conn *Conn, status CloseStatus, reason []byte) {
	_, err := conn.CloseWithReason(status, reason)
	if err != nil {
		l.error(err, false)
	}
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

// assemble websocket frame(s) into a buffer
func (l *listener) assembleFrames(fd int, cn *Conn) (opCode, error) {
	var h header
	var op *opCode
	var err error

	l.framebuf.Reset()
	l.readbuf.Reset(cn)

	for !h.Fin {
		err = cn.SetReadDeadline(time.Now().Add(l.readDeadline))
		if err != nil {
			return 0, err
		}

		h, err = l.codec.ReadHeader(l.readbuf)
		if err != nil {
			return 0, err
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

	// close is a special case, we need to echo
	// back the close message to complete the
	// close handshake
	if *op == opClose {
		err = l.codec.WriteHeader(cn.Conn, h)
		if err != nil {
			return *op, err
		}

		_, err := io.CopyN(cn.Conn, l.framebuf, 2)
		if err != nil {
			return *op, err
		}
	}

	return *op, nil
}
