package wsev

import (
	"bytes"
	"encoding/binary"
	"net"
	"sync"
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
	net.Conn                    // the wrapped conn
	p        *sync.Pool         // the write buffer pool
	b        *wbuf              // the write buffer containing a write buffer and codec
	m        sync.Mutex         // the write lock
	f        time.Duration      // the time to flush the buffer after
	l        int                // the size to flush the buffer after
	s        bool               // is this connection scheduled to be flushed already?
	c        bool               // is this connection closed after an error?
	q        func(*Conn, error) // callback to call upon closing the connection
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

// CloseWith closes a connection with a given status
func (c *Conn) CloseWith(status CloseStatus, reason []byte) (int, error) {
	c.m.Lock()

	var cbuf *wbuf

	defer func() {
		// call the callback to close the connection and remove it from epoll
		c.q(c, nil)
		c.p.Put(cbuf)
		c.m.Unlock()
	}()

	// TODO should flush existing state?

	// mark the connection as closed
	c.c = true

	if c.b != nil {
		cbuf = c.b
	} else {
		cbuf = c.p.Get().(*wbuf)
		cbuf.b.Reset()
	}

	err := cbuf.c.WriteHeader(cbuf.b, header{
		OpCode: opClose,
		Fin:    true,
		Length: int64(len(reason) + 2),
	})
	if err != nil {
		return 0, err
	}

	// write directly to the underlying connection
	binary.BigEndian.PutUint16(cbuf.s[:], uint16(status))

	n, err := cbuf.b.Write(cbuf.s[:])
	if err != nil {
		return int(n), err
	}

	n, err = cbuf.b.Write(reason)
	if err != nil {
		return n, err
	}

	w, err := cbuf.b.WriteTo(c.Conn)
	if err != nil {
		return int(w), err
	}

	fd, err := connectionFd(c.Conn)
	if err != nil {
		return int(0), err
	}

	// we call this as calling conn.Close does not actually
	// correctly shutdown the connection. We directly call
	// shutdown() to signal to the client the connection
	// is being closed. conn.Close does not send a tcp FIN
	// or FIN ACK packet. possibly a bug?)
	return int(w), unix.Shutdown(fd, unix.SHUT_RDWR)
}

func (c *Conn) write(op opCode, size int, wcb func(buf *bytes.Buffer) (int, error)) (int, error) {
	c.m.Lock()
	defer c.m.Unlock()

	// our connection has already been closed by the flush timer
	if c.c {
		return -1, ErrConnectionAlreadyClosed
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
				c.c = true
				c.q(c, err)
			}
		})
	}

	return size, nil
}
