package ratelimit

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a clock the test moves by hand.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(t *testing.T, rate float64, burst int) (*Limiter[string], *fakeClock) {
	t.Helper()
	l, err := New[string](rate, burst)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	l.now = clock.now
	return l, clock
}

func TestNewRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name  string
		rate  float64
		burst int
	}{
		{"zero rate", 0, 1},
		{"negative rate", -1, 1},
		{"NaN rate", math.NaN(), 1},
		{"infinite rate", math.Inf(1), 1},
		{"zero burst", 1, 0},
		{"negative burst", 1, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New[string](tt.rate, tt.burst); err == nil {
				t.Fatal("New accepted the config")
			}
		})
	}
}

func TestBurstThenRefill(t *testing.T) {
	l, clock := newTestLimiter(t, 2, 3) // 2 per second, 3 at once

	for i := range 3 {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("call %d of the burst was refused", i+1)
		}
	}
	ok, retry := l.Allow("k")
	if ok || retry != 500*time.Millisecond {
		t.Fatalf("empty bucket: ok = %v, retry = %v; want false, 500ms", ok, retry)
	}

	// Part of the wait has passed: still refused, and the wait shrinks.
	clock.advance(200 * time.Millisecond)
	ok, retry = l.Allow("k")
	if ok || retry != 300*time.Millisecond {
		t.Fatalf("after 200ms: ok = %v, retry = %v; want false, 300ms", ok, retry)
	}

	// Waiting exactly as long as told is enough for one call, not two.
	clock.advance(retry)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("refused after waiting the advertised time")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("a second call was allowed without a refill")
	}
}

func TestRefillIsCappedAtBurst(t *testing.T) {
	l, clock := newTestLimiter(t, 10, 2)
	l.Allow("k")
	l.Allow("k")
	clock.advance(time.Hour)

	allowed := 0
	for range 5 {
		if ok, _ := l.Allow("k"); ok {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("allowed %d calls after a long idle, want the burst of 2", allowed)
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l, _ := newTestLimiter(t, 1, 1)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a refused")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("a allowed twice")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("b was refused because a used its bucket")
	}
}

func TestClockSteppingBackDoesNotDrainOrRefill(t *testing.T) {
	l, clock := newTestLimiter(t, 1, 1)
	l.Allow("k")
	clock.advance(-time.Hour)
	ok, retry := l.Allow("k")
	if ok || retry != time.Second {
		t.Fatalf("ok = %v, retry = %v; want false, 1s", ok, retry)
	}
}

func TestSweepDropsOnlyRefilledBuckets(t *testing.T) {
	l, clock := newTestLimiter(t, 1, 120) // a bucket refills fully in 2 minutes
	l.Allow("idle")
	clock.advance(90 * time.Second)
	l.Allow("busy")
	clock.advance(40 * time.Second) // idle: 130s since use, busy: 40s

	l.Allow("other") // triggers the sweep
	if _, ok := l.buckets["idle"]; ok {
		t.Fatal("a fully refilled bucket was kept")
	}
	if _, ok := l.buckets["busy"]; !ok {
		t.Fatal("a bucket that is still refilling was dropped")
	}
}

func TestConcurrentCallsNeverExceedBurst(t *testing.T) {
	l, _ := newTestLimiter(t, 1, 50) // the fake clock never moves, so no refill
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 20 {
				if ok, _ := l.Allow("k"); ok {
					allowed.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if got := allowed.Load(); got != 50 {
		t.Fatalf("allowed %d of 400 concurrent calls, want exactly 50", got)
	}
}
