package proxy

import (
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CheeseBurger7/llm-usage-meter/client"
)

// fallbackUsage has the shape of the usage block in the server-side
// fallback example on the provider's "Refusals and fallback" page, with a
// first attempt that declined after producing output.
const fallbackUsage = `"usage": {
    "input_tokens": 412,
    "output_tokens": 264,
    "cache_read_input_tokens": 0,
    "cache_creation_input_tokens": 0,
    "iterations": [
      {"type": "message", "model": "claude-fable-5", "input_tokens": 535, "output_tokens": 37,
       "cache_read_input_tokens": 1000, "cache_creation_input_tokens": 200},
      {"type": "fallback_message", "model": "claude-opus-4-8", "input_tokens": 412, "output_tokens": 264,
       "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
    ]
  }`

const fallbackBody = `{
  "id": "msg_01XFUDYJgAACzvnptvVoYEL",
  "type": "message",
  "role": "assistant",
  "model": "claude-opus-4-8",
  "content": [
    {"type": "fallback", "from": {"model": "claude-fable-5"}, "to": {"model": "claude-opus-4-8"},
     "trigger": {"type": "refusal", "category": "cyber"}},
    {"type": "text", "text": "` + secretCompletion + `"}
  ],
  "stop_reason": "end_turn",
  "stop_details": null,
  ` + fallbackUsage + `
}`

var (
	fallbackServing = client.Event{
		EventID: "msg_01XFUDYJgAACzvnptvVoYEL", Provider: "anthropic", Model: "claude-opus-4-8",
		InputTokens: 412, OutputTokens: 264,
	}
	fallbackDeclined = client.Event{
		EventID: "msg_01XFUDYJgAACzvnptvVoYEL/attempt-1", Provider: "anthropic", Model: "claude-fable-5",
		InputTokens: 535, OutputTokens: 37, CacheReadTokens: 1000, CacheWriteTokens: 200,
	}
)

func TestDeclinedAttempts(t *testing.T) {
	declined := func(model string, out int64) iteration {
		return iteration{Type: "message", Model: model, InputTokens: 100, OutputTokens: out}
	}
	served := iteration{Type: "fallback_message", Model: "c", InputTokens: 90, OutputTokens: 50}
	event := func(id, model string, out int64) client.Event {
		return client.Event{EventID: id, Provider: "anthropic", Model: model, InputTokens: 100, OutputTokens: out}
	}
	tests := []struct {
		name        string
		its         []iteration
		want        []client.Event
		wantLeftOut int
	}{
		{name: "no iterations"},
		{name: "served by the fallback model with no decline in this call", its: []iteration{served}},
		{name: "decline after output is recorded with its own model",
			its:  []iteration{declined("a", 9), served},
			want: []client.Event{event("msg_1/attempt-1", "a", 9)}},
		{name: "decline before any output is left out",
			its: []iteration{declined("a", 0), served}, wantLeftOut: 1},
		{name: "two declines keep their position in the id",
			its:         []iteration{declined("a", 0), declined("b", 4), served},
			want:        []client.Event{event("msg_1/attempt-2", "b", 4)},
			wantLeftOut: 1},
		{name: "two declines after output",
			its:  []iteration{declined("a", 3), declined("b", 4), served},
			want: []client.Event{event("msg_1/attempt-1", "a", 3), event("msg_1/attempt-2", "b", 4)}},
		{name: "attempt without a model is left out",
			its: []iteration{declined("", 9), served}, wantLeftOut: 1},
		{name: "message entries without a fallback entry are not declines",
			its: []iteration{{Type: "compaction", Model: "a", OutputTokens: 5}, declined("a", 9)}},
		{name: "entry types the proxy does not know are ignored",
			its: []iteration{{Type: "something_new", Model: "a", OutputTokens: 5}, served}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, leftOut := declinedAttempts("msg_1", tt.its)
			if !reflect.DeepEqual(got, tt.want) || leftOut != tt.wantLeftOut {
				t.Fatalf("events = %+v, left out = %d; want %+v, %d", got, leftOut, tt.want, tt.wantLeftOut)
			}
			// The same response gives the same ids, so a re-sent event dedupes.
			if again, _ := declinedAttempts("msg_1", tt.its); !reflect.DeepEqual(again, got) {
				t.Fatalf("second read = %+v, first = %+v", again, got)
			}
		})
	}
}

