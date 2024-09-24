package wsev

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

var (
	errBufferFull  = errors.New("circular buffer full")
	errBufferEmpty = errors.New("circular buffer empty or not enough data buffered")
)

// a circular buffer that uses a virtual memory trick
// that allows us to write contiguously without having
// to handle wrap around ourselves
type circbuf struct {
	address uintptr
	fd      int
	size    int
	head    int
	tail    int
}

func newCircbuf(size int) (*circbuf, error) {
	if size%os.Getpagesize() != 0 {
		return nil, errors.New("buffer size not a multiple of the OS page size")
	}

	fd, err := unix.MemfdCreate("wsev buf", 0)
	if err != nil {
		return nil, err
	}

	err = unix.Ftruncate(fd, int64(size))
	if err != nil {
		return nil, err
	}

	// based on:
	// https://lo.calho.st/posts/black-magic-buffer/

	// ask mmap for an address at a location where we can put both virtual copies of the buffer
	addr, err := mmap(
		0,
		uintptr(size*2),
		syscall.PROT_NONE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS,
		-1,
		0,
	)

	if err != nil {
		return nil, err
	}

	// map the buffer at that address
	_, err = mmap(
		addr,
		uintptr(size),
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED|syscall.MAP_FIXED,
		fd,
		0,
	)

	if err != nil {
		return nil, err
	}

	// now map it again, in the next virtual page
	_, err = mmap(
		addr+uintptr(size),
		uintptr(size),
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED|syscall.MAP_FIXED,
		fd,
		0,
	)

	if err != nil {
		return nil, err
	}

	c := &circbuf{
		address: addr,
		fd:      fd,
		size:    size,
	}

	runtime.SetFinalizer(c, func(c *circbuf) {
		err := munmap(c.address+uintptr(c.size), uintptr(c.size))
		if err != nil {
			panic(err)
		}

		err = munmap(c.address, uintptr(c.size))
		if err != nil {
			panic(err)
		}

		// TODO cleanup mappings...
		err = munmap(c.address, uintptr(c.size*2))
		if err != nil {
			panic(err)
		}

		err = unix.Close(c.fd)
		if err != nil {
			panic(err)
		}
	})

	return c, nil
}

func (b *circbuf) buffered() int {
	return b.tail - b.head
}

func (b *circbuf) available() int {
	return b.size - (b.tail - b.head)
}

func (b *circbuf) full() bool {
	return b.tail-b.head == b.size
}

func (b *circbuf) write(data []byte) error {
	if b.size-(b.tail-b.head) < len(data) {
		return errBufferFull
	}

	// create a temporary slice to the tail of our buffers mapping
	buffer := unsafe.Slice(
		(*byte)(unsafe.Pointer(b.address+uintptr(b.tail))),
		len(data),
	)

	copy(buffer, data)
	b.tail = b.tail + len(data)

	return nil
}

func (b *circbuf) next() []byte {
	return unsafe.Slice(
		(*byte)(unsafe.Pointer(b.address+uintptr(b.tail))),
		b.available(),
	)
}

func (b *circbuf) Read(data []byte) (int, error) {
	if len(data) < b.buffered() {
		return len(data), b.read(data)
	}

	return b.buffered(), b.read(data[:b.buffered()])
}

func (b *circbuf) read(data []byte) error {
	if b.tail-b.head < len(data) {
		return errBufferEmpty
	}

	buffer := unsafe.Slice(
		(*byte)(unsafe.Pointer(b.address+uintptr(b.head))),
		len(data),
	)

	copy(data, buffer)
	b.head = b.head + len(data)

	if b.head > b.size {
		b.head = b.head - b.size
		b.tail = b.tail - b.size
	}

	return nil
}

func (b *circbuf) peek(size int) ([]byte, error) {
	if b.tail-b.head < size {
		return nil, errBufferEmpty
	}

	return unsafe.Slice(
		(*byte)(unsafe.Pointer(b.address+uintptr(b.head))),
		size,
	), nil
}

func (b *circbuf) advanceRead(count int) error {
	if b.tail-b.head < count {
		return errBufferEmpty
	}

	b.head = b.head + count

	if b.head > b.size {
		b.head = b.head - b.size
		b.tail = b.tail - b.size
	}

	return nil
}

func (b *circbuf) advanceWrite(count int) error {
	if b.size-(b.tail-b.head) < count {
		return errBufferFull
	}

	b.tail = b.tail + count

	return nil
}

func (b *circbuf) reset() {
	b.head = b.tail
}

func mmap(addr uintptr, length uintptr, prot int, flags int, fd int, offset int64) (uintptr, error) {
	var r0 uintptr
	var e1 syscall.Errno

	if addr != 0 {
		r0, _, e1 = syscall.Syscall6(syscall.SYS_MMAP, uintptr(addr), uintptr(length), uintptr(prot), uintptr(flags), uintptr(fd), uintptr(offset))
	} else {
		r0, _, e1 = syscall.Syscall6(syscall.SYS_MMAP, uintptr(0), uintptr(length), uintptr(prot), uintptr(flags), uintptr(fd), uintptr(offset))
	}

	return uintptr(r0), errnoErr(e1)
}

func munmap(addr, length uintptr) error {
	_, _, e1 := syscall.Syscall(syscall.SYS_MUNMAP, uintptr(addr), uintptr(length), 0)
	return errnoErr(e1)
}

var (
	errEAGAIN error = syscall.EAGAIN
	errEINVAL error = syscall.EINVAL
	errENOENT error = syscall.ENOENT
)

func errnoErr(e syscall.Errno) error {
	switch e {
	case 0:
		return nil
	case syscall.EAGAIN:
		return errEAGAIN
	case syscall.EINVAL:
		return errEINVAL
	case syscall.ENOENT:
		return errENOENT
	}
	return e
}
