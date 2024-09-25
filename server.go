package wsev

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var (
	wsAcceptID = []byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
	enc        = base64.StdEncoding
)

const (
	DefaultBufferSize          = 1 << 14
	DefaultBufferFlushDeadline = time.Millisecond * 50
	DefaultReadDeadline        = time.Second
)

type option func(s *Server)

// WithReadDeadline sets the read deadline option when reading from sockets that have data available
func WithReadDeadline(deadline time.Duration) option {
	return func(s *Server) {
		s.readDeadline = deadline
	}
}

// WithWriteBufferDeadline sets the timeout for flushing the buffer
// to the underlying connection
func WithWriteBufferDeadline(deadline time.Duration) option {
	return func(s *Server) {
		s.writeBufferDeadline = deadline
	}
}

// WithWriteBufferSize sets the size of write buffer used for a connection
// when the buffer exceeds this size, it will be flushed
func WithWriteBufferSize(size int) option {
	return func(s *Server) {
		s.writeBufferSize = size
	}
}

// WithReadBufferSize sets the size of read buffer used for a connection
func WithReadBufferSize(size int) option {
	return func(s *Server) {
		s.readBufferSize = size
	}
}

// WithListeners sets the number of listeners, defaults to GOMAXPROCS
func WithListeners(count int) option {
	return func(s *Server) {
		s.listeners = make([]*listener, count)
	}
}

type Handler struct {
	// OnConnect is invoked upon a new connection
	OnConnect func(conn *Conn)
	// OnDisconnect is invoked when an existing connection disconnects
	OnDisconnect func(conn *Conn, err error)
	// OnPing is invoked when a connection sends a websocket ping frame
	OnPing func(conn *Conn)
	// OnPong is invoked when a connection sends a websocket pong frame
	OnPong func(conn *Conn)
	// OnMessage is invoked when a connection sends either a text or
	// binary message. the msg buffer that is passed is only safe for
	// use until the callback returns
	OnMessage func(conn *Conn, msg []byte)
	// OnBinary is invoked when a connection sends a binary message.
	// the msg buffer that is passed is only safe for use until the
	// callback returns
	OnBinary func(conn *Conn, msg []byte)
	// OnText is invoked when a connection sends a text message.
	// the msg buffer that is passed is only safe for use until the
	// callback returns
	OnText func(conn *Conn, msg string)
	// OnError is invoked when an error occurs
	OnError func(err error, isFatal bool)
}

type Server struct {
	rbuffers            sync.Pool
	wbuffers            sync.Pool
	handler             *Handler
	listeners           []*listener
	readDeadline        time.Duration
	writeBufferDeadline time.Duration
	readBufferSize      int
	writeBufferSize     int
}

func New(handler *Handler, opts ...option) *Server {
	s := &Server{
		handler:             handler,
		listeners:           make([]*listener, runtime.GOMAXPROCS(0)),
		readDeadline:        DefaultReadDeadline,
		writeBufferDeadline: DefaultBufferFlushDeadline,
		writeBufferSize:     DefaultBufferSize,
		readBufferSize:      DefaultBufferSize,
	}

	for i := range opts {
		opts[i](s)
	}

	s.rbuffers = sync.Pool{
		New: func() any {
			cbuf, err := newCircbuf(s.readBufferSize)
			if err != nil {
				panic(err)
			}

			return &rbuf{
				b: cbuf,
				f: bytes.NewBuffer(make([]byte, 0, 64)),
				m: bytes.NewBuffer(make([]byte, 0, s.readBufferSize)),
			}
		},
	}

	s.wbuffers = sync.Pool{
		New: func() any {
			return &wbuf{
				b: bufio.NewWriterSize(nil, s.writeBufferSize),
				c: newWriteCodec(),
			}
		},
	}

	return s
}

func (s *Server) Serve(port int) error {
	for i := 0; i < len(s.listeners); i++ {
		fd, err := unix.EpollCreate1(0)
		if err != nil {
			return err
		}

		s.listeners[i] = newListener(
			fd,
			s.handler,
			&s.rbuffers,
			&s.wbuffers,
			s.readDeadline,
			s.writeBufferDeadline,
			s.writeBufferSize,
		)

		lc := net.ListenConfig{
			Control: func(network, address string, c syscall.RawConn) error {
				var serr error

				err := c.Control(func(fd uintptr) {
					// set SO_REUSEPORT so multiple listeners can listen on the same port
					serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
				})

				if err != nil {
					return err
				}

				return serr
			},
		}

		s.listeners[i].socket, err = lc.Listen(context.Background(), "tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			return err
		}

		go func(pid int) {
			for {
				conn, err := s.listeners[pid].socket.Accept()
				if err != nil {
					s.error(err, true)
					continue
				}

				s.listeners[pid].register(connectionFd(conn), conn)
			}
		}(i)
	}

	return nil
}

