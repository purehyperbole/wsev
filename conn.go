package wsev

import (
	"bytes"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// holds a write buffer and codec
type wbuf struct {
	b *bytes.Buffer
	c *codec
	s [2]byte
}

// A wrapped net.Conn with on demand buffers that implements the net.Conn interface
type Conn struct {
	net.Conn
	p *sync.Pool
	b *wbuf
	q func(*Conn, error)
	f time.Duration
	l int
	m sync.Mutex
	n int32
	c int32
	s bool
}

func newBufConn(conn net.Conn, bufpool *sync.Pool, flush time.Duration, bufsize int, shutdownCallback func(*Conn, error)) *Conn {
	return &Conn{
		Conn: conn,
		p:    bufpool,
		f:    flush,
		l:    bufsize,
		q:    shutdownCallback,
	}
}

// Write writes binary data to the connection via a buffer
// Write can be made to time out and return an error after a fixed
// time limit; see SetDeadline and SetWriteDeadline.
func (c *Conn) Write(b []byte) (int, error) {
	return c.write(opBinary, len(b), func(buf *bytes.Buffer) (int, error) {
		return buf.Write(b)
	})
}

// WriteText writes text data to the connection via a buffer
// Write can be made to time out and return an error after a fixed
// time limit; see SetDeadline and SetWriteDeadline.
func (c *Conn) WriteText(s string) (int, error) {
	return c.write(opText, len(s), func(buf *bytes.Buffer) (int, error) {
		return buf.WriteString(s)
	})
}

// CloseWith writes all existing buffered state and sends close frame to the connection.
// if disconnect is specified as true, the underlying connection will be closed immediately
func (c *Conn) CloseWith(status CloseStatus, reason string, disconnect bool) error {
	c.m.Lock()

	defer func() {
		// call the callback to close the connection and remove it from epoll, if we are disconnecting
		if disconnect {
			c.q(c, nil)
		}
		if c.b != nil {
			c.p.Put(c.b)
		}
		c.m.Unlock()
	}()

	// try to mark our connection as closed
	if !c.close() {
		return ErrConnectionAlreadyClosed
	}

	// if we don't have a write buffer, then get one from the pool
	if c.b == nil {
		c.b = c.p.Get().(*wbuf)
		c.b.b.Reset()
	}

	err := c.b.c.WriteHeader(c.b.b, header{
		OpCode: opClose,
		Fin:    true,
		Length: int64(len(reason) + 2),
	})
	if err != nil {
		return err
	}

	// write directly to the underlying connection
	binary.BigEndian.PutUint16(c.b.s[:], uint16(status))

	_, err = c.b.b.Write(c.b.s[:])
	if err != nil {
		return err
	}

	_, err = c.b.b.WriteString(reason)
	if err != nil {
		return err
	}

	_, err = c.b.b.WriteTo(c.Conn)
	if err != nil {
		return err
	}

	if disconnect {
		// we call this as calling conn.Close does not actually
		// correctly shutdown the connection. We directly call
		// shutdown() to signal to the client the connection
		// is being closed. conn.Close does not send a tcp FIN
		// or FIN ACK packet. possibly a bug?)
		fd, err := connectionFd(c.Conn)
		if err != nil {
			return err
		}

		return unix.Shutdown(fd, unix.SHUT_RDWR)
	}

	return nil
}

// CloseImmediatelyWith sends close frame to the connection immediately, discarding any buffered state.
// if disconnect is specified as true, the underlying connection will be closed immediately
func (c *Conn) CloseImmediatelyWith(status CloseStatus, reason string, disconnect bool) error {
	c.m.Lock()

	defer func() {
		// call the callback to close the connection and remove it from epoll, if we are disconnecting
		if disconnect {
			c.q(c, nil)
		}
		// return the buffer and remove it from this connection
		if c.b != nil {
			c.p.Put(c.b)
			c.b = nil
		}
		c.m.Unlock()
	}()

	// try to mark our connection as closed
	if !c.close() {
		return ErrConnectionAlreadyClosed
	}

	hd, err := newWriteCodec().BuildHeader(header{
		OpCode: opClose,
		Fin:    true,
		Length: int64(len(reason) + 2),
	})

	if err != nil {
		return err
	}

	payload := make([]byte, len(reason)+2)

	// write directly to the underlying connection
	binary.BigEndian.PutUint16(payload, uint16(status))
	copy(payload[2:], reason)

	b := net.Buffers{
		hd,
		payload,
	}

	_, err = b.WriteTo(c.Conn)
	if err != nil {
		return err
	}

	if disconnect {
		// we call this as calling conn.Close does not actually
		// correctly shutdown the connection. We directly call
		// shutdown() to signal to the client the connection
		// is being closed. conn.Close does not send a tcp FIN
		// or FIN ACK packet. possibly a bug?)
		fd, err := connectionFd(c.Conn)
		if err != nil {
			return err
		}

		return unix.Shutdown(fd, unix.SHUT_RDWR)
	}

	return nil
}

func (c *Conn) write(op opCode, size int, wcb func(buf *bytes.Buffer) (int, error)) (int, error) {
	c.m.Lock()
	defer c.m.Unlock()

	// try to mark our connection as closed
	if c.closed() {
		return 0, ErrConnectionAlreadyClosed
	}

	// if we don't have a write buffer, then get one from the pool
	if c.b == nil {
		c.b = c.p.Get().(*wbuf)
		c.b.b.Reset()
	}

	// write the ws header and data to the buffer
	err := c.b.c.WriteHeader(c.b.b, header{
		OpCode: op,
		Fin:    true,
		Length: int64(size),
	})
	if err != nil {
		return 0, err
	}

	n, err := wcb(c.b.b)
	if err != nil {
		return n, err
	}

	// if this buffer is at our high water mark, then flush it to the
	// underlying connection and release the buffer to the pool
	if c.b.b.Len() >= c.l {
		// write all data
		n, err := c.b.b.WriteTo(c.Conn)

		// return the buffer and remove it from this connection
		c.p.Put(c.b)
		c.b = nil

		return int(n), err
	}

	// schedule our buffer to be flushed if it is not already
	if !c.s {
		c.s = true

		time.AfterFunc(c.f, func() {
			c.m.Lock()
			defer func() {
				// return the buffer and remove it from this connection
				if c.b != nil {
					c.p.Put(c.b)
					c.b = nil
				}

				c.s = false
				c.m.Unlock()
			}()

			// someone else has flushed the buffer, so exit
			if c.b == nil {
				return
			}

			if c.b.b.Len() < 1 {
				return
			}

			// write all data
			_, err := c.b.b.WriteTo(c.Conn)
			if err != nil {
				// mark the connection as closed and call the error callback
				c.close()
				c.q(c, err)
			}
		})
	}

	return size, nil
}

func (c *Conn) continuation() opCode {
	return opCode(atomic.LoadInt32(&c.n))
}

func (c *Conn) setContinuation(opCode opCode) bool {
	return atomic.CompareAndSwapInt32(&c.n, 0, int32(opCode))
}

func (c *Conn) resetContinuation() {
	atomic.StoreInt32(&c.n, 0)
}

func (c *Conn) closed() bool {
	return atomic.LoadInt32(&c.c) == 1
}

func (c *Conn) close() bool {
	return atomic.CompareAndSwapInt32(&c.c, 0, 1)
}
