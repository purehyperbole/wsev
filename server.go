package wsev

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var (
	ErrConnectionAlreadyClosed = errors.New("connection has already been closed due to an error")
	wsAcceptID                 = []byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
	enc                        = base64.StdEncoding
)

const (
	DefaultBufferSize          = 1 << 14
	DefaultBufferFlushDeadline = time.Second
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

type listener struct {
	conns sync.Map
	http  http.Server
	_p1   [8]uint64
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
	handler             *Handler
	listeners           []listener
	wbuffers            sync.Pool
	readDeadline        time.Duration
	writeBufferDeadline time.Duration
	readBufferSize      int
	writeBufferSize     int
}

func New(handler *Handler, opts ...option) *Server {
	s := &Server{
		handler:   handler,
		listeners: make([]listener, runtime.GOMAXPROCS(0)),
		wbuffers: sync.Pool{
			New: func() any {
				return &wbuf{
					b: bytes.NewBuffer(make([]byte, DefaultBufferSize)),
					c: newWriteCodec(),
				}
			},
		},
		readDeadline:        DefaultReadDeadline,
		writeBufferDeadline: DefaultBufferFlushDeadline,
		writeBufferSize:     DefaultBufferSize,
		readBufferSize:      DefaultBufferSize,
	}

	for i := range opts {
		opts[i](s)
	}

	return s
}

func (s *Server) Serve(port int) error {
	for i := 0; i < runtime.GOMAXPROCS(0); i++ {
		go func(pid int) {
			fd, err := unix.EpollCreate1(0)
			if err != nil {
				s.error(err, true)
				return
			}

			go func(pid, fd int) {
				events := make([]syscall.EpollEvent, 128)

				// TODO we can't rely on ReadDeadline for connections
				// that are not
				// use heap to track all connections and time them out
				// if they are not active within our deadline

				// read buffer for buffering reads from the underlying
				// connection and to hold any split frames
				rb := bufio.NewReaderSize(nil, DefaultBufferSize)
				fb := bytes.NewBuffer(make([]byte, DefaultBufferSize))
				cd := newCodec()

				// wait for epoll to return some events
				for {
					// TODO make wait duration configurable
					ec, err := syscall.EpollWait(fd, events, 10)
					if err != nil {
						// ignore interupted syscall
						if errors.Is(err, syscall.EINTR) {
							continue
						}

						s.error(err, true)
						return
					}

					for i := 0; i < ec; i++ {
						conn, ok := s.listeners[pid].conns.Load(int(events[i].Fd))
						if !ok {
							continue
						}

						cn := conn.(*Conn)

						if events[i].Events&unix.POLLHUP > 0 {
							// this is a disconnect event
							s.disconnect(pid, fd, int(events[i].Fd), cn, io.EOF)
						}

						// reset the buffers and read a completed
						// frame/message from the connection
						rb.Reset(cn)
						fb.Reset()

						op, err := assembleFrames(cn, cd, rb, fb, s.readDeadline)
						if err != nil {
							s.disconnect(pid, fd, int(events[i].Fd), cn, err)
							continue
						}

						switch op {
						case opPong:
							if s.handler.OnPong != nil {
								s.handler.OnPong(cn)
							}
						case opPing:
							err = pongWs(cn.Conn, cd)
							if err != nil {
								s.disconnect(pid, fd, int(events[i].Fd), cn, err)
								continue
							}

							if s.handler.OnPing != nil {
								s.handler.OnPing(cn)
							}
						case opClose:
							s.disconnect(pid, fd, int(events[i].Fd), cn, nil)
						case opBinary:
							if s.handler.OnMessage != nil {
								s.handler.OnMessage(cn, fb.Bytes())
							} else if s.handler.OnBinary != nil {
								s.handler.OnBinary(cn, fb.Bytes())
							}
						case opText:
							if s.handler.OnMessage != nil {
								s.handler.OnMessage(cn, fb.Bytes())
							} else if s.handler.OnText != nil {
								s.handler.OnText(cn, fb.String())
							}
						}

						// TODO set last read time
					}
				}
			}(pid, fd)

			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				conn, err := acceptWs(w, r)
				if err != nil {
					s.error(fmt.Errorf("failed to upgrade ws: %w", err), false)
					return
				}

				cfd, err := connectionFd(conn)
				if err != nil {
					s.error(fmt.Errorf("failed to get ws file descriptor: %w", err), false)
					return
				}

				err = unix.EpollCtl(fd, syscall.EPOLL_CTL_ADD, cfd, &unix.EpollEvent{Events: unix.POLLIN | unix.POLLHUP, Fd: int32(cfd)})
				if err != nil {
					s.error(fmt.Errorf("failed to add conn fd to epoll: %w", err), false)
					return
				}

				bc := newBufConn(
					conn,
					&s.wbuffers,
					s.writeBufferDeadline,
					s.writeBufferSize,
					func(bc *Conn, err error) {
						s.disconnect(pid, fd, cfd, bc, err)
					},
				)

				s.listeners[pid].conns.Store(cfd, bc)

				if s.handler.OnConnect != nil {
					s.handler.OnConnect(bc)
				}
			})

			s.listeners[pid].http = http.Server{
				Addr:    fmt.Sprintf(":%d", port),
				Handler: mux,
			}

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

			ln, err := lc.Listen(context.Background(), "tcp", s.listeners[pid].http.Addr)
			if err != nil {
				s.error(err, true)
				return
			}

			err = s.listeners[pid].http.Serve(ln)
			if err != nil {
				s.error(err, true)
			}
		}(i)
	}

	return nil
}

