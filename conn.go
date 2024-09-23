package wsev

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	HeapRemoved    = -1
	HeapUnassigned = -2
)

// holds a reader, frame and message buffer
type rbuf struct {
	h header
	f []byte
	//f *bytes.Buffer
	m *bytes.Buffer
}

// holds a write buffer and codec
type wbuf struct {
	b *bufio.Writer
	c *codec
	s [2]byte
}

// A wrapped net.Conn with on demand buffers that implements the net.Conn interface
type Conn struct {
	net.Conn
	fd             int                // file descriptor
	value          any                // user defined value
	readbufpool    *sync.Pool         // read buffer pool
	writebufpool   *sync.Pool         // write buffer pool
	readbuf        *rbuf              // acquired read buffer
	writebuf       *wbuf              // acquired write buffer
	quit           func(*Conn, error) // quit callback
	flush          time.Duration      // flush interval
	mutex          sync.Mutex         // write buffer lock
	heapindex      int                // index of this connection in timer heap
	iscontinuation int32              // marks a continuation frame
	isclosed       int32              // marks a connection as closed
	upgraded       int32              // marks the connection been upgraded
	scheduled      bool               // marks for flush scheduling
}

func newBufConn(conn net.Conn, readbufpool, writebufpool *sync.Pool, flush time.Duration, shutdownCallback func(*Conn, error)) *Conn {
	return &Conn{
		Conn:         conn,
		fd:           connectionFd(conn),
		readbufpool:  readbufpool,
		writebufpool: writebufpool,
		flush:        flush,
		quit:         shutdownCallback,
		heapindex:    HeapUnassigned,
	}
}

// Write writes binary data to the connection via a buffer
// Write can be made to time out and return an error after a fixed
// time limit; see SetDeadline and SetWriteDeadline.
func (c *Conn) Write(b []byte) (int, error) {
	return c.write(opBinary, len(b), func(buf *bufio.Writer) (int, error) {
		return buf.Write(b)
	})
}

// WriteText writes text data to the connection via a buffer
// Write can be made to time out and return an error after a fixed
// time limit; see SetDeadline and SetWriteDeadline.
func (c *Conn) WriteText(s string) (int, error) {
	return c.write(opText, len(s), func(buf *bufio.Writer) (int, error) {
		return buf.WriteString(s)
	})
}

// CloseWith writes all existing buffered state and sends close frame to the connection.
// if disconnect is specified as true, the underlying connection will be closed immediately
func (c *Conn) CloseWith(status CloseStatus, reason string, disconnect bool) error {
	c.mutex.Lock()

	defer func() {
		// call the callback to close the connection and remove it from epoll, if we are disconnecting
		if disconnect {
			c.quit(c, nil)
		}
		if c.writebuf != nil {
			c.writebuf.b.Reset(nil)
			c.writebufpool.Put(c.writebuf)
			c.writebuf = nil
		}
		c.mutex.Unlock()
	}()

	// try to mark our connection as closed
	if !c.close() {
		return ErrConnectionAlreadyClosed
	}

	c.releaseReadBuffer()

	// if we don't have a write buffer, then get one from the pool
	if c.writebuf == nil {
		c.writebuf = c.writebufpool.Get().(*wbuf)
		c.writebuf.b.Reset(c.Conn)
	}

	err := c.writebuf.c.WriteHeader(c.writebuf.b, header{
		OpCode: opClose,
		Fin:    true,
		Length: int64(len(reason) + 2),
	})
	if err != nil {
		return err
	}

	// write directly to the underlying connection
	binary.BigEndian.PutUint16(c.writebuf.s[:], uint16(status))

	_, err = c.writebuf.b.Write(c.writebuf.s[:])
	if err != nil {
		return err
	}

	_, err = c.writebuf.b.WriteString(reason)
	if err != nil {
		return err
	}

	err = c.writebuf.b.Flush()
	if err != nil {
		return err
	}

	if disconnect {
		// we call this as calling conn.Close does not actually
		// correctly shutdown the connection. We directly call
		// shutdown() to signal to the client the connection
		// is being closed. conn.Close does not send a tcp FIN
		// or FIN ACK packet. possibly a bug?)
		return unix.Shutdown(connectionFd(c.Conn), unix.SHUT_RDWR)
	}

	return nil
}

