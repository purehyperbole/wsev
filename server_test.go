package wsev

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testevent struct {
	conn net.Conn
	err  error
	msg  []byte
}

var testport int = 10000 //8000

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
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn := testconn(t, address)
	defer conn.Close()
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
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn := testconn(t, address)
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
	t.Skip("implement manual scan of connections using last read times and heap")

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
		WithReadDeadline(time.Millisecond*100),
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://"+address, nil)
	RequireNil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", address)
	RequireNil(t, err)
	defer conn.Close()

	err = req.Write(conn)
	RequireNil(t, err)

	RequireNil(t, timeout(opench, time.Millisecond*100))
	RequireNil(t, timeout(closech, time.Millisecond*100))
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
		WithReadDeadline(time.Millisecond*100),
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn := testconn(t, address)
	defer conn.Close()

	codec := newCodec()

	err = codec.WriteHeader(conn, header{
		OpCode: opPing,
		Fin:    true,
	})

	RequireNil(t, err)
	RequireNil(t, timeout(pingch, time.Millisecond*100))
	conn.SetReadDeadline(time.Now().Add(time.Millisecond * 100))

	h, err := codec.ReadHeader(conn)
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
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	recv := testconn(t, address)
	defer recv.Close()

	send, err := timeoutValue(opench, time.Millisecond*100)
	RequireNil(t, err)

	codec := newCodec()
	data := make([]byte, 8)

	for i := 0; i < 10; i++ {
		binary.LittleEndian.PutUint64(data, uint64(i))

		_, err := (*send).conn.Write(data)
		RequireNil(t, err)

		h, err := codec.ReadHeader(recv)
		RequireNil(t, err)
		AssertEqual(t, int64(8), h.Length)

		rdata := make([]byte, h.Length)
		recv.Read(rdata)
		AssertEqual(t, data, rdata)
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
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	recv := testconn(t, address)
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

		h, err = codec.ReadHeader(recv)
		RequireNil(t, err)
		AssertEqual(t, int64(8), h.Length)

		rdata := make([]byte, h.Length)
		recv.Read(rdata)
		AssertEqual(t, data, rdata)
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
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn := testconn(t, address)
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
		RequireNil(t, timeout(msgchan, time.Millisecond*100))
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
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn := testconn(t, address)
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
	)

	port, address := nexttestport()

	err := s.Serve(port)
	RequireNil(t, err)
	defer s.Close()

	conn := testconn(t, address)
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
		WithReadDeadline(time.Second),
		WithWriteBufferDeadline(time.Duration(time.Millisecond)),
	).Serve(8000)

	RequireNil(t, err)

	// write config to tempdir
	workdir := randomdir()
	fmt.Println("outputting to", workdir)
	os.Mkdir(filepath.Join(workdir, "config"), 0755)
	os.Mkdir(filepath.Join(workdir, "reports"), 0755)

	config, _ := json.Marshal(map[string]interface{}{
		"outdir": "./reports/servers",
		"servers": []map[string]interface{}{
			{
				"url": "ws://127.0.0.1:8000",
			},
		},
		"cases":         []string{"*"},
		"exclude-cases": []string{"12.*", "13.*"},
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

	for output.Scan() {
		// TODO detect test case and check report json
		if strings.HasPrefix(output.Text(), "Running test case ID") {
			testcase := strings.Split(output.Text(), " ")[4]
			t.Run(testcase, func(t *testing.T) {
			})
		}
	}

	err = cmd.Wait()
	RequireNil(t, err)
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

func testconn(t *testing.T, address string) net.Conn {
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

	rb := bufio.NewReaderSize(conn, 1<<14)

	resp, err := http.ReadResponse(rb, req)
	RequireNil(t, err)
	AssertEqual(t, "Upgrade", resp.Header.Get("Connection"))
	AssertEqual(t, "websocket", resp.Header.Get("Upgrade"))

	resp.Close = false

	time.Sleep(time.Millisecond * 10)

	return conn
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
