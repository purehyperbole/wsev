package wsev

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	HeapRemoved    int32 = -1
	HeapUnassigned int32 = -2
)

// holds a reader, frame and message buffer
type rbuf struct {
	h header
	b *circbuf
	f *bytes.Buffer
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
	writeDeadline  time.Duration      // write deadline
	mutex          sync.Mutex         // write buffer lock
	heapindex      atomic.Int32       // index of this connection in timer heap
	iscontinuation int32              // marks a continuation frame
	isclosed       int32              // marks a connection as closed
	isupgraded     int32              // marks the connection been upgraded
	xforwardedfor  string             // x-forwarded-for from request headers
	scheduled      bool               // marks for flush scheduling
}

func newBufConn(conn net.Conn, readbufpool, writebufpool *sync.Pool, flush, writeDeadline time.Duration, shutdownCallback func(*Conn, error)) *Conn {
	c := &Conn{
		Conn:          conn,
		fd:            connectionFd(conn),
		readbufpool:   readbufpool,
		writebufpool:  writebufpool,
		flush:         flush,
		writeDeadline: writeDeadline,
		quit:          shutdownCallback,
	}

	c.heapindex.Store(HeapUnassigned)

	return c
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
	// try to mark our connection as closed
	if !c.close() {
		return ErrConnectionAlreadyClosed
	}

	c.mutex.Lock()

	defer func() {
		if c.writebuf != nil {
			c.writebuf.b.Reset(nil)
			c.writebufpool.Put(c.writebuf)
			c.writebuf = nil
		}
		c.mutex.Unlock()

		if disconnect {
			c.quit(c, nil)
		}
	}()

	// if we don't have a write buffer, then get one from the pool
	if c.writebuf == nil {
		c.writebuf = c.writebufpool.Get().(*wbuf)
		c.writebuf.b.Reset(c.Conn)
	}

	c.armWriteDeadline()

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
		return unix.Shutdown(c.fd, unix.SHUT_RDWR)
	}

	return nil
}

// CloseImmediatelyWith flushes any buffered state and sends close frame to the connection immediately.
// if disconnect is specified as true, the underlying connection will be closed immediately
func (c *Conn) CloseImmediatelyWith(status CloseStatus, reason string, disconnect bool) error {
	// try to mark our connection as closed
	if !c.close() {
		return ErrConnectionAlreadyClosed
	}

	c.mutex.Lock()

	defer func() {
		if c.writebuf != nil {
			c.writebuf.b.Reset(nil)
			c.writebufpool.Put(c.writebuf)
			c.writebuf = nil
		}
		c.mutex.Unlock()

		if disconnect {
			c.quit(c, nil)
		}
	}()

	payload := make([]byte, len(reason)+2)

	binary.BigEndian.PutUint16(payload, uint16(status))
	copy(payload[2:], reason)

	err := c.writeControlLocked(opClose, payload)
	if err != nil {
		return err
	}

	if disconnect {
		// we call this as calling conn.Close does not actually
		// correctly shutdown the connection. We directly call
		// shutdown() to signal to the client the connection
		// is being closed. conn.Close does not send a tcp FIN
		// or FIN ACK packet. possibly a bug?)
		return unix.Shutdown(c.fd, unix.SHUT_RDWR)
	}

	return nil
}

