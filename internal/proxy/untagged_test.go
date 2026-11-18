package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func withDefaultTenant(name string) func(*Config) {
	return func(c *Config) { c.DefaultTenant = name }
}

// A call without the tenant header is metered under the default tenant
// (OPEN_QUESTIONS Q18): every event of the call, streamed or not, Messages
// or chat completions, including declined fallback attempts.
func TestUntaggedCallIsMeteredUnderDefaultTenant(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		respond http.HandlerFunc
		events  int
		opts    []func(*Config)
	}{
		{name: "json", path: "/v1/messages", respond: jsonMessage, events: 1},
		{name: "stream", path: "/v1/messages", respond: streamed(nil, sseBody), events: 1},
		{name: "json fallback", path: "/v1/messages", events: 2,
			respond: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, fallbackBody)
			}},
		{name: "stream fallback", path: "/v1/messages", events: 2,
			respond: streamed(nil,
				"event: message_start\n"+
					`data: {"type":"message_start","message":{"id":"msg_01XFUDYJgAACzvnptvVoYEL","model":"claude-fable-5","usage":{"input_tokens":535,"output_tokens":1}}}`+"\n\n",
				sseContent,
				"event: message_delta\n"+`data: {"type":"message_delta","delta":{},`+strings.Join(strings.Fields(fallbackUsage), "")+`}`+"\n\n",
				sseStop)},
		{name: "chat completion", path: "/v1/chat/completions", respond: jsonChat, events: 1,
			opts: []func(*Config){withChatProvider("openai")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.respond, append(tt.opts, withDefaultTenant(UnassignedTenant))...)
			resp, _ := f.post(t, tt.path, map[string]string{FeatureHeader: "chat"})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			f.waitStats(t, Stats{Recorded: 1, Untagged: 1})
			events := f.rec.all()
			if len(events) != tt.events {
				t.Fatalf("recorded %d events, want %d: %+v", len(events), tt.events, events)
			}
			for _, ev := range events {
				if ev.TenantID != "_unassigned" || ev.Feature != "chat" {
					t.Fatalf("tenant %q, feature %q; want _unassigned, chat", ev.TenantID, ev.Feature)
				}
			}
			logs := f.logs.String()
			if !strings.Contains(logs, "request has no "+TenantHeader+" header") {
				t.Fatalf("log does not give the reason: %s", logs)
			}
		})
	}
}

// The header wins over the default, and such a call is not untagged.
func TestTaggedCallIgnoresDefaultTenant(t *testing.T) {
	f := newFixture(t, jsonMessage, withDefaultTenant(UnassignedTenant))
	f.post(t, "/v1/messages", tags)
	f.waitStats(t, Stats{Recorded: 1})
	events := f.rec.all()
	if len(events) != 1 || events[0].TenantID != "tenant-a" {
		t.Fatalf("events = %+v, want one for tenant-a", events)
	}
	if strings.Contains(f.logs.String(), "default tenant") {
		t.Fatalf("tagged call logged as untagged: %s", f.logs.String())
	}
}

// The default tenant is for the meter only: the proxy never adds it to the
// request it forwards.
func TestDefaultTenantIsNotForwarded(t *testing.T) {
	f := newFixture(t, jsonMessage, withDefaultTenant(UnassignedTenant))
	f.post(t, "/v1/messages", nil)
	f.waitStats(t, Stats{Recorded: 1, Untagged: 1})
	_, _, header, _ := f.upstream.get()
	if _, ok := header[http.CanonicalHeaderKey(TenantHeader)]; ok {
		t.Fatalf("upstream got a tenant header: %v", header)
	}
}

// A call the provider refused or that cannot be metered stays out of
// Untagged: the counter is part of Recorded.
func TestUntaggedButSkippedIsNotCountedAsUntagged(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>gateway</html>")
	}, withDefaultTenant(UnassignedTenant))
	f.post(t, "/v1/messages", nil)
	f.waitStats(t, Stats{Skipped: 1})
}
