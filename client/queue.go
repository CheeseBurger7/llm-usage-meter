package client

// queue is a fixed-capacity FIFO of events. When it is full, push evicts the
// oldest event to make room. It is not safe for concurrent use; Client guards
// it with a mutex.
type queue struct {
	buf  []Event
	head int // index of the oldest event
	n    int // number of events held
}

func newQueue(capacity int) *queue {
	return &queue{buf: make([]Event, capacity)}
}

func (q *queue) len() int { return q.n }

// push appends ev and reports whether the oldest event was evicted for it.
func (q *queue) push(ev Event) (evicted bool) {
	if q.n == len(q.buf) {
		q.buf[q.head] = ev
		q.head = (q.head + 1) % len(q.buf)
		return true
	}
	q.buf[(q.head+q.n)%len(q.buf)] = ev
	q.n++
	return false
}

// take removes and returns up to limit events, oldest first.
func (q *queue) take(limit int) []Event {
	k := min(limit, q.n)
	if k == 0 {
		return nil
	}
	out := make([]Event, k)
	for i := range out {
		idx := (q.head + i) % len(q.buf)
		out[i] = q.buf[idx]
		q.buf[idx] = Event{} // release the strings
	}
	q.head = (q.head + k) % len(q.buf)
	q.n -= k
	return out
}