func (c *Conn) Close() error {
	c.close()
	c.quit(c, nil)
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

// XForwardedFor returns the value for X-Forwarded-For from the initial requests header
func (c *Conn) XForwardedFor() string {
	return c.xforwardedfor
}

func (c *Conn) buffer() error {
	if c.closed() {
		return ErrConnectionAlreadyClosed
	}

	buf := c.acquireReadBuffer()

	// check our buffer isnt full
	if buf.b.full() {
		return bufio.ErrBufferFull
	}

	// read some data into our read buffer
	n, err := syscall.Read(c.fd, buf.b.next())
	if err != nil {
		return err
	}

	if n < 1 {
		return io.EOF
	}

	// advance our buffers write position
	// by the number of bytes we've just read
	return buf.b.advanceWrite(n)
}

func (c *Conn) write(op opCode, size int, wcb func(buf *bufio.Writer) (int, error)) (int, error) {
	c.mutex.Lock()

	n, err := c.writeLocked(op, size, wcb)
	if err != nil && err != ErrConnectionAlreadyClosed {
		if c.writebuf != nil {
			c.writebuf.b.Reset(nil)
			c.writebufpool.Put(c.writebuf)
			c.writebuf = nil
		}

		c.close()
		c.mutex.Unlock()
		c.quit(c, err)

		return n, err
	}

	c.mutex.Unlock()

	return n, err
}

func (c *Conn) writeLocked(op opCode, size int, wcb func(buf *bufio.Writer) (int, error)) (int, error) {
	// try to mark our connection as closed
	if c.closed() {
		return 0, ErrConnectionAlreadyClosed
	}

	// if we don't have a write buffer, then get one from the pool
	if c.writebuf == nil {
		c.writebuf = c.writebufpool.Get().(*wbuf)
		c.writebuf.b.Reset(c.Conn)
	}

	c.armWriteDeadline()

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

		time.AfterFunc(c.flush, c.flushScheduled)
	}

	return size, nil
}

func (c *Conn) armWriteDeadline() {
	if c.writeDeadline > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.writeDeadline))
	}
}

func (c *Conn) flushScheduled() {
	c.mutex.Lock()

	var err error

	if c.writebuf != nil {
		if c.writebuf.b.Buffered() > 0 {
			c.armWriteDeadline()
			err = c.writebuf.b.Flush()
		}

		c.writebuf.b.Reset(nil)
		c.writebufpool.Put(c.writebuf)
		c.writebuf = nil
	}

	c.scheduled = false
	c.mutex.Unlock()

	if err != nil {
		c.close()
		c.quit(c, err)
	}
}

func (c *Conn) writeControl(op opCode, payload []byte) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return c.writeControlLocked(op, payload)
}

func (c *Conn) writeControlLocked(op opCode, payload []byte) error {
	c.armWriteDeadline()

	h := header{
		OpCode: op,
		Fin:    true,
		Length: int64(len(payload)),
	}

	if c.writebuf != nil && c.writebuf.b.Buffered() > 0 {
		err := c.writebuf.c.WriteHeader(c.writebuf.b, h)
		if err != nil {
			return err
		}

		_, err = c.writebuf.b.Write(payload)
		if err != nil {
			return err
		}

		return c.writebuf.b.Flush()
	}

	hd, err := newWriteCodec().BuildHeader(h)
	if err != nil {
		return err
	}

	b := net.Buffers{
		hd,
		payload,
	}

	_, err = b.WriteTo(c.Conn)

	return err
}

func (c *Conn) acquireReadBuffer() *rbuf {
	if c.readbuf == nil {
		c.readbuf = c.readbufpool.Get().(*rbuf)
	}

	return c.readbuf
}

func (c *Conn) releaseReadBuffer() {
	if c.readbuf == nil {
		return
	}

	c.readbuf.h.reset()
	c.readbuf.b.reset()
	c.readbuf.f.Reset()
	c.readbuf.m.Reset()
	c.readbufpool.Put(c.readbuf)
	c.readbuf = nil
}

func (c *Conn) tryReleaseReadBuffer() {
	if c.readbuf == nil || c.readbuf.h.remaining > 0 || c.readbuf.b.buffered() > 0 || c.readbuf.f.Len() > 0 || c.readbuf.m.Len() > 0 {
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
	return atomic.CompareAndSwapInt32(&c.isupgraded, 0, 1)
}

func (c *Conn) upgraded() bool {
	return atomic.LoadInt32(&c.isupgraded) == 1
}