// Close closes all of the http servers
func (s *Server) Close() error {
	for i := range s.listeners {
		err := s.listeners[i].http.Close()
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) close(pid, fd, cfd int, conn *Conn, status CloseStatus, reason []byte) {
	_, err := conn.CloseWithReason(status, reason)
	if err != nil {
		s.error(err, false)
	}
}

func (s *Server) disconnect(pid, fd, cfd int, conn *Conn, derr error) {
	// tell epoll we don't need to monitor this connection anymore
	err := unix.EpollCtl(fd, syscall.EPOLL_CTL_DEL, cfd, &unix.EpollEvent{Events: unix.POLLIN | unix.POLLHUP, Fd: int32(cfd)})
	if err != nil {
		s.error(err, false)
	}

	err = conn.Close()
	if err != nil {
		s.error(err, false)
	}

	// delete the connection from our connection list
	_, ok := s.listeners[pid].conns.LoadAndDelete(cfd)
	if !ok {
		return
	}

	if s.handler.OnDisconnect != nil {
		s.handler.OnDisconnect(conn, derr)
	}
}

func (s *Server) error(err error, isFatal bool) {
	if s.handler.OnError != nil {
		s.handler.OnError(err, isFatal)
	}
}

func acceptWs(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	// https://datatracker.ietf.org/doc/html/rfc6455#section-4.2.1
	h := r.Header

	// check the client is running at least HTTP/1.1
	if !r.ProtoAtLeast(1, 1) {
		w.WriteHeader(http.StatusUpgradeRequired)
		return nil, errors.New("unsupported client http version")
	}

	// check this is a get request
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusUpgradeRequired)
		return nil, errors.New("unsupported http method")
	}

	// check request header contains the 'Connection: Upgrade' and 'Upgrade: websocket' headers
	if !strings.EqualFold(h.Get("Connection"), "upgrade") && !strings.EqualFold(h.Get("Upgrade"), "websocket") {
		w.WriteHeader(http.StatusUpgradeRequired)
		return nil, errors.New("invalid upgrade headers")
	}

	// validate Sec-WebSocket-Key header
	socketKey, err := enc.DecodeString(h.Get("Sec-WebSocket-Key"))
	if err != nil {
		w.WriteHeader(http.StatusUpgradeRequired)
		return nil, errors.New("invalid Sec-Websocket-Key base64")
	}

	// check Sec-WebSocket-Key is at least 16 bytes
	if len(socketKey) != 16 {
		w.WriteHeader(http.StatusUpgradeRequired)
		return nil, errors.New("invalid Sec-Websocket-Key base64 length")
	}

	// check the client is running websocket version 13
	if h.Get("Sec-WebSocket-Version") != "13" {
		w.WriteHeader(http.StatusUpgradeRequired)
		return nil, errors.New("invalid Sec-Websocket-Key base64 length")
	}

	origin := h.Get("Origin")
	if origin != "" {
		o, err := url.Parse(origin)
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			return nil, errors.New("invalid Origin header")
		}

		if o.Host != r.Host {
			w.WriteHeader(http.StatusForbidden)
			return nil, errors.New("Origin header does not match hostname")
		}
	}

	// https://datatracker.ietf.org/doc/html/rfc6455#section-4.2.2

	// hash the key with the unique string specified by the spec
	sh := sha1.New()
	sh.Write([]byte(h.Get("Sec-WebSocket-Key")))
	sh.Write(wsAcceptID)

	// write headers
	w.Header().Add("Connection", "Upgrade")
	w.Header().Add("Upgrade", "websocket")
	w.Header().Add("Sec-WebSocket-Accept", enc.EncodeToString(sh.Sum(nil)))
	w.WriteHeader(http.StatusSwitchingProtocols)

	// hijack connection
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return nil, err
	}

	return conn, conn.SetDeadline(time.Time{})
}

func pingWs(cn net.Conn, wc *codec) error {
	return wc.WriteHeader(cn, header{
		OpCode: opPing,
		Fin:    true,
	})
}

func pongWs(cn net.Conn, wc *codec) error {
	return wc.WriteHeader(cn, header{
		OpCode: opPong,
		Fin:    true,
	})
}

// assemble websocket frame(s) into a buffer
func assembleFrames(cn net.Conn, rc *codec, rb *bufio.Reader, fb *bytes.Buffer, dl time.Duration) (opCode, error) {
	var h header
	var op *opCode
	var err error

	for !h.Fin {
		err = cn.SetReadDeadline(time.Now().Add(dl))
		if err != nil {
			return 0, err
		}

		h, err = rc.ReadHeader(rb)
		if err != nil {
			return 0, err
		}

		if op == nil {
			op = &h.OpCode
		}

		err = cn.SetReadDeadline(time.Now().Add(dl))
		if err != nil {
			return *op, err
		}

		_, err := io.CopyN(fb, rb, h.Length)
		if err != nil {
			return *op, err
		}

		if h.Masked {
			// apply mask to frame we've just read
			cipher(
				fb.Bytes()[fb.Len()-int(h.Length):],
				h.Mask,
				0,
			)
		}
	}

	return *op, nil
}

func connectionFd(conn net.Conn) (int, error) {
	fd, err := conn.(*net.TCPConn).File()
	return int(fd.Fd()), err
}
