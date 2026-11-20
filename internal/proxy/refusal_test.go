package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// refusalBody has the shape of the refusal example on the provider's
// "Refusals and fallback" page: empty content, stop_reason "refusal" and a
// category in stop_details.
func refusalBody(category string, outputTokens string) string {
	return `{
  "id": "msg_01XFUDYJgAACzvnptvVoYEL",
  "type": "message",
  "role": "assistant",
  "model": "claude-fable-5",
  "content": [],
  "stop_reason": "refusal",
  "stop_details": {
    "type": "refusal",
    "category": ` + category + `,
    "explanation": "This request was declined."
  },
  "usage": {"input_tokens": 412, "output_tokens": ` + outputTokens + `}
}`
}

// refusalStream is the same call streamed. The stop reason and details
// arrive in the delta of message_delta, next to each other.
func refusalStream(category string, outputTokens string) string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_01XFUDYJgAACzvnptvVoYEL","type":"message","role":"assistant",` +
		`"model":"claude-fable-5","content":[],"stop_reason":null,"usage":{"input_tokens":412,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null,` +
		`"stop_details":{"type":"refusal","category":` + category + `,"explanation":"declined"}},` +
		`"usage":{"output_tokens":` + outputTokens + `}}` + "\n\n" +
		sseStop
}

func TestRefusalWithoutOutput(t *testing.T) {
	cat := func(s string) *string { return &s }
	tests := []struct {
		name    string
		reason  string
		details *stopDetails
		output  int64
		want    *refusal
	}{
		{name: "not a refusal", reason: "end_turn", want: nil},
		{name: "refusal after output", reason: "refusal", details: &stopDetails{Type: "refusal", Category: cat("cyber")}, output: 9, want: nil},
		{name: "refusal before output", reason: "refusal", details: &stopDetails{Type: "refusal", Category: cat("bio")}, want: &refusal{category: "bio"}},
		{name: "null category", reason: "refusal", details: &stopDetails{Type: "refusal"}, want: &refusal{}},
		{name: "no stop_details", reason: "refusal", want: &refusal{}},
		{name: "stop_details type only", details: &stopDetails{Type: "refusal", Category: cat("cyber")}, want: &refusal{category: "cyber"}},
	}
	for _, tt := range tests {
		got := refusalWithoutOutput(tt.reason, tt.details, tt.output)
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("%s: refusal = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestRefusalPolicy(t *testing.T) {
	def, err := newRefusalPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	for c, want := range map[string]refusalVerdict{
		"bio": refusalBilled, "frontier_llm": refusalBilled, "reasoning_extraction": refusalBilled,
		"cyber": refusalUnbilled, "general_harms": refusalUnbilled,
		"": refusalUnknown, "something_new": refusalUnknown, "BIO": refusalUnknown,
	} {
		if got := def.verdict(refusal{category: c}); got != want {
			t.Errorf("default list, %q: verdict %d, want %d", c, got, want)
		}
	}

	// A replaced list: a category the provider started billing is billed,
	// and one it stopped billing becomes a known unbilled one.
	custom, err := newRefusalPolicy([]string{"cyber", "something_new"})
	if err != nil {
		t.Fatal(err)
	}
	for c, want := range map[string]refusalVerdict{
		"cyber": refusalBilled, "something_new": refusalBilled, "bio": refusalUnbilled, "": refusalUnknown,
	} {
		if got := custom.verdict(refusal{category: c}); got != want {
			t.Errorf("custom list, %q: verdict %d, want %d", c, got, want)
		}
	}

	// An empty list bills no refusal without output.
	none, err := newRefusalPolicy([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if got := none.verdict(refusal{category: "bio"}); got != refusalUnbilled {
		t.Errorf("empty list, bio: verdict %d, want unbilled", got)
	}

	for _, bad := range []string{"Bio", "bio harm", "", strings.Repeat("x", 65), "bío"} {
		if _, err := newRefusalPolicy([]string{bad}); err == nil {
			t.Errorf("category %q accepted", bad)
		} else if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("error echoes the configured value: %v", err)
		}
	}
}

// End to end through the proxy, streamed and non-streamed: only a billed
// category is recorded, the others are counted, and a refusal after some
// output is recorded whatever its category.
func TestRefusalCallsRecordedByCategory(t *testing.T) {
	tests := []struct {
		name      string
		category  string // JSON value
		output    string
		wantStats Stats
		wantLog   string
	}{
		{name: "billed category", category: `"bio"`, output: "0", wantStats: Stats{Recorded: 1}},
		{name: "unbilled category", category: `"cyber"`, output: "0",
			wantStats: Stats{RefusalsUnbilled: 1}, wantLog: `"category":"cyber"`},
		{name: "unknown category", category: `"brand_new"`, output: "0",
			wantStats: Stats{RefusalsUnknownCategory: 1}, wantLog: `"category":"brand_new"`},
		{name: "null category", category: `null`, output: "0",
			wantStats: Stats{RefusalsUnknownCategory: 1}, wantLog: "missing or unrecognised"},
		{name: "unbilled category after output", category: `"cyber"`, output: "12", wantStats: Stats{Recorded: 1}},
	}
	for _, tt := range tests {
		for _, mode := range []string{"json", "stream"} {
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				var h http.HandlerFunc
				if mode == "json" {
					h = func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, refusalBody(tt.category, tt.output))
					}
				} else {
					h = streamed(nil, refusalStream(tt.category, tt.output))
				}
				f := newFixture(t, h)
				resp, _ := f.post(t, "/v1/messages", tags)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status %d", resp.StatusCode)
				}
				f.waitStats(t, tt.wantStats)

				events := f.rec.all()
				if tt.wantStats.Recorded == 0 {
					if len(events) != 0 {
						t.Fatalf("recorded %+v, want nothing", events)
					}
				} else if len(events) != 1 || events[0].EventID != "msg_01XFUDYJgAACzvnptvVoYEL" ||
					events[0].Model != "claude-fable-5" || events[0].InputTokens != 412 {
					t.Fatalf("events = %+v", events)
				}
				if tt.wantLog != "" && !strings.Contains(f.logs.String(), tt.wantLog) {
					t.Fatalf("log lacks %s: %s", tt.wantLog, f.logs.String())
				}
			})
		}
	}
}

