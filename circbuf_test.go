package wsev

import (
	"crypto/rand"
	"testing"
)

func TestCircbuf(t *testing.T) {
	buf, err := newCircbuf(DefaultBufferSize)
	RequireNil(t, err)

	valueA := make([]byte, 88)
	rand.Read(valueA)

	valueB := make([]byte, 349)
	rand.Read(valueB)

	// check the empty buffer
	next := buf.next()
	RequireEqual(t, DefaultBufferSize, len(next))

	err = buf.read(make([]byte, 1))
	RequireNotNil(t, err)

	// write some data to the buffer
	err = buf.write(valueA)
	RequireNil(t, err)

	buffered := buf.buffered()
	RequireEqual(t, len(valueA), buffered)

	next = buf.next()
	RequireEqual(t, DefaultBufferSize-len(valueA), len(next))

	err = buf.write(valueB)
	RequireNil(t, err)

	buffered = buf.buffered()
	RequireEqual(t, len(valueA)+len(valueB), buffered)

	next = buf.next()
	RequireEqual(t, DefaultBufferSize-(len(valueA)+len(valueB)), len(next))

	// read some data from the buffer
	data := make([]byte, 88)

	err = buf.read(data)
	RequireNil(t, err)
	AssertEqual(t, valueA, data)

	data = make([]byte, 349)

	err = buf.read(data)
	RequireNil(t, err)
	AssertEqual(t, valueB, data)

	buffered = buf.buffered()
	RequireEqual(t, 0, buffered)

	var written int

	// fill the buffer up and overflow it
	for err == nil {
		err = buf.write(valueB)
		written++
	}

	RequireEqual(t, errBufferFull, err)
	RequireFalse(t, buf.full())

	valueC := make([]byte, buf.available())
	rand.Read(valueC)

	err = buf.write(valueC)
	RequireNil(t, err)

	// try and peek some data
	peeked, err := buf.peek(len(valueB))
	RequireNil(t, err)
	AssertEqual(t, valueB, peeked)

	// advance after peeking data
	RequireNil(t, buf.advanceRead(len(peeked)))

	// read all the data again
	for i := 0; i < written-2; i++ {
		err = buf.read(data)
		RequireNil(t, err)
	}

	buffered = buf.buffered()
	next = buf.next()
	RequireEqual(t, len(valueC), buffered)
	RequireEqual(t, DefaultBufferSize-len(valueC), len(next))
}
