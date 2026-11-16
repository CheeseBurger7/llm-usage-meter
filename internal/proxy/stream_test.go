package proxy

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CheeseBurger7/llm-usage-meter/client"
)

// Event lines follow the examples on the provider's "Streaming messages"
// page, including a ping, a content delta and counts that grow.
const (
	sseStart = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_stream_1","type":"message","role":"assistant",` +
		`"model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,` +
		`"usage":{"input_tokens":2679,"cache_creation_input_tokens":40,"cache_read_input_tokens":50,"output_tokens":3}}}` + "\n\n"
	sseContent = "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: ping\n" + `data: {"type": "ping"}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + secretCompletion + `"}}` + "\n\n" +
		"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n"
	sseDelta = "event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},` +
		`"usage":{"input_tokens":10682,"cache_creation_input_tokens":40,"cache_read_input_tokens":50,"output_tokens":510,` +
		`"server_tool_use":{"web_search_requests":1}}}` + "\n\n"
	sseStop = "event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

	sseBody = sseStart + sseContent + sseDelta + sseStop
)

var sseWant = client.Event{
	EventID: "msg_stream_1", Provider: "anthropic", Model: "claude-opus-5-5",
	InputTokens: 10682, OutputTokens: 510, CacheReadTokens: 50, CacheWriteTokens: 40,
}

// meterStream feeds body to an sseMeter in pieces of the given size and
// returns the event of the serving attempt.
func meterStream(body string, piece int) (client.Event, string) {
	m, reason := meterStreamAll(body, piece)
	return m.serving, reason
}

// meterStreamAll is meterStream with the declined attempts as well.
func meterStreamAll(body string, piece int) (metered, string) {
	var s sseMeter
	for len(body) > 0 {
		n := min(piece, len(body))
		s.write([]byte(body[:n]))
		body = body[n:]
	}
	return s.result()
}

func TestStreamUsage(t *testing.T) {
	// A content line far longer than anything the meter keeps.
	longDelta := "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"text":"` + strings.Repeat("x", 3*maxUsageEventBytes) + `"}}` + "\n\n"
	fallbackDelta := "event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":412,"output_tokens":264,` +
		`"iterations":[{"type":"message","model":"claude-opus-5-5","input_tokens":535,"output_tokens":9},` +
		`{"type":"fallback_message","model":"claude-opus-4-8","input_tokens":412,"output_tokens":264}]}}` + "\n\n"

	outputOnly := sseWant
	outputOnly.InputTokens, outputOnly.OutputTokens = 25, 15
	outputOnly.CacheReadTokens, outputOnly.CacheWriteTokens = 0, 0
	fallback := sseWant
	fallback.Model, fallback.InputTokens, fallback.OutputTokens = "claude-opus-4-8", 412, 264

	tests := []struct {
		name string
		body string
		want client.Event
	}{
		{name: "counts in message_delta replace those in message_start", body: sseBody, want: sseWant},
		{name: "CRLF line endings", body: strings.ReplaceAll(sseBody, "\n", "\r\n"), want: sseWant},
		{name: "no space after the colon", body: strings.ReplaceAll(sseBody, ": ", ":"), want: sseWant},
		{name: "comments and unknown events are ignored",
			body: ": keep-alive\n\n" + sseStart + "event: something_new\ndata: {\"usage\":{\"output_tokens\":999}}\n\n" + sseDelta + sseStop,
			want: sseWant},
		{name: "long content lines are not kept", body: sseStart + longDelta + sseDelta + sseStop, want: sseWant},
		{name: "message_delta with output tokens only keeps the input count",
			body: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_stream_1","model":"claude-opus-5-5","usage":{"input_tokens":25,"output_tokens":1}}}` + "\n\n" +
				"event: message_delta\n" + `data: {"type":"message_delta","delta":{},"usage":{"output_tokens":15}}` + "\n\n" + sseStop,
			want: outputOnly},
		{name: "null counts keep the value seen so far",
			body: sseStart + sseDelta + "event: message_delta\n" +
				`data: {"type":"message_delta","usage":{"input_tokens":null,"output_tokens":null,"cache_read_input_tokens":null}}` + "\n\n" + sseStop,
			want: sseWant},
		{name: "fallback in the middle of the stream reports the serving model",
			body: sseStart + sseContent + fallbackDelta + sseStop, want: fallback},
		{name: "bytes after message_stop are ignored", body: sseBody + "event: error\ndata: {}\n\n", want: sseWant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// One piece, and pieces that split every line and field.
			for _, piece := range []int{len(tt.body), 1, 7} {
				got, reason := meterStream(tt.body, piece)
				if reason != "" || got != tt.want {
					t.Fatalf("piece %d: event = %+v, reason = %q, want %+v", piece, got, reason, tt.want)
				}
			}
		})
	}
}

