package wsev

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

func TestCloseFromOtherGoroutineDoesNotRaceReader(t *testing.T) {
	var mu sync.Mutex
	var conns []*Conn

	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				mu.Lock()
				conns = append(conns, conn)
				mu.Unlock()
			},
		},
		WithReadDeadline(5*time.Second),
	)

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		conn, _ := testconn(t, address)
		defer conn.Close()

		wg.Add(1)

		go func() {
			defer wg.Done()

			frame := maskedFrame(opBinary, true, bytes.Repeat([]byte{'x'}, 4000))

			for j := 0; j < 200; j++ {
				_, err := conn.Write(frame[:len(frame)/2])
				if err != nil {
					return
				}

				_, err = conn.Write(frame[len(frame)/2:])
				if err != nil {
					return
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	for _, c := range conns {
		go c.CloseWith(CloseStatusGoingAway, "bye", false)
	}
	mu.Unlock()

	time.Sleep(300 * time.Millisecond)
	wg.Wait()
}

func TestStaleTeardownKeepsReusedFd(t *testing.T) {
	epfd, err := unix.EpollCreate1(0)
	RequireNil(t, err)
	defer unix.Close(epfd)

	l := &listener{
		fd:      epfd,
		handler: &Handler{},
	}

	stale := &Conn{}
	current := &Conn{}

	l.conns.Store(42, current)

	RequireFalse(t, l.teardown(42, stale, nil))

	v, ok := l.conns.Load(42)
	RequireTrue(t, ok)
	RequireTrue(t, v.(*Conn) == current)
}

func TestConnCloseNotifiesDisconnect(t *testing.T) {
	closech := make(chan *testevent, 1)

	s := New(
		&Handler{
			OnMessage: func(conn *Conn, msg []byte) {
				conn.Close()
			},
			OnDisconnect: func(conn *Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
		},
		WithReadDeadline(5*time.Second),
	)

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	conn, rb := testconn(t, address)
	defer conn.Close()

	_, err := conn.Write(maskedFrame(opBinary, true, []byte("close me")))
	RequireNil(t, err)

	RequireNil(t, timeout(closech, time.Second))

	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err = rb.ReadByte()
	RequireEqual(t, io.EOF, err)
}

func TestDisconnectCallbackCanUseConn(t *testing.T) {
	closech := make(chan error, 1)

	s := New(
		&Handler{
			OnMessage: func(conn *Conn, msg []byte) {
				go conn.CloseWith(CloseStatusNormalClosure, "", true)
			},
			OnDisconnect: func(conn *Conn, err error) {
				_, werr := conn.Write([]byte("late"))
				closech <- werr
			},
		},
		WithReadDeadline(5*time.Second),
	)

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	conn, _ := testconn(t, address)
	defer conn.Close()

	_, err := conn.Write(maskedFrame(opBinary, true, []byte("bye")))
	RequireNil(t, err)

	werr, err := timeoutValue(closech, time.Second)
	RequireNil(t, err)
	RequireEqual(t, ErrConnectionAlreadyClosed, *werr)
}

func TestWriteToStalledClientTimesOut(t *testing.T) {
	errch := make(chan error, 1)

	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				go func() {
					payload := bytes.Repeat([]byte{'x'}, 1024)

					for {
						_, err := conn.Write(payload)
						if err != nil {
							errch <- err
							return
						}
					}
				}()
			},
		},
		WithWriteDeadline(200*time.Millisecond),
		WithReadDeadline(time.Minute),
	)

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	conn, _ := testconn(t, address)
	defer conn.Close()

	_, err := timeoutValue(errch, 5*time.Second)
	RequireNil(t, err)
}

