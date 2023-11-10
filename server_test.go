package wsev

import (
	"bufio"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testevent struct {
	conn net.Conn
	msg  []byte
	err  error
}

func TestServerConnect(t *testing.T) {
	opench := make(chan *testevent, 1)
	closech := make(chan *testevent, 1)
	msgch := make(chan *testevent, 1)

	s := New(
		&Handler{
			OnConnect: func(conn net.Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn net.Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnMessage: func(conn net.Conn, msg []byte) {
				msgch <- &testevent{conn: conn, msg: msg}
			},
		},
	)

	err := s.Serve(9000)
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://localhost:9000", nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", "localhost:9000")
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
			OnConnect: func(conn net.Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn net.Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnMessage: func(conn net.Conn, msg []byte) {
				msgch <- &testevent{conn: conn, msg: msg}
			},
		},
	)

	err := s.Serve(9000)
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://localhost:9000", nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", "localhost:9000")
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
			OnConnect: func(conn net.Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn net.Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnMessage: func(conn net.Conn, msg []byte) {
				msgch <- &testevent{conn: conn, msg: msg}
			},
		},
		WithReadDeadline(time.Millisecond*100),
	)

	err := s.Serve(9000)
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://localhost:9000", nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", "localhost:9000")
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
			OnConnect: func(conn net.Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn net.Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnPing: func(conn net.Conn) {
				pingch <- &testevent{conn: conn}
			},
		},
		WithReadDeadline(time.Millisecond*100),
	)

	err := s.Serve(9000)
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://localhost:9000", nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", "localhost:9000")
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

func TestServerSendSmall(t *testing.T) {
	opench := make(chan *testevent, 1)
	closech := make(chan *testevent, 1)
	msgchan := make(chan *testevent, 1)

	var counter uint64

	s := New(
		&Handler{
			OnConnect: func(conn net.Conn) {
				opench <- &testevent{conn: conn}
			},
			OnDisconnect: func(conn net.Conn, err error) {
				closech <- &testevent{conn: conn, err: err}
			},
			OnMessage: func(conn net.Conn, msg []byte) {
				assert.Equal(t, counter, binary.LittleEndian.Uint64(msg))
				counter++
				msgchan <- &testevent{conn: conn}
			},
		},
	)

	err := s.Serve(9000)
	require.Nil(t, err)
	defer s.Close()

	req, err := http.NewRequest("GET", "ws://localhost:9000", nil)
	require.Nil(t, err)

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", enc.EncodeToString(make([]byte, 16)))

	conn, err := net.Dial("tcp", "localhost:9000")
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
