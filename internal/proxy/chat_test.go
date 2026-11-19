package proxy

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/CheeseBurger7/llm-usage-meter/client"
)

// chatBody has the shape of a ChatCompletion from OpenAI's API spec,
// including fields the proxy must ignore.
const chatBody = `{
  "id": "chatcmpl-B9MHDbslfkBeAs8l4bebGdFOJ6PeG",
  "object": "chat.completion",
  "created": 1741570283,
  "model": "gpt-5.1-2025-11-13",
  "choices": [{"index": 0, "message": {"role": "assistant", "content": "` + secretCompletion + `",
    "refusal": null, "annotations": []}, "logprobs": null, "finish_reason": "stop"}],
  "usage": {
    "prompt_tokens": 1117,
    "completion_tokens": 46,
    "total_tokens": 1163,
    "prompt_tokens_details": {"cached_tokens": 1024, "audio_tokens": 0},
    "completion_tokens_details": {"reasoning_tokens": 12, "audio_tokens": 0,
      "accepted_prediction_tokens": 0, "rejected_prediction_tokens": 0}
  },
  "service_tier": "default",
  "system_fingerprint": "fp_fc9f1d7035"
}`

func jsonChat(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, chatBody)
}

func withChatProvider(name string) func(*Config) {
	return func(c *Config) { c.ChatProvider = name }
}

func TestChatEvent(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		want   client.Event
		reason bool // true: not metered
	}{
		{name: "cached tokens are taken out of the input count", body: chatBody,
			want: client.Event{EventID: "chatcmpl-B9MHDbslfkBeAs8l4bebGdFOJ6PeG", Provider: "openai",
				Model: "gpt-5.1-2025-11-13", InputTokens: 93, OutputTokens: 46, CacheReadTokens: 1024}},
		{name: "no details block",
			body: `{"id":"c1","object":"chat.completion","model":"m","usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
			want: client.Event{EventID: "c1", Provider: "openai", Model: "m", InputTokens: 7, OutputTokens: 3}},
		{name: "null details and null cached count",
			body: `{"id":"c1","object":"chat.completion","model":"m","usage":{"prompt_tokens":7,"completion_tokens":3,
				"prompt_tokens_details":null,"completion_tokens_details":null}}`,
			want: client.Event{EventID: "c1", Provider: "openai", Model: "m", InputTokens: 7, OutputTokens: 3}},
		{name: "cache writes stay in the input count (Q22)",
			body: `{"id":"c1","object":"chat.completion","model":"m","usage":{"prompt_tokens":100,"completion_tokens":3,
				"prompt_tokens_details":{"cached_tokens":10,"cache_write_tokens":40}}}`,
			want: client.Event{EventID: "c1", Provider: "openai", Model: "m", InputTokens: 90, OutputTokens: 3, CacheReadTokens: 10}},
		{name: "zero counts are recorded",
			body: `{"id":"c1","object":"chat.completion","model":"m","usage":{"prompt_tokens":0,"completion_tokens":0}}`,
			want: client.Event{EventID: "c1", Provider: "openai", Model: "m"}},
		{name: "malformed JSON", body: `{"id":"c1",`, reason: true},
		{name: "wrong object type", body: `{"id":"c1","object":"text_completion","model":"m","usage":{"prompt_tokens":1,"completion_tokens":1}}`, reason: true},
		{name: "a stream chunk", body: `{"id":"c1","object":"chat.completion.chunk","model":"m","usage":{"prompt_tokens":1,"completion_tokens":1}}`, reason: true},
		{name: "no id", body: `{"object":"chat.completion","model":"m","usage":{"prompt_tokens":1,"completion_tokens":1}}`, reason: true},
		{name: "no model", body: `{"id":"c1","object":"chat.completion","usage":{"prompt_tokens":1,"completion_tokens":1}}`, reason: true},
		{name: "no usage", body: `{"id":"c1","object":"chat.completion","model":"m"}`, reason: true},
		{name: "null usage", body: `{"id":"c1","object":"chat.completion","model":"m","usage":null}`, reason: true},
		{name: "no prompt count", body: `{"id":"c1","object":"chat.completion","model":"m","usage":{"completion_tokens":1}}`, reason: true},
		{name: "no completion count", body: `{"id":"c1","object":"chat.completion","model":"m","usage":{"prompt_tokens":1}}`, reason: true},
		{name: "more cached than prompt tokens",
			body:   `{"id":"c1","object":"chat.completion","model":"m","usage":{"prompt_tokens":5,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":6}}}`,
			reason: true},
		{name: "negative count", body: `{"id":"c1","object":"chat.completion","model":"m","usage":{"prompt_tokens":-1,"completion_tokens":1}}`, reason: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := chatEvent([]byte(tt.body), "openai")
			if tt.reason {
				if reason == "" {
					t.Fatalf("metered %+v, want a skip reason", got)
				}
				return
			}
			if reason != "" || got != tt.want {
				t.Fatalf("event = %+v (%q), want %+v", got, reason, tt.want)
			}
		})
	}
}

func TestChatCompletionIsForwardedUnchangedAndMetered(t *testing.T) {
	f := newFixture(t, jsonChat, withChatProvider("OpenAI"))
	resp, body := f.post(t, "/v1/chat/completions", tags)
	if resp.StatusCode != http.StatusOK || body != chatBody {
		t.Fatalf("status %d, body changed: %q", resp.StatusCode, body)
	}
	_, uri, header, _ := f.upstream.get()
	if uri != "/v1/chat/completions" || header.Get(TenantHeader) != "" || header.Get(FeatureHeader) != "" {
		t.Fatalf("upstream got %s with headers %v", uri, header)
	}

	f.waitStats(t, Stats{Recorded: 1})
	events := f.rec.all()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	got := events[0]
	if got.Timestamp.IsZero() || got.LatencyMs < 0 {
		t.Fatalf("timestamp %v or latency %d not set", got.Timestamp, got.LatencyMs)
	}
	got.Timestamp, got.LatencyMs = time.Time{}, 0
	want := client.Event{
		EventID: "chatcmpl-B9MHDbslfkBeAs8l4bebGdFOJ6PeG", TenantID: "tenant-a", Feature: "chat",
		Provider: "openai", // the configured label, lowercased as ingest would (Q13)
		Model:    "gpt-5.1-2025-11-13", InputTokens: 93, OutputTokens: 46, CacheReadTokens: 1024,
	}
	if got != want {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
}

// Every case is forwarded exactly as the provider answered; none produces
// an event.
func TestChatCompletionForwardedButNotMetered(t *testing.T) {
	const chunk = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[]}\n\ndata: [DONE]\n\n"
	streamChat := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chunk)
	}
	tests := []struct {
		name     string
		provider string
		header   map[string]string
		respond  http.HandlerFunc
		body     string
		skipped  uint64
	}{
		{name: "no provider label configured", header: tags, respond: jsonChat, body: chatBody, skipped: 1},
		{name: "no tenant header", provider: "openai", header: nil, respond: jsonChat, body: chatBody, skipped: 1},
		{name: "streamed", provider: "openai", header: tags, respond: streamChat, body: chunk, skipped: 1},
		{name: "not a chat completion", provider: "openai", header: tags, respond: jsonMessage, body: messageBody, skipped: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.respond, withChatProvider(tt.provider))
			resp, body := f.post(t, "/v1/chat/completions", tt.header)
			if resp.StatusCode != http.StatusOK || body != tt.body {
				t.Fatalf("got %d %q, want 200 %q", resp.StatusCode, body, tt.body)
			}
			f.waitStats(t, Stats{Skipped: tt.skipped})
			if n := len(f.rec.all()); n != 0 {
				t.Fatalf("recorded %d events, want 0", n)
			}
		})
	}
}

