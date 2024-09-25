package wsev

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testevent struct {
	conn net.Conn
	err  error
	msg  []byte
}

var testport int = 10000

func nexttestport() (int, string) {
	port := testport
	address := fmt.Sprintf("localhost:%d", port)

	testport++

	return port, address
}

func TestServerConnect(t *testing.T) {
	opench := make(chan *testevent, 1)
	closech := make(chan *testevent, 1)
	msgch := make(chan *testevent, 1)

	s := New(
		&Handler{
			OnError: func(err error, fatal bool) {
				//fmt.Println(err)
			},
			OnConnect: func(conn *Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn *Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnMessage: func(conn *Conn, msg []byte) {
				msgch <- &testevent{conn: conn, msg: msg}
			},
		},
		WithReadDeadline(time.Second*5),
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn, _ := testconn(t, address)
	defer conn.Close()
	<-opench
}

func TestServerDisconnect(t *testing.T) {
	opench := make(chan *testevent, 1)
	closech := make(chan *testevent, 1)
	msgch := make(chan *testevent, 1)

	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn *Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnMessage: func(conn *Conn, msg []byte) {
				msgch <- &testevent{conn: conn, msg: msg}
			},
		},
		WithReadDeadline(time.Second*5),
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn, _ := testconn(t, address)
	defer conn.Close()

	RequireNil(t, timeout(opench, time.Millisecond*100))

	codec := newCodec()

	err = codec.WriteHeader(conn, header{
		OpCode: opClose,
		Fin:    true,
	})

	RequireNil(t, err)

	event, err := timeoutValue(closech, time.Millisecond*100)
	RequireNil(t, err)
	AssertNil(t, (*event).err)
}

func TestServerDisconnectTimeout(t *testing.T) {
	opench := make(chan *testevent, 1)
	closech := make(chan *testevent, 1)
	msgch := make(chan *testevent, 1)

	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn *Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnMessage: func(conn *Conn, msg []byte) {
				msgch <- &testevent{conn: conn, msg: msg}
			},
		},
		WithReadDeadline(time.Second),
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn, _ := testconn(t, address)
	defer conn.Close()

	RequireNil(t, timeout(opench, time.Millisecond*100))
	RequireNil(t, timeout(closech, time.Second*5))
}

func TestServerPong(t *testing.T) {
	opench := make(chan *testevent, 1)
	closech := make(chan *testevent, 1)
	pingch := make(chan *testevent, 1)

	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn *Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnPing: func(conn *Conn) {
				pingch <- &testevent{conn: conn}
			},
		},
		WithReadDeadline(time.Second*5),
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn, rb := testconn(t, address)
	defer conn.Close()

	codec := newCodec()

	err = codec.WriteHeader(conn, header{
		OpCode: opPing,
		Fin:    true,
	})

	RequireNil(t, err)
	RequireNil(t, timeout(pingch, time.Millisecond*100))
	conn.SetReadDeadline(time.Now().Add(time.Millisecond * 100))

	var h header

	buf, err := newCircbuf(4096)
	RequireNil(t, err)

	n, err := rb.Read(buf.next())
	RequireNil(t, err)
	RequireNil(t, buf.advanceWrite(n))

	err = codec.ReadHeader(&h, buf)
	RequireNil(t, err)
	AssertEqual(t, opPong, h.OpCode)
	AssertTrue(t, h.Fin)
}

func TestServerSendUnmasked(t *testing.T) {
	opench := make(chan *testevent, 1)
	closech := make(chan *testevent, 1)

	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn *Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
		},
		WithWriteBufferDeadline(time.Millisecond),
		WithReadDeadline(time.Second*5),
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	recv, rb := testconn(t, address)
	defer recv.Close()

	send, err := timeoutValue(opench, time.Millisecond*100)
	RequireNil(t, err)

	codec := newCodec()
	data := make([]byte, 8)

	for i := 0; i < 10; i++ {
		binary.LittleEndian.PutUint64(data, uint64(i))

		_, err := (*send).conn.Write(data)
		RequireNil(t, err)

		var h header

		buf, err := newCircbuf(4096)
		RequireNil(t, err)

		n, err := rb.Read(buf.next())
		RequireNil(t, err)
		RequireNil(t, buf.advanceWrite(n))

		err = codec.ReadHeader(&h, buf)
		RequireNil(t, err)
		AssertEqual(t, int64(8), h.Length)

		if buf.buffered() < int(h.Length) {
			n, err = io.ReadFull(rb, buf.next())
			RequireNil(t, err)
			RequireNil(t, buf.advanceWrite(n))
		}

		peeked, err := buf.peek(len(data))
		RequireNil(t, err)

		AssertEqual(t, data, peeked)

		RequireNil(t, buf.advanceRead(len(data)))
	}
}

