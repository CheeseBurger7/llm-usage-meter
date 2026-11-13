package main

import (
	"strings"
	"testing"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
)

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// burstOf counts how many calls a fresh key gets before it is refused.
func burstOf(t *testing.T, kv map[string]string) int {
	t.Helper()
	l, err := ingestLimiter(env(kv))
	if err != nil {
		t.Fatal(err)
	}
	if l == nil {
		t.Fatal("limiter is off")
	}
	n := 0
	for n < 10_000 {
		if ok, _ := l.Allow(auth.KeyID{1}); !ok {
			break
		}
		n++
	}
	return n
}

func TestIngestLimiterConfig(t *testing.T) {
	// The rates are tiny so that no token refills while the test counts.
	t.Run("default is on", func(t *testing.T) {
		l, err := ingestLimiter(env(nil))
		if err != nil || l == nil {
			t.Fatalf("limiter = %v, err = %v; want a limiter", l, err)
		}
	})
	t.Run("burst defaults to one second of the rate, at least 1", func(t *testing.T) {
		if got := burstOf(t, map[string]string{envRateLimitRPS: "0.001"}); got != 1 {
			t.Fatalf("burst = %d, want 1", got)
		}
	})
	t.Run("burst is configurable", func(t *testing.T) {
		kv := map[string]string{envRateLimitRPS: "0.001", envRateLimitBurst: "7"}
		if got := burstOf(t, kv); got != 7 {
			t.Fatalf("burst = %d, want 7", got)
		}
	})
	t.Run("rate 0 turns the limit off", func(t *testing.T) {
		l, err := ingestLimiter(env(map[string]string{envRateLimitRPS: "0"}))
		if err != nil || l != nil {
			t.Fatalf("limiter = %v, err = %v; want nil, nil", l, err)
		}
	})
}

func TestIngestLimiterRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name string
		kv   map[string]string
		want string // the variable the error must name
	}{
		{"rate is not a number", map[string]string{envRateLimitRPS: "fast"}, envRateLimitRPS},
		{"rate is negative", map[string]string{envRateLimitRPS: "-1"}, envRateLimitRPS},
		{"rate is NaN", map[string]string{envRateLimitRPS: "NaN"}, envRateLimitRPS},
		{"rate is infinite", map[string]string{envRateLimitRPS: "Inf"}, envRateLimitRPS},
		{"burst is not a whole number", map[string]string{envRateLimitBurst: "1.5"}, envRateLimitBurst},
		{"burst is zero", map[string]string{envRateLimitBurst: "0"}, envRateLimitBurst},
		// A bad burst is reported even when the limit is off, so the typo is
		// found now and not when the limit is turned back on.
		{"bad burst with the limit off", map[string]string{envRateLimitRPS: "0", envRateLimitBurst: "x"}, envRateLimitBurst},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, err := ingestLimiter(env(tt.kv))
			if err == nil || l != nil {
				t.Fatalf("limiter = %v, err = %v; want an error", l, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not name %s", err, tt.want)
			}
		})
	}
}