func TestStreamNotMetered(t *testing.T) {
	bigStart := "event: message_start\n" +
		`data: {"type":"message_start","pad":"` + strings.Repeat("x", maxUsageEventBytes) + `","message":{"id":"m"}}` + "\n\n"
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "cut off before message_stop", body: sseStart + sseContent + sseDelta, want: "before message_stop"},
		{name: "last line never ended", body: sseStart + sseDelta + "event: message_stop\ndata: {}", want: "before message_stop"},
		{name: "error event", body: sseStart + "event: error\n" + `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n",
			want: "error in the stream"},
		{name: "malformed usage event", body: sseStart + "event: message_delta\ndata: {\"usage\":\n\n" + sseStop, want: "could not be read"},
		{name: "usage event over the limit", body: bigStart + sseDelta + sseStop, want: "could not be read"},
		{name: "no message_start", body: sseDelta + sseStop, want: "no message id"},
		{name: "no usage", body: "event: message_start\n" + `data: {"type":"message_start","message":{"id":"m","model":"x"}}` + "\n\n" + sseStop,
			want: "no message id"},
		{name: "no event names", body: strings.ReplaceAll(sseBody, "event: ", "x-event: "), want: "before message_stop"},
		{name: "empty", body: "", want: "before message_stop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, piece := range []int{max(len(tt.body), 1), 1} {
				if _, reason := meterStream(tt.body, piece); !strings.Contains(reason, tt.want) {
					t.Fatalf("piece %d: reason = %q, want one containing %q", piece, reason, tt.want)
				}
			}
		})
	}
}

// The meter must not hold on to the response: whatever the length of the
// stream, it keeps at most one usage event and a short line prefix.
func TestStreamMeterMemoryIsBounded(t *testing.T) {
	var s sseMeter
	s.write([]byte(sseStart))
	delta := []byte("event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"text":"` + strings.Repeat("y", 4096) + `"}}` + "\n\n")
	for range 2000 { // about 8 MiB of content
		s.write(delta)
	}
	if c := cap(s.line) + cap(s.data); c > 16<<10 {
		t.Fatalf("meter holds %d bytes after 8 MiB of content", c)
	}
	s.write([]byte(sseDelta + sseStop))
	if got, reason := s.result(); reason != "" || got.serving != sseWant {
		t.Fatalf("event = %+v, reason = %q", got, reason)
	}
}

// streamed answers with an event stream, flushing after each part and
// waiting for a value on gate (if not nil) before each part after the first.
func streamed(gate <-chan struct{}, parts ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i, part := range parts {
			if i > 0 && gate != nil {
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
			_, _ = io.WriteString(w, part)
			w.(http.Flusher).Flush()
		}
	}
}

