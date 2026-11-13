package api

import (
	"log/slog"
	"net/http"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
)

// NewRouter wires every route of the service. Every route except /healthz
// requires an API key with the scope named next to it. The ingest routes
// share one rate limit bucket per key; a nil limit turns rate limiting off.
func NewRouter(store Ingester, keys auth.Verifier, limit *KeyLimiter, log *slog.Logger) http.Handler {
	requireIngest := auth.Require(keys, auth.ScopeIngest, log)
	limited := rateLimit(limit, log)
	// Authentication runs first: the limiter needs the key id it provides.
	ingestOnly := func(h http.Handler) http.Handler { return requireIngest(limited(h)) }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("POST /v1/events", ingestOnly(NewIngestHandler(store, log)))
	mux.Handle("POST /v1/events/batch", ingestOnly(NewBatchIngestHandler(store, log)))
	return mux
}
