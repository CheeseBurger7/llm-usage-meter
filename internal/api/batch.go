package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
)

const (
	// MaxBatchEvents is the most events one batch request may carry.
	MaxBatchEvents = 500
	// maxBatchBodyBytes bounds a batch body. An event with every string at its
	// byte limit is under 2 KiB of JSON unless it is heavily escaped, so 500 of
	// them fit with room to spare.
	maxBatchBodyBytes = 4 << 20
)

// Per-event outcomes in a batch response.
const (
	statusCreated   = "created"
	statusDuplicate = "duplicate"
	statusConflict  = "conflict"
	statusInvalid   = "invalid"
)

// Each event stays raw so that one with a wrong type or an unknown field is
// reported as invalid in its own slot instead of failing the whole body.
type batchRequest struct {
	Events []json.RawMessage `json:"events"`
}

type batchResult struct {
	// EventID is empty when the event could not be decoded.
	EventID string            `json:"event_id"`
	Status  string            `json:"status"`
	Error   string            `json:"error,omitempty"`
	Fields  map[string]string `json:"fields,omitempty"`
}

type batchResponse struct {
	// Results has one entry per event, in request order.
	Results []batchResult `json:"results"`
}

// NewBatchIngestHandler returns the handler for POST /v1/events/batch. Like
// NewIngestHandler it must sit behind auth.Require.
//
// Events are ingested one by one, each in its own statement, with the same
// rules as the single endpoint. The response is 200 with a result per event
// whenever the batch itself was readable, even if every event was rejected. A
// store failure fails the request with 500: events before it are already
// stored, and re-sending the whole batch reports them as duplicates.
func NewBatchIngestHandler(store Ingester, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFromContext(r.Context())
		if !ok {
			log.ErrorContext(r.Context(), "batch ingest handler reached without an identity")
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
			return
		}

		var req batchRequest
		if !decodeBody(w, r, maxBatchBodyBytes, &req) {
			return
		}
		if n := len(req.Events); n == 0 || n > MaxBatchEvents {
			writeJSON(w, http.StatusBadRequest, errorResponse{
				Error: fmt.Sprintf("events must hold 1 to %d events, got %d", MaxBatchEvents, n),
			})
			return
		}

		results := make([]batchResult, len(req.Events))
		for i, raw := range req.Events {
			var ev eventRequest
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&ev); err != nil {
				// Report the event_id if that much is readable, so the client
				// can match the result without counting positions.
				var idOnly struct {
					EventID string `json:"event_id"`
				}
				_ = json.Unmarshal(raw, &idOnly)
				results[i] = batchResult{EventID: idOnly.EventID, Status: statusInvalid, Error: "malformed event: " + err.Error()}
				continue
			}

			res, err := store.Ingest(r.Context(), id.OrgID, ev.event())
			var verr *ingest.ValidationError
			switch {
			case errors.As(err, &verr):
				results[i] = batchResult{EventID: ev.EventID, Status: statusInvalid, Error: "validation failed", Fields: verr.Fields}
			case errors.Is(err, ingest.ErrConflict):
				results[i] = batchResult{EventID: ev.EventID, Status: statusConflict, Error: err.Error()}
			case err != nil:
				log.ErrorContext(r.Context(), "batch ingest failed",
					"org_id", id.OrgID.String(), "event_id", ev.EventID, "index", i, "err", err)
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
				return
			case res == ingest.Created:
				results[i] = batchResult{EventID: ev.EventID, Status: statusCreated}
			default:
				results[i] = batchResult{EventID: ev.EventID, Status: statusDuplicate}
			}
		}
		writeJSON(w, http.StatusOK, batchResponse{Results: results})
	})
}
