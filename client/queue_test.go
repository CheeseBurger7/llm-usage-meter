package client

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
)

func TestQueueIsFIFO(t *testing.T) {
	q := newQueue(4)
	for i := range 3 {
		if q.push(event(fmt.Sprintf("e%d", i))) {
			t.Fatalf("push %d evicted with room left", i)
		}
	}
	if got := ids(q.take(2)); got != "e0,e1" {
		t.Fatalf("take(2) = %q, want e0,e1", got)
	}
	if got := ids(q.take(10)); got != "e2" {
		t.Fatalf("take(10) = %q, want e2", got)
	}
	if got := q.take(1); got != nil {
		t.Fatalf("take on an empty queue = %v, want nil", got)
	}
}

func TestQueueDropsOldestWhenFull(t *testing.T) {
	q := newQueue(3)
	evicted := 0
	for i := range 8 {
		if q.push(event(fmt.Sprintf("e%d", i))) {
			evicted++
		}
	}
	if evicted != 5 || q.len() != 3 {
		t.Fatalf("evicted = %d, len = %d, want 5 and 3", evicted, q.len())
	}
	if got := ids(q.take(3)); got != "e5,e6,e7" {
		t.Fatalf("kept %q, want the newest three e5,e6,e7", got)
	}
}

// Pushing and taking in turns walks head around the ring several times.
func TestQueueWrapsAround(t *testing.T) {
	q := newQueue(3)
	next := 0
	for round := range 10 {
		q.push(event(fmt.Sprintf("e%d", next)))
		q.push(event(fmt.Sprintf("e%d", next+1)))
		want := fmt.Sprintf("e%d,e%d", next, next+1)
		if got := ids(q.take(2)); got != want {
			t.Fatalf("round %d: took %q, want %q", round, got, want)
		}
		next += 2
	}
	if q.len() != 0 {
		t.Fatalf("len = %d, want 0", q.len())
	}
}

// lockedWriter lets the worker goroutine log while the test reads.
type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func newTestLogger(w io.Writer, mu *sync.Mutex) *slog.Logger {
	return slog.New(slog.NewTextHandler(lockedWriter{w, mu}, nil))
}
