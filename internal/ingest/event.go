// Package ingest accepts usage events and stores them idempotently.
package ingest

import (
	"errors"
	"fmt"
	"time"
)

// Event is a single LLM call as reported by a client.
type Event struct {
	EventID  string
	TenantID string
	// Provider is the LLM provider that served the call, for example
	// "anthropic". It is a free-text label and is compared byte for byte.
	Provider string
	Model    string
	// Feature is an optional label for the product feature that made the
	// call. Empty means not set.
	Feature      string
	InputTokens  int64
	OutputTokens int64
	// CacheReadTokens and CacheWriteTokens are counted separately from
	// InputTokens, as providers report and price them separately.
	CacheReadTokens  int64
	CacheWriteTokens int64
	LatencyMs        int64
	Timestamp        time.Time
}

// ValidationError lists every field that failed validation.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid event: %d field(s) failed validation", len(e.Fields))
}

const (
	maxIDLen      = 256
	maxFeatureLen = 256
)

// Validate checks the event and returns a *ValidationError describing all problems.
func (e Event) Validate() error {
	fields := map[string]string{}
	requireString := func(name, v string) {
		switch {
		case v == "":
			fields[name] = "required"
		case len(v) > maxIDLen:
			fields[name] = fmt.Sprintf("must be at most %d bytes", maxIDLen)
		}
	}
	requireString("event_id", e.EventID)
	requireString("tenant_id", e.TenantID)
	requireString("provider", e.Provider)
	requireString("model", e.Model)
	if len(e.Feature) > maxFeatureLen {
		fields["feature"] = fmt.Sprintf("must be at most %d bytes", maxFeatureLen)
	}

	if e.InputTokens < 0 {
		fields["input_tokens"] = "must be >= 0"
	}
	if e.OutputTokens < 0 {
		fields["output_tokens"] = "must be >= 0"
	}
	if e.CacheReadTokens < 0 {
		fields["cache_read_tokens"] = "must be >= 0"
	}
	if e.CacheWriteTokens < 0 {
		fields["cache_write_tokens"] = "must be >= 0"
	}
	if e.LatencyMs < 0 {
		fields["latency_ms"] = "must be >= 0"
	}
	if e.Timestamp.IsZero() {
		fields["timestamp"] = "required"
	}

	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

// normalized returns the event as Postgres will store it: UTC, microsecond precision.
func (e Event) normalized() Event {
	e.Timestamp = e.Timestamp.UTC().Truncate(time.Microsecond)
	return e
}

// sameAs compares every field; timestamps compare by instant, not by location.
func (e Event) sameAs(o Event) bool {
	return e.EventID == o.EventID &&
		e.TenantID == o.TenantID &&
		e.Provider == o.Provider &&
		e.Model == o.Model &&
		e.Feature == o.Feature &&
		e.InputTokens == o.InputTokens &&
		e.OutputTokens == o.OutputTokens &&
		e.CacheReadTokens == o.CacheReadTokens &&
		e.CacheWriteTokens == o.CacheWriteTokens &&
		e.LatencyMs == o.LatencyMs &&
		e.Timestamp.Equal(o.Timestamp)
}

// ErrConflict means the event_id already exists with a different payload.
var ErrConflict = errors.New("event_id already exists with a different payload")
