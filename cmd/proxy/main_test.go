package main

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"METER_ENDPOINT":   "https://meter.example.com",
		"METER_INGEST_KEY": "lum_ingest_abc",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.listenAddr != "127.0.0.1:8081" || cfg.upstream.String() != "https://api.anthropic.com" {
		t.Fatalf("defaults = %q, %q", cfg.listenAddr, cfg.upstream)
	}
	if cfg.meterEndpoint != "https://meter.example.com" || cfg.meterKey != "lum_ingest_abc" {
		t.Fatalf("meter settings = %q, %q", cfg.meterEndpoint, cfg.meterKey)
	}
	if cfg.chatProvider != "anthropic" {
		t.Fatalf("chat provider for the default upstream = %q", cfg.chatProvider)
	}
	if cfg.defaultTenant != "_unassigned" {
		t.Fatalf("default tenant = %q, want _unassigned", cfg.defaultTenant)
	}
}

func TestLoadConfigDefaultTenant(t *testing.T) {
	tests := map[string]struct {
		set   bool
		value string
		want  string
	}{
		"unset":  {want: "_unassigned"},
		"empty":  {set: true, value: "", want: ""}, // forward untagged calls without metering
		"custom": {set: true, value: "untagged-calls", want: "untagged-calls"},
	}
	for name, tt := range tests {
		m := map[string]string{"METER_ENDPOINT": "https://meter.example.com", "METER_INGEST_KEY": "lum_ingest_abc"}
		if tt.set {
			m["PROXY_DEFAULT_TENANT"] = tt.value
		}
		cfg, err := loadConfig(env(m))
		if err != nil {
			t.Fatalf("%s: loadConfig: %v", name, err)
		}
		if cfg.defaultTenant != tt.want {
			t.Errorf("%s: default tenant = %q, want %q", name, cfg.defaultTenant, tt.want)
		}
	}
}

func TestLoadConfigChatProvider(t *testing.T) {
	tests := []struct {
		upstream, setting, want string
	}{
		{upstream: "https://api.openai.com", want: "openai"},
		{upstream: "https://API.OpenAI.com:443/", want: "openai"},
		{upstream: "https://api.anthropic.com", want: "anthropic"},
		{upstream: "http://gateway.internal:9000", want: ""}, // unknown host: not guessed
		{upstream: "http://vllm.internal:8000", setting: "Self-Hosted", want: "self-hosted"},
		{upstream: "https://api.openai.com", setting: "azure-openai", want: "azure-openai"},
	}
	for _, tt := range tests {
		cfg, err := loadConfig(env(map[string]string{
			"METER_ENDPOINT":            "https://meter.example.com",
			"METER_INGEST_KEY":          "lum_ingest_abc",
			"UPSTREAM_URL":              tt.upstream,
			"CHAT_COMPLETIONS_PROVIDER": tt.setting,
		}))
		if err != nil {
			t.Fatalf("%s: loadConfig: %v", tt.upstream, err)
		}
		if cfg.chatProvider != tt.want {
			t.Errorf("%s with %q: chat provider = %q, want %q", tt.upstream, tt.setting, cfg.chatProvider, tt.want)
		}
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"METER_ENDPOINT":   "https://meter.example.com",
		"METER_INGEST_KEY": "lum_ingest_abc",
		"UPSTREAM_URL":     "http://gateway.internal:9000/anthropic",
		"LISTEN_ADDR":      ":9001",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.listenAddr != ":9001" || cfg.upstream.String() != "http://gateway.internal:9000/anthropic" {
		t.Fatalf("got %q, %q", cfg.listenAddr, cfg.upstream)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	const key = "lum_ingest_SECRET"
	tests := map[string]struct {
		env  map[string]string
		want string
	}{
		"no endpoint": {map[string]string{"METER_INGEST_KEY": key}, "METER_ENDPOINT"},
		"no key":      {map[string]string{"METER_ENDPOINT": "https://m.example.com"}, "METER_INGEST_KEY"},
		"bad upstream": {map[string]string{
			"METER_ENDPOINT": "https://m.example.com", "METER_INGEST_KEY": key, "UPSTREAM_URL": "api.anthropic.com",
		}, "UPSTREAM_URL"},
		"default tenant too long": {map[string]string{
			"METER_ENDPOINT": "https://m.example.com", "METER_INGEST_KEY": key,
			"PROXY_DEFAULT_TENANT": strings.Repeat("t", 257),
		}, "PROXY_DEFAULT_TENANT"},
	}
	for name, tt := range tests {
		_, err := loadConfig(env(tt.env))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to name %s", name, err, tt.want)
		}
		if err != nil && strings.Contains(err.Error(), key) {
			t.Errorf("%s: the error contains the key", name)
		}
	}
}
