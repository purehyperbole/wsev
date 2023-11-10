package wsev

import (
	"bytes"
	"net"
	"sync"
	"time"
)

// holds a write buffer and codec
type wbuf struct {
	b *bytes.Buffer
	c *codec
}

// wrapped net.Conn with on demand buffers that implements the net.Conn interface
type bufconn struct {
	net.Conn                       // the wrapped conn
	p        *sync.Pool            // the write buffer pool
	b        *wbuf                 // the write buffer containing a write buffer and codec
	m        sync.Mutex            // the write lock
	f        time.Duration         // the time to flush the buffer after
	l        int                   // the size to flush the buffer after
	s        bool                  // is this connection scheduled to be flushed already?
	c        bool                  // is this connection closed after an error?
	e        func(net.Conn, error) // callback to call upon flushing
}

func newBufConn(conn net.Conn, bufpool *sync.Pool, flush time.Duration, bufsize int, errCallback func(net.Conn, error)) *bufconn {
	return &bufconn{
		Conn: conn,
		p:    bufpool,
		f:    flush,
		l:    bufsize,
		e:    errCallback,
	}
}

// Write writes data to the connection via a buffer
// Write can be made to time out and return an error after a fixed
// time limit; see SetDeadline and SetWriteDeadline.
func (c *bufconn) Write(b []byte) (int, error) {
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
		OpCode: opBinary,
		Fin:    true,
		Length: int64(len(b)),
	})
	if err != nil {
		return 0, err
	}

	n, err := c.b.b.Write(b)
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
			_, err = c.b.b.WriteTo(c.Conn)
			if err != nil {
				// mark the connection as closed and call the error callback
				c.c = true
				c.e(c, err)
			}
		})
	}

	return len(b), nil
}