// The chat completion body must not reach the log either, including when
// the call is skipped for want of a provider label.
func TestChatLogsNeverContainBodiesOrKeys(t *testing.T) {
	for _, provider := range []string{"openai", ""} {
		f := newFixture(t, jsonChat, withChatProvider(provider))
		f.post(t, "/v1/chat/completions?key="+secretKey, tags)
		if provider == "" {
			f.waitStats(t, Stats{Skipped: 1})
		} else {
			f.waitStats(t, Stats{Recorded: 1})
		}
		logs := f.logs.String()
		for _, secret := range []string{secretKey, secretPrompt, secretCompletion, "tenant-a"} {
			if strings.Contains(logs, secret) {
				t.Fatalf("provider %q: log contains %q:\n%s", provider, secret, logs)
			}
		}
	}
}

// New refuses a chat provider label that ingest would refuse (Q20), and
// folds only A-Z, as ingest does.
func TestNewChecksChatProvider(t *testing.T) {
	upstream, _ := url.Parse("http://upstream.invalid")
	for _, bad := range []string{"self hosted", "ópenai", "open/ai"} {
		if _, err := New(Config{Upstream: upstream, Recorder: &fakeRecorder{}, ChatProvider: bad}); err == nil {
			t.Errorf("%q: New succeeded, want an error", bad)
		}
	}
	for _, ok := range []string{"", "openai", "Azure-OpenAI", "self-hosted.v1"} {
		if _, err := New(Config{Upstream: upstream, Recorder: &fakeRecorder{}, ChatProvider: ok}); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}
