package wsev

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testevent struct {
	conn net.Conn
	err  error
	msg  []byte
}

var testport int = 8000

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
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://"+address, nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", address)
	require.Nil(t, err)
	defer conn.Close()

	err = req.Write(conn)
	require.Nil(t, err)
	require.Nil(t, timeout(opench, time.Millisecond*100))

	sh := sha1.New()
	sh.Write([]byte(enc.EncodeToString(make([]byte, 16))))
	sh.Write(wsAcceptID)

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	require.Nil(t, err)
	assert.Equal(t, "Upgrade", resp.Header.Get("Connection"))
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))
	assert.Equal(t, enc.EncodeToString(sh.Sum(nil)), resp.Header.Get("Sec-WebSocket-Accept"))
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
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://"+address, nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", address)
	require.Nil(t, err)
	defer conn.Close()

	err = req.Write(conn)
	require.Nil(t, err)

	require.Nil(t, timeout(opench, time.Millisecond*100))

	codec := newCodec()

	err = codec.WriteHeader(conn, header{
		OpCode: opClose,
		Fin:    true,
	})

	require.Nil(t, err)

	event, err := timeoutValue(closech, time.Millisecond*100)
	require.Nil(t, err)
	assert.Nil(t, (*event).err)
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
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://"+address, nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", address)
	require.Nil(t, err)
	defer conn.Close()

	err = req.Write(conn)
	require.Nil(t, err)

	require.Nil(t, timeout(opench, time.Millisecond*100))
	require.Nil(t, timeout(closech, time.Millisecond*100))
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
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://"+address, nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", address)
	require.Nil(t, err)
	defer conn.Close()

	err = req.Write(conn)
	require.Nil(t, err)
	require.Nil(t, timeout(opench, time.Millisecond*100))

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	require.Nil(t, err)
	assert.Equal(t, "Upgrade", resp.Header.Get("Connection"))
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	codec := newCodec()

	err = codec.WriteHeader(conn, header{
		OpCode: opPing,
		Fin:    true,
	})

	require.Nil(t, err)
	require.Nil(t, timeout(pingch, time.Millisecond*100))
	conn.SetReadDeadline(time.Now().Add(time.Millisecond * 100))

	h, err := codec.ReadHeader(conn)
	require.Nil(t, err)
	assert.Equal(t, opPong, h.OpCode)
	assert.True(t, h.Fin)
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
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://"+address, nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	recv, err := net.Dial("tcp", address)
	require.Nil(t, err)
	defer recv.Close()

	err = req.Write(recv)
	require.Nil(t, err)

	send, err := timeoutValue(opench, time.Millisecond*100)
	require.Nil(t, err)

	resp, err := http.ReadResponse(bufio.NewReader(recv), req)
	require.Nil(t, err)
	assert.Equal(t, "Upgrade", resp.Header.Get("Connection"))
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	codec := newCodec()
	data := make([]byte, 8)

	for i := 0; i < 10; i++ {
		binary.LittleEndian.PutUint64(data, uint64(i))

		_, err := (*send).conn.Write(data)
		require.Nil(t, err)

		h, err := codec.ReadHeader(recv)
		require.Nil(t, err)
		assert.Equal(t, int64(8), h.Length)

		rdata := make([]byte, h.Length)
		recv.Read(rdata)
		assert.Equal(t, data, rdata)
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
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://"+address, nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	recv, err := net.Dial("tcp", address)
	require.Nil(t, err)
	defer recv.Close()

	err = req.Write(recv)
	require.Nil(t, err)

	send, err := timeoutValue(opench, time.Millisecond*100)
	require.Nil(t, err)

	resp, err := http.ReadResponse(bufio.NewReader(recv), req)
	require.Nil(t, err)
	assert.Equal(t, "Upgrade", resp.Header.Get("Connection"))
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

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
		require.Nil(t, err)

		_, err := (*send).conn.(*Conn).Conn.Write(data)
		require.Nil(t, err)

		h, err = codec.ReadHeader(recv)
		require.Nil(t, err)
		assert.Equal(t, int64(8), h.Length)

		rdata := make([]byte, h.Length)
		recv.Read(rdata)
		assert.Equal(t, data, rdata)
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
				assert.Equal(t, counter, binary.LittleEndian.Uint64(msg))
				counter++
				msgchan <- &testevent{conn: conn}
			},
		},
	)

	port, address := nexttestport()

	err := s.Serve(port)
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://"+address, nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", address)
	require.Nil(t, err)
	defer conn.Close()

	err = req.Write(conn)
	require.Nil(t, err)
	require.Nil(t, timeout(opench, time.Millisecond*100))

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	require.Nil(t, err)
	assert.Equal(t, "Upgrade", resp.Header.Get("Connection"))
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	codec := newCodec()
	data := make([]byte, 8)

	for i := 0; i < 100; i++ {
		err = codec.WriteHeader(conn, header{
			OpCode: opBinary,
			Fin:    true,
			Length: 8,
		})

		require.Nil(t, err)

		binary.LittleEndian.PutUint64(data, uint64(i))

		_, err := conn.Write(data)
		require.Nil(t, err)
		require.Nil(t, timeout(msgchan, time.Millisecond*100))
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
				assert.Equal(t, data, msg)
				msgchan <- &testevent{conn: conn}
			},
		},
	)

	port, address := nexttestport()

	err := s.Serve(port)
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://"+address, nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", address)
	require.Nil(t, err)
	defer conn.Close()

	err = req.Write(conn)
	require.Nil(t, err)
	require.Nil(t, timeout(opench, time.Millisecond*100))

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	require.Nil(t, err)
	assert.Equal(t, "Upgrade", resp.Header.Get("Connection"))
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

	codec := newCodec()

	for i := 0; i < 100; i++ {
		err = codec.WriteHeader(conn, header{
			OpCode: opBinary,
			Fin:    true,
			Length: int64(len(data)),
		})

		require.Nil(t, err)

		_, err := conn.Write(data)
		require.Nil(t, err)
		require.Nil(t, timeout(msgchan, time.Millisecond*500))
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
				assert.Equal(t, data, msg)
				msgchan <- &testevent{conn: conn}
			},
		},
	)

	port, address := nexttestport()

	err := s.Serve(port)
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://"+address, nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", address)
	require.Nil(t, err)
	defer conn.Close()

	err = req.Write(conn)
	require.Nil(t, err)
	require.Nil(t, timeout(opench, time.Millisecond*100))

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	require.Nil(t, err)
	assert.Equal(t, "Upgrade", resp.Header.Get("Connection"))
	assert.Equal(t, "websocket", resp.Header.Get("Upgrade"))

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
		require.Nil(t, err)

		_, err := conn.Write(mcopy)
		require.Nil(t, err)
		require.Nil(t, timeout(msgchan, time.Millisecond*500))
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
			require.False(t, fatal)
		},
	}

	// start echo server
	err := New(
		h,
		WithReadDeadline(time.Second),
		WithWriteBufferDeadline(time.Duration(time.Millisecond)),
	).Serve(8000)

	require.Nil(t, err)

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
	require.Nil(t, err)

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
	require.Nil(t, err)
	output := bufio.NewScanner(stdout)

	err = cmd.Start()
	require.Nil(t, err)

	for output.Scan() {
		// TODO detect test case and check report json
		if strings.HasPrefix(output.Text(), "Running test case ID") {
			testcase := strings.Split(output.Text(), " ")[4]
			t.Run(testcase, func(t *testing.T) {
			})
		}
	}

	err = cmd.Wait()
	require.Nil(t, err)
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