func TestServerSendMasked(t *testing.T) {
	opench := make(chan *testevent, 1)
	closech := make(chan *testevent, 1)

	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn *Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
		},
		WithWriteBufferDeadline(time.Millisecond),
		WithReadDeadline(time.Second*5),
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	recv, rb := testconn(t, address)
	defer recv.Close()

	send, err := timeoutValue(opench, time.Millisecond*100)
	RequireNil(t, err)

	codec := newCodec()
	data := make([]byte, 8)

	for i := 0; i < 10; i++ {
		binary.LittleEndian.PutUint64(data, uint64(i))

		h := header{
			OpCode: opBinary,
			Fin:    true,
			Masked: true,
			Length: 8,
		}

		// mask the data
		cipher(data, h.Mask, 0)

		err = codec.WriteHeader((*send).conn.(*Conn).Conn, h)
		RequireNil(t, err)

		_, err := (*send).conn.(*Conn).Conn.Write(data)
		RequireNil(t, err)

		h.reset()

		buf, err := newCircbuf(4096)
		RequireNil(t, err)

		n, err := rb.Read(buf.next())
		RequireNil(t, err)

		RequireNil(t, buf.advanceWrite(n))

		err = codec.ReadHeader(&h, buf)
		RequireNil(t, err)
		AssertEqual(t, int64(8), h.Length)

		if buf.buffered() < int(h.Length) {
			n, err := io.ReadFull(rb, buf.next())
			RequireNil(t, err)
			RequireNil(t, buf.advanceWrite(n))
		}

		peeked, err := buf.peek(len(data))
		RequireNil(t, err)
		AssertEqual(t, data, peeked)
	}
}

func TestServerReceiveSmall(t *testing.T) {
	opench := make(chan *testevent, 1)
	closech := make(chan *testevent, 1)
	msgchan := make(chan *testevent, 1)

	var counter uint64

	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn *Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnMessage: func(conn *Conn, msg []byte) {
				AssertEqual(t, counter, binary.LittleEndian.Uint64(msg))
				counter++
				msgchan <- &testevent{conn: conn}
			},
		},
		WithReadDeadline(time.Second*5),
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn, _ := testconn(t, address)
	defer conn.Close()

	codec := newCodec()
	data := make([]byte, 8)

	for i := 0; i < 100; i++ {
		err = codec.WriteHeader(conn, header{
			OpCode: opBinary,
			Fin:    true,
			Length: 8,
		})

		RequireNil(t, err)

		binary.LittleEndian.PutUint64(data, uint64(i))

		_, err := conn.Write(data)
		RequireNil(t, err)

		err = timeout(msgchan, time.Millisecond*500)
		RequireNil(t, err)
	}
}

func TestServerReceiveLarge(t *testing.T) {
	opench := make(chan *testevent, 1)
	closech := make(chan *testevent, 1)
	msgchan := make(chan *testevent, 1)

	data := make([]byte, 1<<16)
	rand.Read(data)

	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn *Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnMessage: func(conn *Conn, msg []byte) {
				AssertEqual(t, data, msg)
				msgchan <- &testevent{conn: conn}
			},
		},
		WithReadDeadline(time.Second*5),
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn, _ := testconn(t, address)
	defer conn.Close()

	codec := newCodec()

	for i := 0; i < 100; i++ {
		err = codec.WriteHeader(conn, header{
			OpCode: opBinary,
			Fin:    true,
			Length: int64(len(data)),
		})

		RequireNil(t, err)

		_, err := conn.Write(data)
		RequireNil(t, err)
		RequireNil(t, timeout(msgchan, time.Millisecond*500))
	}
}

