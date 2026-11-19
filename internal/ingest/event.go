// Package ingest accepts usage events and stores them idempotently.
package ingest

import (
	"errors"
	"fmt"
	"time"

	"github.com/CheeseBurger7/llm-usage-meter/internal/provider"
)

// Event is a single LLM call as reported by a client.
type Event struct {
	EventID  string
	TenantID string
	// Provider is the LLM provider that served the call, for example
	// "anthropic". There is no fixed list, but the label must match
	// ^[a-z0-9._-]{1,256}$ (Q20). It is case-insensitive (Q13): ingest folds
	// the ASCII letters A to Z to lowercase before validation, dedupe and
	// storage, so "Anthropic" and "anthropic" are one provider. Nothing else
	// is normalized: " anthropic" and "ánthropic" are refused.
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
	// Partial marks a call that ended before its final usage report, such
	// as a stream the caller hung up on (Q19). Its counts can only be too
	// low. It is part of the payload: a re-send that differs only in
	// Partial is a conflict, so an event cannot be marked afterwards.
	Partial bool
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
	if reason := provider.Check(e.Provider); reason != "" {
		fields["provider"] = reason
	}
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

// normalized returns the event as Postgres will store it: provider with
// A to Z lowercased, timestamp in UTC at microsecond precision. Ingest
// applies it before Validate, so Validate sees the stored form. TenantID,
// Model and Feature stay byte for byte.
func (e Event) normalized() Event {
	e.Provider = provider.Normalize(e.Provider)
	e.Timestamp = e.Timestamp.UTC().Truncate(time.Microsecond)
	return e
}

// sameAs compares every field; timestamps compare by instant, not by location.
// Both events must be normalized, which makes the provider comparison
// case-insensitive.
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
		e.Timestamp.Equal(o.Timestamp) &&
		e.Partial == o.Partial
}

// ErrConflict means the event_id already exists with a different payload.
var ErrConflict = errors.New("event_id already exists with a different payload")
