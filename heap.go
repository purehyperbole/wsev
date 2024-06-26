package wsev

type entry struct {
	key   int64
	value *Conn
}

type heap []entry

func (h *heap) push(key int64, value *Conn) {
	current := len(*h)
	value.h = current

	*h = append(*h, entry{
		key:   key,
		value: value,
	})

	for h.hasParent(current) {
		parent := parentIndex(current)

		if (*h)[parent].key <= key {
			break
		}

		h.swap(current, parent)
		current = parent
	}
}

func (h *heap) decrease(value *Conn, key int64) {
	current := value.h

	if value.h < 0 {
		return
	}

	(*h)[current].key = key

	for h.hasParent(current) {
		parent := parentIndex(current)

		if (*h)[parent].key <= key {
			break
		}

		h.swap(current, parent)
		current = parent
	}
}

func (h *heap) pop() *Conn {
	if h.isEmpty() {
		return nil
	}

	value := (*h)[0].value
	(*h)[0] = (*h)[len(*h)-1]
	(*h)[0].value.h = 0
	*h = (*h)[:len(*h)-1]

	var current int

	for h.hasLeftChild(current) {
		smallest := leftChildIndex(current)

		if h.hasRightChild(current) && (*h)[rightChildIndex(current)].key < (*h)[smallest].key {
			smallest = rightChildIndex(current)
		}

		if (*h)[current].key < (*h)[smallest].key {
			break
		} else {
			h.swap(current, smallest)
		}

		current = smallest
	}

	value.h = -1

	return value
}

func (h *heap) popIf(cond int64) *Conn {
	if h.isEmpty() || (*h)[0].key > cond {
		return nil
	}

	return h.pop()
}

func (h *heap) swap(a, b int) {
	// swap the index values on the connection
	(*h)[a].value.h = b
	(*h)[b].value.h = a

	(*h)[a], (*h)[b] = (*h)[b], (*h)[a]
}

func (h *heap) isEmpty() bool {
	return len(*h) < 1
}

func (h *heap) hasLeftChild(index int) bool {
	return leftChildIndex(index) < len(*h)
}

func (h *heap) hasRightChild(index int) bool {
	return rightChildIndex(index) < len(*h)
}

func (h *heap) hasParent(index int) bool {
	return parentIndex(index) > 0
}

func leftChildIndex(index int) int {
	return (index * 2) + 1
}

func rightChildIndex(index int) int {
	return (index * 2) + 2
}

func parentIndex(index int) int {
	return (index - 1) / 2
}