func TestServerReceiveMasked(t *testing.T) {
	opench := make(chan *testevent, 1)
	closech := make(chan *testevent, 1)
	msgchan := make(chan *testevent, 1)

	data := make([]byte, 1<<16)
	rand.Read(data)

	s := New(
		&Handler{
			OnConnect: func(conn *Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn *Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnMessage: func(conn *Conn, msg []byte) {
				AssertEqual(t, data, msg)
				msgchan <- &testevent{conn: conn}
			},
		},
		WithReadDeadline(time.Second*5),
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn, _ := testconn(t, address)
	defer conn.Close()

	codec := newCodec()

	for i := 0; i < 100; i++ {
		mcopy := make([]byte, len(data))
		copy(mcopy, data)

		h := header{
			OpCode: opBinary,
			Fin:    true,
			Masked: true,
			Length: int64(len(data)),
		}

		// mask the data
		cipher(mcopy, h.Mask, 0)

		err = codec.WriteHeader(conn, h)
		RequireNil(t, err)

		_, err := conn.Write(mcopy)
		RequireNil(t, err)
		RequireNil(t, timeout(msgchan, time.Millisecond*500))
	}
}

func TestServerAutobahn(t *testing.T) {
	if os.Getenv("AUTOBAHN") != "1" {
		t.Skip("skipping autobahn testsuite")
	}

	h := &Handler{
		OnBinary: func(conn *Conn, msg []byte) {
			conn.Write(msg)
		},
		OnText: func(conn *Conn, msg string) {
			conn.WriteText(msg)
		},
		OnError: func(err error, fatal bool) {
			RequireFalse(t, fatal)
		},
	}

	// start echo server
	err := New(
		h,
		WithReadDeadline(time.Minute),
		WithWriteBufferDeadline(time.Millisecond),
	).Serve(8000)

	RequireNil(t, err)

	// write config to tempdir
	workdir := randomdir()
	fmt.Println("outputting to", workdir)
	os.Mkdir(filepath.Join(workdir, "config"), 0755)
	os.Mkdir(filepath.Join(workdir, "reports"), 0755)

	excluded := []string{"12.*", "13.*"}

	if os.Getenv("CI") != "" {
		excluded = append(excluded, "9.*")
	}

	config, _ := json.Marshal(map[string]interface{}{
		"outdir": "./reports/servers",
		"servers": []map[string]interface{}{
			{
				"url": "ws://127.0.0.1:8000",
			},
		},
		"cases":         []string{"*"},
		"exclude-cases": excluded,
	})

	err = os.WriteFile(filepath.Join(workdir, "config", "fuzzingclient.json"), config, 0644)
	RequireNil(t, err)

	// run autobahn testsuite container
	cmd := exec.Command(
		"docker",
		"run",
		"-t",
		"--rm",
		"--network=host",
		"-v",
		fmt.Sprintf("%s:/config", filepath.Join(workdir, "config")),
		"-v",
		fmt.Sprintf("%s:/reports", filepath.Join(workdir, "reports")),
		"crossbario/autobahn-testsuite",
		"wstest",
		"-m",
		"fuzzingclient",
		"-s",
		"/config/fuzzingclient.json",
	)

	stdout, err := cmd.StdoutPipe()
	RequireNil(t, err)
	output := bufio.NewScanner(stdout)

	err = cmd.Start()
	RequireNil(t, err)

	var waiting sync.Map

	for output.Scan() {
		// TODO detect test case and check report json
		if strings.HasPrefix(output.Text(), "Running test case ID") {
			testcase := strings.Split(output.Text(), " ")[4]
			go func(testcase string) {
				rc := make(chan bool, 1)
				waiting.Store(testcase, rc)
				t.Run(testcase, func(t *testing.T) {
					if !<-rc {
						t.Fail()
					}
				})
				rc <- true
			}(testcase)
		}
	}

	err = cmd.Wait()
	RequireNil(t, err)

	fd, err := os.Open(fmt.Sprintf("%s/reports/servers/index.json", workdir))
	RequireNil(t, err)

	data, err := io.ReadAll(fd)
	RequireNil(t, err)

	var report testResults

	err = json.Unmarshal(data, &report)
	RequireNil(t, err)

	for testcase, results := range report.UnknownServer {
		waiter, _ := waiting.Load(testcase)
		if results.Behavior == "FAILED" || results.BehaviorClose == "FAILED" {
			waiter.(chan bool) <- false

			data, _ := os.ReadFile(fmt.Sprintf(
				"%s/reports/servers/unknownserver_case_%s.html",
				workdir,
				strings.ReplaceAll(testcase, ".", "_"),
			))

			fmt.Println(base64.RawURLEncoding.EncodeToString(data))

		} else {
			waiter.(chan bool) <- true
		}
		<-waiter.(chan bool)
	}
}

type testResults struct {
	UnknownServer map[string]testCase `json:"UnknownServer"`
}

type testCase struct {
	Behavior      string `json:"behavior"`
	BehaviorClose string `json:"behaviorClose"`
}

func timeout[T any](ch chan T, after time.Duration) error {
	select {
	case <-ch:
		return nil
	case <-time.After(after):
		return errors.New("timeout")
	}
}

func timeoutValue[T any](ch chan T, after time.Duration) (*T, error) {
	select {
	case v := <-ch:
		return &v, nil
	case <-time.After(after):
		return nil, errors.New("timeout")
	}
}

func randomdir() string {
	random := make([]byte, 10)
	rand.Read(random)
	dir := filepath.Join("/tmp", hex.EncodeToString(random))
	os.Mkdir(dir, 0755)
	return dir
}

func testconn(t *testing.T, address string) (net.Conn, *bufio.Reader) {
	req, err := http.NewRequest("GET", "ws://"+address, nil)
	RequireNil(t, err)

	d := net.Dialer{
		KeepAlive: time.Duration(-1),
	}

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := d.Dial("tcp", address)
	RequireNil(t, err)

	err = conn.SetDeadline(time.Time{})
	RequireNil(t, err)

	err = req.Write(conn)
	RequireNil(t, err)

	// buf := bytes.NewBuffer(make([]byte, 0, 1024))
	// tbf := io.TeeReader(conn, buf)

	rb := bufio.NewReaderSize(conn, 1<<14)

	resp, err := http.ReadResponse(rb, req)
	RequireNil(t, err)
	AssertEqual(t, "Upgrade", resp.Header.Get("Connection"))
	AssertEqual(t, "websocket", resp.Header.Get("Upgrade"))

	resp.Close = false

	return conn, rb
}

func AssertNil(t *testing.T, v any) {
	if v != nil {
		t.Fail()
	}
}

func RequireNil(t *testing.T, v any) {
	if v != nil {
		t.FailNow()
	}
}

func RequireNotNil(t *testing.T, v any) {
	if v == nil {
		t.FailNow()
	}
}

func AssertEqual(t *testing.T, e, a any) {
	eb, eok := e.([]byte)
	ab, aok := a.([]byte)

	if eok && aok {
		if !bytes.Equal(eb, ab) {
			t.Fail()
		}
	} else if e != a {
		t.Fail()
	}
}

func RequireEqual(t *testing.T, e, a any) {
	eb, eok := e.([]byte)
	ab, aok := a.([]byte)

	if eok && aok {
		if !bytes.Equal(eb, ab) {
			t.FailNow()
		}
	} else if e != a {
		t.FailNow()
	}
}

func AssertTrue(t *testing.T, b bool) {
	if !b {
		t.Fail()
	}
}

func RequireTrue(t *testing.T, b bool) {
	if !b {
		t.FailNow()
	}
}

func AssertFalse(t *testing.T, b bool) {
	if b {
		t.Fail()
	}
}

func RequireFalse(t *testing.T, b bool) {
	if b {
		t.FailNow()
	}
}
