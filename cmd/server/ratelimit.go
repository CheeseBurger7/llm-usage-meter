package main

import (
	"fmt"
	"math"
	"strconv"

	"github.com/CheeseBurger7/llm-usage-meter/internal/api"
	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ratelimit"
)

const (
	envRateLimitRPS   = "INGEST_RATE_LIMIT_RPS"
	envRateLimitBurst = "INGEST_RATE_LIMIT_BURST"

	// defaultRateLimitRPS is per API key. At 500 events per batch it is far
	// above what one key needs, and low enough to stop a client stuck in a
	// retry loop from filling the connection pool.
	defaultRateLimitRPS = 100
)

// ingestLimiter builds the per-key limiter for the ingest routes from the
// environment. It returns nil, meaning no limit, only when the rate is
// explicitly 0. A value that does not parse is an error, not a silent
// fallback: a typo must not start the service with a limit nobody chose.
func ingestLimiter(getenv func(string) string) (*api.KeyLimiter, error) {
	rps := float64(defaultRateLimitRPS)
	if v := getenv(envRateLimitRPS); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%s=%q: want a number of requests per second, or 0 for no limit", envRateLimitRPS, v)
		}
		rps = f
	}

	// By default a key may spend one second's worth of requests at once.
	burst := max(int(math.Ceil(rps)), 1)
	if v := getenv(envRateLimitBurst); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%s=%q: want a whole number of requests, at least 1", envRateLimitBurst, v)
		}
		burst = n
	}

	if rps == 0 {
		return nil, nil
	}
	l, err := ratelimit.New[auth.KeyID](rps, burst)
	if err != nil {
		return nil, fmt.Errorf("%s, %s: %w", envRateLimitRPS, envRateLimitBurst, err)
	}
	return l, nil
}
