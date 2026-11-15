package main

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
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
