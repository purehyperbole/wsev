package wsev

import (
	"math/rand"
	"testing"
)

func TestHeapDelete(t *testing.T) {
	var h heap

	r := rand.New(rand.NewSource(1829462))

	cs := make([]*Conn, 10000)

	for i := 0; i < 100; i++ {
		k := r.Intn(10000)
		c := &Conn{}
		c.Set(k)
		cs[k] = c
		h.push(int64(k), c)
	}

	h.delete(cs[4668])
	h.delete(cs[883])
	h.delete(cs[9311])

	var last int

	for i := 0; i < 97; i++ {
		c := h.pop()

		if c.Get().(int) < last {
			t.FailNow()
		}

		last = c.Get().(int)
	}
}

func TestHeapIncrease(t *testing.T) {
	var h heap

	r := rand.New(rand.NewSource(1829462))

	cs := make([]*Conn, 10000)

	for i := 0; i < 100; i++ {
		k := r.Intn(10000)
		c := &Conn{}
		c.Set(k)
		cs[k] = c
		h.push(int64(k), c)
	}

	increased := []*Conn{cs[4668], cs[883], cs[9311]}

	for i := range increased {
		increased[i].Set(10000 + i)
		h.increase(increased[i], int64(10000+i))
	}

	var last int

	for i := 0; i < 97; i++ {
		c := h.pop()

		if c.Get().(int) < last {
			t.FailNow()
		}

		last = c.Get().(int)
	}

	AssertEqual(t, increased[0], h.pop())
	AssertEqual(t, increased[1], h.pop())
	AssertEqual(t, increased[2], h.pop())
}