// A replaced billed list applies to calls through the proxy.
func TestRefusalBilledListFromConfig(t *testing.T) {
	h := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, refusalBody(`"bio"`, "0"))
	}
	f := newFixture(t, h, func(c *Config) { c.BilledRefusalCategories = []string{} })
	if resp, _ := f.post(t, "/v1/messages", tags); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	f.waitStats(t, Stats{RefusalsUnbilled: 1})
	if n := len(f.rec.all()); n != 0 {
		t.Fatalf("recorded %d events, want 0", n)
	}
}

// When every attempt was declined, the attempts that produced output stay
// recorded even though the final refusal is not billed.
func TestUnbilledFinalRefusalKeepsDeclinedAttempts(t *testing.T) {
	body := `{"id":"msg_x","type":"message","model":"claude-opus-4-8","content":[],` +
		`"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber"},` +
		`"usage":{"input_tokens":412,"output_tokens":0,"iterations":[` +
		`{"type":"message","model":"claude-fable-5","input_tokens":535,"output_tokens":37},` +
		`{"type":"fallback_message","model":"claude-opus-4-8","input_tokens":412,"output_tokens":0}]}}`
	h := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
	f := newFixture(t, h)
	if resp, _ := f.post(t, "/v1/messages", tags); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	f.waitStats(t, Stats{Recorded: 1, RefusalsUnbilled: 1})
	events := f.rec.all()
	if len(events) != 1 || events[0].EventID != "msg_x/attempt-1" || events[0].Model != "claude-fable-5" {
		t.Fatalf("events = %+v", events)
	}
}