// Close closes all of the http servers
func (s *Server) Close() error {
	for i := range s.listeners {
		err := s.listeners[i].shutdown()
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) error(err error, isFatal bool) {
	if s.handler.OnError != nil {
		s.handler.OnError(err, isFatal)
	}
}

func acceptWs(conn *Conn, buf *rbuf, rb *bufio.Reader) error {
	// wrap our read buffer in a temporary bufio reader
	rb.Reset(buf.b)

	// https://datatracker.ietf.org/doc/html/rfc6455#section-4.2.1
	r, err := http.ReadRequest(rb)
	if err != nil {
		return err
	}

	h := r.Header

	// check the client is running at least HTTP/1.1
	if !r.ProtoAtLeast(1, 1) {
		return writeHeader(
			conn,
			http.StatusUpgradeRequired,
			errors.New("unsupported client http version"),
		)
	}

	// check this is a get request
	if r.Method != http.MethodGet {
		return writeHeader(
			conn,
			http.StatusUpgradeRequired,
			errors.New("unsupported http method"),
		)
	}

	// check request header contains the 'Connection: Upgrade' and 'Upgrade: websocket' headers
	if !strings.EqualFold(h.Get("Connection"), "upgrade") && !strings.EqualFold(h.Get("Upgrade"), "websocket") {
		return writeHeader(
			conn,
			http.StatusUpgradeRequired,
			errors.New("invalid upgrade headers"),
		)
	}

	// validate Sec-WebSocket-Key header
	socketKey, err := enc.DecodeString(h.Get("Sec-WebSocket-Key"))
	if err != nil {
		writeHeader(
			conn,
			http.StatusUpgradeRequired,
			errors.New("invalid Sec-Websocket-Key base64"),
		)
	}

	// check Sec-WebSocket-Key is at least 16 bytes
	if len(socketKey) != 16 {
		return writeHeader(
			conn,
			http.StatusUpgradeRequired,
			errors.New("invalid Sec-Websocket-Key base64 length"),
		)
	}

	// check the client is running websocket version 13
	if h.Get("Sec-WebSocket-Version") != "13" {
		return writeHeader(
			conn,
			http.StatusUpgradeRequired,
			errors.New("invalid Sec-Websocket-Key base64 length"),
		)
	}

	origin := h.Get("Origin")
	if origin != "" {
		o, err := url.Parse(origin)
		if err != nil {
			return writeHeader(
				conn,
				http.StatusForbidden,
				errors.New("invalid Origin header"),
			)
		}

		if o.Host != r.Host {
			return writeHeader(
				conn,
				http.StatusForbidden,
				errors.New("origin header does not match hostname"),
			)
		}
	}

	// https://datatracker.ietf.org/doc/html/rfc6455#section-4.2.2

	// hash the key with the unique string specified by the spec
	sh := sha1.New()
	sh.Write([]byte(h.Get("Sec-WebSocket-Key")))
	sh.Write(wsAcceptID)

	// write response
	_, err = fmt.Fprintf(
		conn.Conn,
		"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		enc.EncodeToString(sh.Sum(nil)),
	)

	if err != nil {
		return err
	}

	return nil
}

func connectionFd(conn net.Conn) int {
	// get the poll file descriptor via reflect
	// os.File().Fd() doesn't return this sadly
	tcpConn := reflect.Indirect(reflect.ValueOf(conn)).FieldByName("conn")
	fdVal := tcpConn.FieldByName("fd")
	pfdVal := reflect.Indirect(fdVal).FieldByName("pfd")

	return int(pfdVal.FieldByName("Sysfd").Int())
}

func writeHeader(conn net.Conn, status int, err error) error {
	_, _ = fmt.Fprintf(
		conn,
		"HTTP/1.1 %d %s\r\n\r\n",
		status,
		http.StatusText(status),
	)
	return err
}
