package ingest

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validEvent() Event {
	return Event{
		EventID:      "evt-1",
		TenantID:     "tenant-a",
		Provider:     "anthropic",
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
		{"missing provider", func(e *Event) { e.Provider = "" }, "provider"},
		{"overlong provider", func(e *Event) { e.Provider = strings.Repeat("p", maxIDLen+1) }, "provider"},
		{"provider at limit", func(e *Event) { e.Provider = strings.Repeat("p", maxIDLen) }, ""},
		{"provider with space", func(e *Event) { e.Provider = "anthropic " }, "provider"},
		{"non-ASCII provider", func(e *Event) { e.Provider = "ánthropic" }, "provider"},
		{"feature and cache tokens set", func(e *Event) {
			e.Feature, e.CacheReadTokens, e.CacheWriteTokens = "chat", 30, 20
		}, ""},
		{"feature at limit", func(e *Event) { e.Feature = strings.Repeat("f", maxFeatureLen) }, ""},
		{"overlong feature", func(e *Event) { e.Feature = strings.Repeat("f", maxFeatureLen+1) }, "feature"},
		// The limit is in bytes: 86 three-byte runes are 258 bytes.
		{"overlong multibyte feature", func(e *Event) { e.Feature = strings.Repeat("\u20ac", 86) }, "feature"},
		{"negative cache read", func(e *Event) { e.CacheReadTokens = -1 }, "cache_read_tokens"},
		{"negative cache write", func(e *Event) { e.CacheWriteTokens = -1 }, "cache_write_tokens"},
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
	if len(verr.Fields) != 5 {
		t.Fatalf("want 5 failing fields, got %v", verr.Fields)
	}
}

// Every field takes part in the comparison behind the 409 rule, so a re-send
// that changes any one of them is not a duplicate.
func TestSameAsComparesEveryField(t *testing.T) {
	mutations := map[string]func(*Event){
		"event_id":           func(e *Event) { e.EventID = "evt-2" },
		"tenant_id":          func(e *Event) { e.TenantID = "tenant-b" },
		"provider":           func(e *Event) { e.Provider = "openai" },
		"model":              func(e *Event) { e.Model = "other" },
		"feature":            func(e *Event) { e.Feature = "search" },
		"input_tokens":       func(e *Event) { e.InputTokens++ },
		"output_tokens":      func(e *Event) { e.OutputTokens++ },
		"cache_read_tokens":  func(e *Event) { e.CacheReadTokens++ },
		"cache_write_tokens": func(e *Event) { e.CacheWriteTokens++ },
		"latency_ms":         func(e *Event) { e.LatencyMs++ },
		"timestamp":          func(e *Event) { e.Timestamp = e.Timestamp.Add(time.Second) },
		"partial":            func(e *Event) { e.Partial = true },
	}
	// A field added to Event without a mutation here fails the test.
	if n := reflect.TypeOf(Event{}).NumField(); n != len(mutations) {
		t.Fatalf("Event has %d fields but %d mutations: cover the new field here and in sameAs", n, len(mutations))
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			a, b := validEvent(), validEvent()
			mutate(&b)
			if a.sameAs(b) {
				t.Fatalf("events differing in %s compare equal", name)
			}
		})
	}
	if a := validEvent(); !a.sameAs(validEvent()) {
		t.Fatal("identical events compare different")
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

// Only provider is case-insensitive, and only its ASCII letters are folded.
func TestNormalizedLowercasesProviderOnly(t *testing.T) {
	ev := validEvent()
	ev.TenantID, ev.Provider, ev.Model, ev.Feature = "Tenant-A", " Anthropic\tÉ ", "Claude-Sonnet", "Chat"
	got := ev.normalized()
	if got.Provider != " anthropic\tÉ " {
		t.Fatalf("provider = %q; want A-Z lowercased and everything else kept", got.Provider)
	}
	if got.TenantID != ev.TenantID || got.Model != ev.Model || got.Feature != ev.Feature {
		t.Fatalf("normalized changed another label: %+v", got)
	}
	if ev.Provider != " Anthropic\tÉ " {
		t.Fatal("normalized changed its receiver")
	}
}

// A re-send that differs only in the case of provider is the same event once
// both are normalized. Without normalizing, sameAs still tells them apart.
func TestSameAsProviderCaseAfterNormalizing(t *testing.T) {
	a, b := validEvent(), validEvent()
	b.Provider = "Anthropic"
	if !a.normalized().sameAs(b.normalized()) {
		t.Fatal("providers differing only in case should compare equal after normalizing")
	}
	b.Provider = "anthropic "
	if a.normalized().sameAs(b.normalized()) {
		t.Fatal("a trailing space should still be a different provider")
	}
}

// Ingest folds A-Z before it validates (Q13, Q20), so uppercase ASCII is
// accepted, while non-ASCII letters, whitespace and other punctuation are
// refused rather than rewritten. Validation fails before the database is
// used, so a Store without one is enough for the refusals.
func TestIngestProviderCharset(t *testing.T) {
	for _, p := range []string{"Anthropic", "OPENAI", "Azure-OpenAI", "bedrock.us_east-1"} {
		ev := validEvent()
		ev.Provider = p
		if err := ev.normalized().Validate(); err != nil {
			t.Errorf("%q after normalizing: %v", p, err)
		}
	}
	refused := map[string]string{
		"non-ASCII letter":     "ánthropic",
		"non-ASCII uppercase":  "ÁNTHROPIC",
		"two-byte to 3-byte":   strings.Repeat("Ⱥ", maxIDLen/2),
		"leading space":        " anthropic",
		"trailing space":       "anthropic ",
		"tab":                  "anthropic\t",
		"slash":                "open/ai",
		"overlong":             strings.Repeat("p", maxIDLen+1),
		"overlong after upper": strings.Repeat("P", maxIDLen+1),
	}
	for name, p := range refused {
		ev := validEvent()
		ev.Provider = p
		_, err := NewStore(nil).Ingest(t.Context(), [16]byte{1}, ev)
		var verr *ValidationError
		if !errors.As(err, &verr) || verr.Fields["provider"] == "" {
			t.Errorf("%s: err = %v; want a validation error on provider", name, err)
		}
	}
}