func TestFailedWriteClosesConnection(t *testing.T) {
	closech := make(chan error, 1)

	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				go func() {
					payload := bytes.Repeat([]byte{'x'}, 1<<16)

					for {
						_, err := conn.Write(payload)
						if err != nil {
							return
						}
					}
				}()
			},
			OnDisconnect: func(conn *Conn, err error) {
				closech <- err
			},
		},
		WithWriteDeadline(200*time.Millisecond),
		WithReadDeadline(time.Minute),
	)

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	conn, _ := testconn(t, address)
	defer conn.Close()

	derr, err := timeoutValue(closech, 5*time.Second)
	RequireNil(t, err)
	RequireNotNil(t, *derr)
}

func TestOversizedMessageRejected(t *testing.T) {
	for _, fragmented := range []bool{false, true} {
		s := New(
			&Handler{},
			WithMaxMessageSize(1024),
			WithReadDeadline(5*time.Second),
		)

		port, address := nexttestport()
		RequireNil(t, s.Serve(port))

		conn, rb := testconn(t, address)

		if fragmented {
			chunk := bytes.Repeat([]byte{'x'}, 600)
			conn.Write(maskedFrame(opBinary, false, chunk))
			conn.Write(maskedFrame(opContinuation, true, chunk))
		} else {
			hd, _ := newCodec().BuildHeader(header{
				OpCode: opBinary,
				Fin:    true,
				Masked: true,
				Length: 1 << 40,
			})
			conn.Write(hd)
			conn.Write(make([]byte, 4096))
		}

		got := make([]byte, 4)
		conn.SetReadDeadline(time.Now().Add(time.Second))
		_, err := io.ReadFull(rb, got)
		RequireNil(t, err)
		RequireEqual(t, byte(0x88), got[0])
		RequireEqual(t, []byte{0x03, 0xf1}, got[2:4])

		conn.Close()
		s.Close()
	}
}

func TestIdleConnectionPurgedUnderLoad(t *testing.T) {
	var idle *Conn

	opench := make(chan *Conn, 2)
	closech := make(chan *Conn, 2)

	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				opench <- conn
			},
			OnDisconnect: func(conn *Conn, err error) {
				closech <- conn
			},
		},
		WithListeners(1),
		WithReadDeadline(time.Second),
	)

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	busy, _ := testconn(t, address)
	defer busy.Close()
	<-opench

	quiet, _ := testconn(t, address)
	defer quiet.Close()
	idle = <-opench

	stop := make(chan struct{})
	defer close(stop)

	go func() {
		frame := maskedFrame(opBinary, true, []byte("x"))

		for {
			select {
			case <-stop:
				return
			default:
			}

			if _, err := busy.Write(frame); err != nil {
				return
			}

			time.Sleep(time.Millisecond)
		}
	}()

	select {
	case c := <-closech:
		RequireTrue(t, c == idle)
	case <-time.After(5 * time.Second):
		t.Fatal("idle connection was never purged while another connection was busy")
	}
}

type flakyListener struct {
	net.Listener
	failures int
	calls    int
}

func (f *flakyListener) Accept() (net.Conn, error) {
	f.calls++

	if f.calls <= f.failures {
		return nil, syscall.EMFILE
	}

	return nil, net.ErrClosed
}

func TestAcceptSurvivesTransientErrors(t *testing.T) {
	var errs []error
	var fatal bool

	s := New(&Handler{
		OnError: func(err error, isFatal bool) {
			errs = append(errs, err)
			fatal = fatal || isFatal
		},
	})

	fl := &flakyListener{failures: 3}

	done := make(chan struct{})

	go func() {
		s.accept(&listener{socket: fl})
		close(done)
	}()

	RequireNil(t, timeout(done, 2*time.Second))
	RequireEqual(t, 4, fl.calls)
	RequireEqual(t, 3, len(errs))
	RequireFalse(t, fatal)
}

