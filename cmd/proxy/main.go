// Command proxy is the capture proxy a customer runs next to their app. See
// docs/proxy.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/CheeseBurger7/llm-usage-meter/client"
	"github.com/CheeseBurger7/llm-usage-meter/internal/provider"
	"github.com/CheeseBurger7/llm-usage-meter/internal/proxy"
)

const (
	defaultUpstream = "https://api.anthropic.com"
	// Loopback by default: the proxy has no authentication of its own, so
	// reaching it from other hosts is something the operator opts into.
	defaultListenAddr = "127.0.0.1:8081"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("proxy failed", "err", err)
		os.Exit(1)
	}
}

type config struct {
	listenAddr    string
	upstream      *url.URL
	meterEndpoint string
	meterKey      string
	// chatProvider labels events from OpenAI-compatible chat completions.
	// Empty: those calls are forwarded but not metered.
	chatProvider string
	// defaultTenant is the tenant of calls without X-Meter-Tenant. Empty:
	// those calls are forwarded but not metered.
	defaultTenant string
	// billedRefusals replaces the built-in list of refusal categories the
	// provider bills before any output. Nil: the built-in list.
	billedRefusals []string
}

// maxTenantLen matches the tenant_id limit in ingest; a longer default
// tenant would get every untagged event refused.
const maxTenantLen = 256

// knownChatProviders gives the provider label for chat completions when the
// upstream is a provider's own API host. Any other host, such as a gateway
// or a self-hosted server, needs CHAT_COMPLETIONS_PROVIDER: a guessed label
// would put the usage on the wrong price book entry (OPEN_QUESTIONS Q23).
var knownChatProviders = map[string]string{
	"api.openai.com":    "openai",
	"api.anthropic.com": "anthropic", // the provider's OpenAI SDK compatibility layer
}

// loadConfig reads the configuration from the environment. Error messages
// name the variable and never include its value. lookupEnv has the
// signature of os.LookupEnv, because PROXY_DEFAULT_TENANT set to empty
// means something different from PROXY_DEFAULT_TENANT unset.
func loadConfig(lookupEnv func(string) (string, bool)) (config, error) {
	getenv := func(k string) string { v, _ := lookupEnv(k); return v }
	cfg := config{
		listenAddr:    getenv("LISTEN_ADDR"),
		meterEndpoint: getenv("METER_ENDPOINT"),
		meterKey:      getenv("METER_INGEST_KEY"),
	}
	if cfg.listenAddr == "" {
		cfg.listenAddr = defaultListenAddr
	}
	if cfg.meterEndpoint == "" {
		return config{}, errors.New("METER_ENDPOINT is required")
	}
	if cfg.meterKey == "" {
		return config{}, errors.New("METER_INGEST_KEY is required")
	}
	raw := getenv("UPSTREAM_URL")
	if raw == "" {
		raw = defaultUpstream
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return config{}, errors.New("UPSTREAM_URL must be an http or https URL with a host")
	}
	cfg.upstream = u
	// The label must pass the ingest rule (Q20), or ingest would refuse every
	// chat completion event the proxy sends. Checked at startup so a bad value
	// stops the proxy instead of losing usage silently.
	cfg.chatProvider = provider.Normalize(getenv("CHAT_COMPLETIONS_PROVIDER"))
	if cfg.chatProvider != "" {
		if reason := provider.Check(cfg.chatProvider); reason != "" {
			return config{}, fmt.Errorf("CHAT_COMPLETIONS_PROVIDER %s", reason)
		}
	} else {
		cfg.chatProvider = knownChatProviders[strings.ToLower(u.Hostname())]
	}
	// Unset: meter untagged calls under the reserved tenant (Q18). Set to
	// empty: forward them without metering, as before.
	tenant, ok := lookupEnv("PROXY_DEFAULT_TENANT")
	if !ok {
		tenant = proxy.UnassignedTenant
	}
	if len(tenant) > maxTenantLen {
		return config{}, fmt.Errorf("PROXY_DEFAULT_TENANT must be at most %d bytes", maxTenantLen)
	}
	cfg.defaultTenant = tenant
	// Unset: the built-in list (Q21). Set: a comma-separated list that
	// replaces it; set to empty bills no refusal without output.
	if raw, ok := lookupEnv("PROXY_BILLED_REFUSAL_CATEGORIES"); ok {
		cfg.billedRefusals = parseList(raw)
		for _, c := range cfg.billedRefusals {
			if !proxy.ValidRefusalCategory(c) {
				return config{}, errors.New("PROXY_BILLED_REFUSAL_CATEGORIES entries must be lowercase letters, digits or underscores, at most 64 each")
			}
		}
	}
	return cfg, nil
}

// parseList splits a comma-separated value, trimming spaces and dropping
// empty entries. The result is never nil, so an empty value can be told
// apart from an unset one.
func parseList(raw string) []string {
	out := []string{}
	for _, v := range strings.Split(raw, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig(os.LookupEnv)
	if err != nil {
		return err
	}
	meter, err := client.New(cfg.meterEndpoint, cfg.meterKey, client.WithLogger(log))
	if err != nil {
		return fmt.Errorf("METER_ENDPOINT or METER_INGEST_KEY: %w", err)
	}
	handler, err := proxy.New(proxy.Config{
		Upstream: cfg.upstream, Recorder: meter, Log: log, ChatProvider: cfg.chatProvider,
		DefaultTenant: cfg.defaultTenant, BilledRefusalCategories: cfg.billedRefusals,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No read or write timeout: an LLM call can take minutes.
	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.listenAddr, "upstream", cfg.upstream.Host,
			"chat_completions_provider", cfg.chatProvider,
			"default_tenant", cfg.defaultTenant,
			"billed_refusal_categories", billedList(cfg.billedRefusals))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err = <-serveErr:
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err = srv.Shutdown(shutdownCtx)
		cancel()
	}

	// In-flight calls are finished; now send the events they produced.
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if cerr := meter.Close(closeCtx); cerr != nil {
		log.Warn("some usage events were not delivered", "err", cerr.Error())
	}
	ps, ms := handler.Stats(), meter.Stats()
	log.Info("stopped", "calls_recorded", ps.Recorded, "calls_skipped", ps.Skipped,
		"calls_untagged", ps.Untagged, "calls_partial", ps.Partial,
		"refusals_unbilled", ps.RefusalsUnbilled, "refusals_unknown_category", ps.RefusalsUnknownCategory,
		"fallback_usage_mismatches", ps.FallbackUsageMismatches,
		"events_sent", ms.Sent, "events_rejected", ms.Rejected, "events_dropped", ms.Dropped)
	return err
}

// billedList is the billed refusal list in effect, for the startup log.
func billedList(configured []string) string {
	if configured == nil {
		configured = proxy.DefaultBilledRefusalCategories
	}
	return strings.Join(configured, ",")
}