func TestJSONFallbackAttempts(t *testing.T) {
	m, ok := anthropicEvents([]byte(fallbackBody))
	if !ok || m.serving != fallbackServing || !reflect.DeepEqual(m.declined, []client.Event{fallbackDeclined}) || m.leftOut != 0 {
		t.Fatalf("metered = %+v, ok = %v", m, ok)
	}

	// The documented example: the first attempt declined before any output.
	noOutput := strings.Replace(fallbackBody, `"output_tokens": 37`, `"output_tokens": 0`, 1)
	m, ok = anthropicEvents([]byte(noOutput))
	if !ok || m.serving != fallbackServing || len(m.declined) != 0 || m.leftOut != 1 {
		t.Fatalf("decline before output: metered = %+v, ok = %v", m, ok)
	}
}

func TestStreamFallbackAttempts(t *testing.T) {
	// message_start named the model that declined in the middle of the stream.
	start := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_01XFUDYJgAACzvnptvVoYEL","type":"message","model":"claude-fable-5",` +
		`"usage":{"input_tokens":535,"output_tokens":1}}}` + "\n\n"
	delta := "event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},` +
		strings.Join(strings.Fields(fallbackUsage), "") + `}` + "\n\n"
	body := start + sseContent + delta + sseStop

	for _, piece := range []int{len(body), 1, 7} {
		m, reason := meterStreamAll(body, piece)
		if reason != "" || m.serving != fallbackServing || !reflect.DeepEqual(m.declined, []client.Event{fallbackDeclined}) {
			t.Fatalf("piece %d: metered = %+v, reason = %q", piece, m, reason)
		}
	}

	// A stream that did not finish records no attempt at all (Q19).
	if m, reason := meterStreamAll(start+sseContent+delta, 7); reason == "" || len(m.declined) != 0 {
		t.Fatalf("unfinished stream: metered = %+v, reason = %q", m, reason)
	}
}

// One call through the proxy that fell back gives two events: the same
// tags and timestamp on both, the call's latency on the serving one only.
func TestFallbackCallRecordsOneEventPerBillableAttempt(t *testing.T) {
	respond := map[string]http.HandlerFunc{
		"json": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fallbackBody)
		},
		"stream": streamed(nil,
			"event: message_start\n"+
				`data: {"type":"message_start","message":{"id":"msg_01XFUDYJgAACzvnptvVoYEL","model":"claude-fable-5","usage":{"input_tokens":535,"output_tokens":1}}}`+"\n\n",
			sseContent,
			"event: message_delta\n"+`data: {"type":"message_delta","delta":{},`+strings.Join(strings.Fields(fallbackUsage), "")+`}`+"\n\n",
			sseStop),
	}
	for name, h := range respond {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, h)
			f.p.now = steppingClock(250 * time.Millisecond)
			resp, _ := f.post(t, "/v1/messages", tags)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			// One call, however many events it produced.
			f.waitStats(t, Stats{Recorded: 1})

			events := f.rec.all()
			if len(events) != 2 {
				t.Fatalf("recorded %d events, want 2: %+v", len(events), events)
			}
			serving, declined := events[0], events[1]
			if serving.LatencyMs != 250 || declined.LatencyMs != 0 {
				t.Fatalf("latency: serving %d, declined %d; want 250 and 0", serving.LatencyMs, declined.LatencyMs)
			}
			if serving.Timestamp.IsZero() || !serving.Timestamp.Equal(declined.Timestamp) {
				t.Fatalf("timestamps differ: %v, %v", serving.Timestamp, declined.Timestamp)
			}
			for _, ev := range []*client.Event{&serving, &declined} {
				if ev.TenantID != "tenant-a" || ev.Feature != "chat" {
					t.Fatalf("tags missing on %+v", *ev)
				}
				ev.TenantID, ev.Feature, ev.Timestamp, ev.LatencyMs = "", "", time.Time{}, 0
			}
			if serving != fallbackServing || declined != fallbackDeclined {
				t.Fatalf("events = %+v, %+v", serving, declined)
			}
			if strings.Contains(f.logs.String(), secretCompletion) {
				t.Fatal("log contains the completion")
			}
		})
	}
}

// steppingClock returns a clock that starts at a fixed time on its first
// call and has moved on by step on every later one.
func steppingClock(step time.Duration) func() time.Time {
	t0 := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	var calls atomic.Int64
	return func() time.Time {
		if calls.Add(1) == 1 {
			return t0
		}
		return t0.Add(step)
	}
}
