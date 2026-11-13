// Package api exposes the HTTP interface of the service.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
)

const maxBodyBytes = 1 << 20

// Ingester stores an org's events idempotently. *ingest.Store implements it.
type Ingester interface {
	Ingest(ctx context.Context, org auth.OrgID, ev ingest.Event) (ingest.Result, error)
}

type eventRequest struct {
	EventID          string    `json:"event_id"`
	TenantID         string    `json:"tenant_id"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	Feature          string    `json:"feature"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	LatencyMs        int64     `json:"latency_ms"`
	Timestamp        time.Time `json:"timestamp"`
}

type errorResponse struct {
	Error  string            `json:"error"`
	Fields map[string]string `json:"fields,omitempty"`
}

// NewIngestHandler returns the handler for POST /v1/events. It must sit behind
// auth.Require: the org an event is stored under comes from the verified key in
// the request context. The payload has no org field.
func NewIngestHandler(store Ingester, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFromContext(r.Context())
		if !ok {
			// The route was registered without the auth middleware. Refuse
			// instead of storing an event under no org.
			log.ErrorContext(r.Context(), "ingest handler reached without an identity")
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
			return
		}

		var req eventRequest
		if !decodeBody(w, r, maxBodyBytes, &req) {
			return
		}

		res, err := store.Ingest(r.Context(), id.OrgID, req.event())

		var verr *ingest.ValidationError
		switch {
		case errors.As(err, &verr):
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "validation failed", Fields: verr.Fields})
		case errors.Is(err, ingest.ErrConflict):
			writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		case err != nil:
			log.ErrorContext(r.Context(), "ingest failed", "org_id", id.OrgID.String(), "event_id", req.EventID, "err", err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
		case res == ingest.Created:
			writeJSON(w, http.StatusCreated, map[string]string{"status": "created"})
		default:
			writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		}
	})
}

func (r eventRequest) event() ingest.Event {
	return ingest.Event{
		EventID:          r.EventID,
		TenantID:         r.TenantID,
		Provider:         r.Provider,
		Model:            r.Model,
		Feature:          r.Feature,
		InputTokens:      r.InputTokens,
		OutputTokens:     r.OutputTokens,
		CacheReadTokens:  r.CacheReadTokens,
		CacheWriteTokens: r.CacheWriteTokens,
		LatencyMs:        r.LatencyMs,
		Timestamp:        r.Timestamp,
	}
}

// decodeBody reads one JSON object of at most limit bytes into v, rejecting
// unknown fields and trailing data. On failure it writes the error response
// (400 or 413) and returns false.
func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "request body too large"})
			return false
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "malformed JSON: " + err.Error()})
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "unexpected data after JSON object"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