func TestStreamIsForwardedUnchangedAndMetered(t *testing.T) {
	f := newFixture(t, streamed(nil, sseStart, sseContent, sseDelta, sseStop))
	resp, body := f.post(t, "/v1/messages", tags)
	if resp.StatusCode != http.StatusOK || body != sseBody {
		t.Fatalf("status %d, stream changed: %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	f.waitStats(t, Stats{Recorded: 1})
	got := f.rec.all()[0]
	if got.Timestamp.IsZero() || got.LatencyMs < 0 {
		t.Fatalf("timestamp %v or latency %d out of range", got.Timestamp, got.LatencyMs)
	}
	got.Timestamp, got.LatencyMs = time.Time{}, 0
	want := sseWant
	want.TenantID, want.Feature = "tenant-a", "chat"
	if got != want {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
	if logs := f.logs.String(); strings.Contains(logs, secretCompletion) || strings.Contains(logs, "tenant-a") {
		t.Fatalf("log contains part of the call:\n%s", logs)
	}
}

// The provider holds the rest of the stream back until the caller has read
// the first event through the proxy. If the proxy waited for more of the
// stream before forwarding, this would never finish.
func TestStreamFirstByteIsNotDelayed(t *testing.T) {
	gate := make(chan struct{})
	f := newFixture(t, streamed(gate, sseStart, sseContent, sseDelta+sseStop))

	req, err := http.NewRequest(http.MethodPost, f.proxy.URL+"/v1/messages", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(TenantHeader, "tenant-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Fail instead of hanging if the proxy holds bytes back.
	watchdog := time.AfterFunc(10*time.Second, func() { _ = resp.Body.Close() })
	defer watchdog.Stop()

	r := bufio.NewReader(resp.Body)
	readEvents := func(want string) {
		t.Helper()
		got := make([]byte, len(want))
		if _, err := io.ReadFull(r, got); err != nil || string(got) != want {
			t.Fatalf("read %q, err %v; want %q while the provider is still holding the rest", got, err, want)
		}
	}
	readEvents(sseStart)
	if got := f.p.Stats(); got != (Stats{}) {
		t.Fatalf("stats = %+v before the stream ended", got)
	}
	gate <- struct{}{}
	readEvents(sseContent)
	gate <- struct{}{}
	readEvents(sseDelta + sseStop)

	f.waitStats(t, Stats{Recorded: 1})
	if ev := f.rec.all()[0]; ev.EventID != "msg_stream_1" || ev.OutputTokens != 510 {
		t.Fatalf("event = %+v", ev)
	}
}

// A caller that hangs up part-way gets no event: the final counts were
// never seen (OPEN_QUESTIONS Q19).
func TestStreamAbandonedByCallerIsSkipped(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	f := newFixture(t, streamed(gate, sseStart, sseContent, sseDelta+sseStop))

	req, err := http.NewRequest(http.MethodPost, f.proxy.URL+"/v1/messages", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(TenantHeader, "tenant-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, len(sseStart))); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	f.waitStats(t, Stats{Skipped: 1})
	if n := len(f.rec.all()); n != 0 {
		t.Fatalf("recorded %d events, want 0", n)
	}
}

func TestStreamForwardedButNotMetered(t *testing.T) {
	tests := []struct {
		name    string
		header  map[string]string
		respond http.HandlerFunc
		body    string
	}{
		{name: "no tenant header", header: map[string]string{FeatureHeader: "chat"},
			respond: streamed(nil, sseBody), body: sseBody},
		// The caller asked for gzip itself, so the proxy passes the
		// provider's bytes on without decoding them.
		{name: "compressed stream", header: map[string]string{TenantHeader: "tenant-a", "Accept-Encoding": "gzip"},
			respond: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", "gzip")
				_, _ = io.WriteString(w, "not really gzip")
			}, body: "not really gzip"},
		{name: "error event in the stream", header: tags,
			respond: streamed(nil, sseStart, "event: error\ndata: {\"type\":\"error\"}\n\n"),
			body:    sseStart + "event: error\ndata: {\"type\":\"error\"}\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.respond)
			resp, body := f.post(t, "/v1/messages", tt.header)
			if resp.StatusCode != http.StatusOK || body != tt.body {
				t.Fatalf("got %d %q", resp.StatusCode, body)
			}
			f.waitStats(t, Stats{Skipped: 1})
			if n := len(f.rec.all()); n != 0 {
				t.Fatalf("recorded %d events, want 0", n)
			}
		})
	}
}
