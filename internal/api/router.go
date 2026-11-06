package api

import (
	"log/slog"
	"net/http"
)

// NewRouter wires every route of the service.
func NewRouter(store Ingester, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("POST /v1/events", NewIngestHandler(store, log))
	return mux
}
