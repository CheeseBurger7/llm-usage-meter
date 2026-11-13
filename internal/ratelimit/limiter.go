// Package ratelimit is an in-memory token bucket limiter, one bucket per key.
package ratelimit

import (
	"errors"
	"math"
	"sync"
	"time"
)

// minSweepInterval bounds how often idle buckets are dropped.
const minSweepInterval = time.Minute

// Limiter holds one token bucket per key. A bucket starts full with burst
// tokens, each allowed call takes one, and tokens refill at rate per second
// up to burst. It is safe for concurrent use.
//
// State lives in this process only: it is lost on restart, and each replica
// of the service enforces the limit on its own.
type Limiter[K comparable] struct {
	rate  float64 // tokens per second
	burst float64
	now   func() time.Time

	mu        sync.Mutex
	buckets   map[K]bucket
	lastSweep time.Time
}

type bucket struct {
	tokens  float64
	updated time.Time
}

// New returns a limiter that allows rate calls per second per key on average
// and up to burst calls at once.
func New[K comparable](rate float64, burst int) (*Limiter[K], error) {
	if !(rate > 0) || math.IsInf(rate, 0) {
		return nil, errors.New("ratelimit: rate must be a positive number")
	}
	if burst < 1 {
		return nil, errors.New("ratelimit: burst must be at least 1")
	}
	return &Limiter[K]{
		rate:    rate,
		burst:   float64(burst),
		now:     time.Now,
		buckets: make(map[K]bucket),
	}, nil
}

// Allow takes one token from the key's bucket. When the bucket is empty it
// returns false and how long until the next token is available.
func (l *Limiter[K]) Allow(key K) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweep(now)

	b, found := l.buckets[key]
	if !found {
		b = bucket{tokens: l.burst, updated: now}
	}
	// A clock that steps backwards refills nothing rather than draining.
	if elapsed := now.Sub(b.updated).Seconds(); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
		b.updated = now
	}
	if b.tokens < 1 {
		l.buckets[key] = b
		return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	}
	b.tokens--
	l.buckets[key] = b
	return true, 0
}

// sweep drops buckets idle for long enough to have refilled completely. A
// missing bucket is treated as full, so dropping one changes no decision; it
// only keeps the map from growing with keys that stopped sending.
func (l *Limiter[K]) sweep(now time.Time) {
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	if now.Sub(l.lastSweep) < max(full, minSweepInterval) {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.updated) >= full {
			delete(l.buckets, k)
		}
	}
}
