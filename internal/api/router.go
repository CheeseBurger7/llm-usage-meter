package api

import (
	"log/slog"
	"net/http"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
)

// NewRouter wires every route of the service. Every route except /healthz
// requires an API key with the scope named next to it.
func NewRouter(store Ingester, keys auth.Verifier, log *slog.Logger) http.Handler {
	ingestOnly := auth.Require(keys, auth.ScopeIngest, log)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("POST /v1/events", ingestOnly(NewIngestHandler(store, log)))
	return mux
}