func TestHandshakeAcceptsConnectionTokenList(t *testing.T) {
	s := New(&Handler{}, WithReadDeadline(5*time.Second))

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	conn, err := net.Dial("tcp", address)
	RequireNil(t, err)
	defer conn.Close()

	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", address, enc.EncodeToString(make([]byte, 16)))

	conn.SetReadDeadline(time.Now().Add(time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	RequireNil(t, err)
	RequireEqual(t, http.StatusSwitchingProtocols, resp.StatusCode)
}

func TestHandshakeSplitAcrossSegments(t *testing.T) {
	msgch := make(chan []byte, 1)

	s := New(
		&Handler{
			OnMessage: func(conn *Conn, msg []byte) {
				msgch <- append([]byte{}, msg...)
			},
		},
		WithReadDeadline(5*time.Second),
	)

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	conn, err := net.Dial("tcp", address)
	RequireNil(t, err)
	defer conn.Close()

	conn.(*net.TCPConn).SetNoDelay(true)

	request := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", address, enc.EncodeToString(make([]byte, 16)))

	_, err = conn.Write([]byte(request[:20]))
	RequireNil(t, err)

	time.Sleep(50 * time.Millisecond)

	_, err = conn.Write(append([]byte(request[20:]), maskedFrame(opBinary, true, []byte("pipelined"))...))
	RequireNil(t, err)

	conn.SetReadDeadline(time.Now().Add(time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	RequireNil(t, err)
	RequireEqual(t, http.StatusSwitchingProtocols, resp.StatusCode)

	msg, err := timeoutValue(msgch, time.Second)
	RequireNil(t, err)
	RequireEqual(t, []byte("pipelined"), *msg)
}

func TestUTF8SplitAcrossFragments(t *testing.T) {
	msgch := make(chan string, 1)

	s := New(
		&Handler{
			OnText: func(conn *Conn, msg string) {
				msgch <- msg
			},
		},
		WithReadDeadline(5*time.Second),
	)

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	conn, _ := testconn(t, address)
	defer conn.Close()

	euro := []byte("€")

	_, err := conn.Write(maskedFrame(opText, false, euro[:1]))
	RequireNil(t, err)

	_, err = conn.Write(maskedFrame(opContinuation, true, euro[1:]))
	RequireNil(t, err)

	msg, err := timeoutValue(msgch, time.Second)
	RequireNil(t, err)
	RequireEqual(t, "€", *msg)
}

func TestInvalidUTF8MessageRejected(t *testing.T) {
	s := New(&Handler{OnText: func(conn *Conn, msg string) {}}, WithReadDeadline(5*time.Second))

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	conn, rb := testconn(t, address)
	defer conn.Close()

	euro := []byte("€")

	_, err := conn.Write(maskedFrame(opText, false, euro[:1]))
	RequireNil(t, err)

	_, err = conn.Write(maskedFrame(opContinuation, true, []byte{'x'}))
	RequireNil(t, err)

	got := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err = io.ReadFull(rb, got)
	RequireNil(t, err)
	RequireEqual(t, byte(0x88), got[0])
	RequireEqual(t, []byte{0x03, 0xef}, got[2:4])
}

func TestUnmaskedClientFrameRejected(t *testing.T) {
	msgch := make(chan []byte, 1)

	s := New(
		&Handler{
			OnMessage: func(conn *Conn, msg []byte) {
				msgch <- msg
			},
		},
		WithReadDeadline(5*time.Second),
	)

	port, address := nexttestport()
	RequireNil(t, s.Serve(port))
	defer s.Close()

	conn, rb := testconn(t, address)
	defer conn.Close()

	RequireNil(t, newCodec().WriteHeader(conn, header{OpCode: opBinary, Fin: true, Length: 2}))
	_, err := conn.Write([]byte("hi"))
	RequireNil(t, err)

	got := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err = io.ReadFull(rb, got)
	RequireNil(t, err)
	RequireEqual(t, byte(0x88), got[0])
	RequireEqual(t, []byte{0x03, 0xea}, got[2:4])

	select {
	case <-msgch:
		t.Fatal("unmasked frame was delivered to the handler")
	default:
	}
}
