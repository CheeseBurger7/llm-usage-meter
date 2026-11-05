package ingest

import (
	"errors"
	"testing"
	"time"
)

func validEvent() Event {
	return Event{
		EventID:      "evt-1",
		TenantID:     "tenant-a",
		Model:        "claude-sonnet",
		InputTokens:  100,
		OutputTokens: 50,
		LatencyMs:    420,
		Timestamp:    time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Event)
		field  string
	}{
		{"valid", func(*Event) {}, ""},
		{"zero tokens ok", func(e *Event) { e.InputTokens, e.OutputTokens, e.LatencyMs = 0, 0, 0 }, ""},
		{"missing event_id", func(e *Event) { e.EventID = "" }, "event_id"},
		{"missing tenant_id", func(e *Event) { e.TenantID = "" }, "tenant_id"},
		{"missing model", func(e *Event) { e.Model = "" }, "model"},
		{"overlong event_id", func(e *Event) { e.EventID = string(make([]byte, maxIDLen+1)) }, "event_id"},
		{"negative input", func(e *Event) { e.InputTokens = -1 }, "input_tokens"},
		{"negative output", func(e *Event) { e.OutputTokens = -1 }, "output_tokens"},
		{"negative latency", func(e *Event) { e.LatencyMs = -1 }, "latency_ms"},
		{"missing timestamp", func(e *Event) { e.Timestamp = time.Time{} }, "timestamp"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := validEvent()
			tc.mutate(&ev)
			err := ev.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("want *ValidationError, got %v", err)
			}
			if _, ok := verr.Fields[tc.field]; !ok {
				t.Fatalf("want error on %q, got %v", tc.field, verr.Fields)
			}
		})
	}
}

func TestValidateReportsAllFields(t *testing.T) {
	err := Event{}.Validate()
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("want *ValidationError, got %v", err)
	}
	if len(verr.Fields) != 4 {
		t.Fatalf("want 4 failing fields, got %v", verr.Fields)
	}
}

func TestSameAsIgnoresTimezone(t *testing.T) {
	a := validEvent()
	b := validEvent()
	b.Timestamp = a.Timestamp.In(time.FixedZone("x", 5*3600))
	if !a.sameAs(b) {
		t.Fatal("same instant in a different zone should compare equal")
	}
}