// CloseImmediatelyWith sends close frame to the connection immediately, discarding any buffered state.
// if disconnect is specified as true, the underlying connection will be closed immediately
func (c *Conn) CloseImmediatelyWith(status CloseStatus, reason string, disconnect bool) error {
	c.mutex.Lock()

	defer func() {
		// call the callback to close the connection and remove it from epoll, if we are disconnecting
		if disconnect {
			c.quit(c, nil)
		}
		// return the buffer and remove it from this connection
		if c.writebuf != nil {
			c.writebuf.b.Reset(nil)
			c.writebufpool.Put(c.writebuf)
			c.writebuf = nil
		}
		c.mutex.Unlock()
	}()

	// try to mark our connection as closed
	if !c.close() {
		return ErrConnectionAlreadyClosed
	}

	c.releaseReadBuffer()

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
		return unix.Shutdown(connectionFd(c.Conn), unix.SHUT_RDWR)
	}

	return nil
}

// Get gets a user specified value
func (c *Conn) Get() any {
	return c.value
}

// Set sets a user specified value
func (c *Conn) Set(value any) {
	c.value = value
}

func (c *Conn) buffer() error {
	buf := c.acquireReadBuffer()

	// check our buffer isnt full
	if len(buf.f) == cap(buf.f) {
		return bufio.ErrBufferFull
	}

	// read some data into our read buffer
	n, err := syscall.Read(c.fd, buf.f[len(buf.f):cap(buf.f)])
	if err != nil {
		return err
	}

	// reslice our buffer to account for the new bytes
	buf.f = buf.f[:len(buf.f)+n]

	return nil
}

func (c *Conn) write(op opCode, size int, wcb func(buf *bufio.Writer) (int, error)) (int, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	// try to mark our connection as closed
	if c.closed() {
		return 0, ErrConnectionAlreadyClosed
	}

	// if we don't have a write buffer, then get one from the pool
	if c.writebuf == nil {
		c.writebuf = c.writebufpool.Get().(*wbuf)
		c.writebuf.b.Reset(c.Conn)
	}

	// write the ws header and data to the buffer
	err := c.writebuf.c.WriteHeader(c.writebuf.b, header{
		OpCode: op,
		Fin:    true,
		Length: int64(size),
	})
	if err != nil {
		return 0, err
	}

	n, err := wcb(c.writebuf.b)
	if err != nil {
		return n, err
	}

	// if this buffer has been flushed, then return the buffer to the pool
	if c.writebuf.b.Buffered() < 1 {
		// return the buffer and remove it from this connection
		c.writebuf.b.Reset(nil)
		c.writebufpool.Put(c.writebuf)
		c.writebuf = nil

		return int(n), err
	}

	// schedule our buffer to be flushed if it is not already
	if !c.scheduled {
		c.scheduled = true

		time.AfterFunc(c.flush, func() {
			c.mutex.Lock()
			defer func() {
				// return the buffer and remove it from this connection
				if c.writebuf != nil {
					c.writebuf.b.Reset(nil)
					c.writebufpool.Put(c.writebuf)
					c.writebuf = nil
				}

				c.scheduled = false
				c.mutex.Unlock()
			}()

			// someone else has flushed the buffer, so exit
			if c.writebuf == nil {
				return
			}

			if c.writebuf.b.Buffered() < 1 {
				return
			}

			// write all data
			err := c.writebuf.b.Flush()
			if err != nil {
				// mark the connection as closed and call the error callback
				c.close()
				c.quit(c, err)
			}
		})
	}

	return size, nil
}

func (c *Conn) acquireReadBuffer() *rbuf {
	if c.readbuf == nil {
		c.readbuf = c.readbufpool.Get().(*rbuf)
	}

	return c.readbuf
}

func (c *Conn) releaseReadBuffer() {
	c.readbuf.f = c.readbuf.f[:0]
	c.readbuf.h.reset()
	c.readbuf.m.Reset()
	c.readbufpool.Put(c.readbuf)
	c.readbuf = nil
}

func (c *Conn) tryReleaseReadBuffer() {
	if c.readbuf != nil && len(c.readbuf.f) > 0 {
		return
	}

	c.releaseReadBuffer()
}

func (c *Conn) continuation() opCode {
	return opCode(atomic.LoadInt32(&c.iscontinuation))
}

func (c *Conn) setContinuation(opCode opCode) bool {
	return atomic.CompareAndSwapInt32(&c.iscontinuation, 0, int32(opCode))
}

func (c *Conn) resetContinuation() {
	atomic.StoreInt32(&c.iscontinuation, 0)
}

func (c *Conn) closed() bool {
	return atomic.LoadInt32(&c.isclosed) == 1
}

func (c *Conn) close() bool {
	return atomic.CompareAndSwapInt32(&c.isclosed, 0, 1)
}

func (c *Conn) upgrade() bool {
	return atomic.CompareAndSwapInt32(&c.upgraded, 0, 1)
}
