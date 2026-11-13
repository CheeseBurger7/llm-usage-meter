package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CheeseBurger7/llm-usage-meter/internal/api"
	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
)

// scriptedIngester answers per event_id and records the order of calls.
type scriptedIngester struct {
	errs   map[string]error
	dups   map[string]bool
	got    []string
	gotOrg []auth.OrgID
}

func (s *scriptedIngester) Ingest(_ context.Context, org auth.OrgID, ev ingest.Event) (ingest.Result, error) {
	s.got = append(s.got, ev.EventID)
	s.gotOrg = append(s.gotOrg, org)
	if err := s.errs[ev.EventID]; err != nil {
		return 0, err
	}
	if s.dups[ev.EventID] {
		return ingest.Duplicate, nil
	}
	return ingest.Created, nil
}

type batchResult struct {
	EventID string            `json:"event_id"`
	Status  string            `json:"status"`
	Error   string            `json:"error"`
	Fields  map[string]string `json:"fields"`
}

func eventJSON(id string) string {
	return strings.Replace(validBody, `"event_id":"e1"`, fmt.Sprintf(`"event_id":%q`, id), 1)
}

func batchBody(events ...string) string {
	return `{"events":[` + strings.Join(events, ",") + `]}`
}

func postBatch(t *testing.T, s api.Ingester, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := api.NewBatchIngestHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/events/batch", strings.NewReader(body))
	ctx := auth.WithIdentity(req.Context(), auth.Identity{OrgID: testOrg, Scope: auth.ScopeIngest})
	h.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func decodeResults(t *testing.T, rec *httptest.ResponseRecorder) []batchResult {
	t.Helper()
	var resp struct {
		Results []batchResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body, err)
	}
	return resp.Results
}

// One result per event, in request order, and a bad event of any kind does not
// stop the ones after it.
func TestBatchHandlerPerEventResults(t *testing.T) {
	s := &scriptedIngester{
		dups: map[string]bool{"dup": true},
		errs: map[string]error{
			"conflict": ingest.ErrConflict,
			"badfield": &ingest.ValidationError{Fields: map[string]string{"model": "required"}},
			// `null` decodes to an empty event, which the real store rejects.
			"": &ingest.ValidationError{Fields: map[string]string{"event_id": "required"}},
		},
	}
	rec := postBatch(t, s, batchBody(
		eventJSON("new"),
		eventJSON("dup"),
		eventJSON("conflict"),
		eventJSON("badfield"),
		`{"event_id":"wrongtype","input_tokens":"ten"}`,
		`{"event_id":"unknown","bogus":1}`,
		`{"event_id":"org","org_id":"00000000-0000-0000-0000-000000000001"}`,
		`null`,
		`5`,
		eventJSON("last"),
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}

	want := []struct{ id, status string }{
		{"new", "created"},
		{"dup", "duplicate"},
		{"conflict", "conflict"},
		{"badfield", "invalid"},
		{"wrongtype", "invalid"},
		{"unknown", "invalid"},
		{"org", "invalid"},
		{"", "invalid"},
		{"", "invalid"},
		{"last", "created"},
	}
	got := decodeResults(t, rec)
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].EventID != w.id || got[i].Status != w.status {
			t.Errorf("result %d = %+v, want id %q status %q", i, got[i], w.id, w.status)
		}
		if failed := w.status == "conflict" || w.status == "invalid"; failed != (got[i].Error != "") {
			t.Errorf("result %d: error = %q for status %q", i, got[i].Error, w.status)
		}
	}
	if got[3].Fields["model"] != "required" {
		t.Errorf("validation result has fields %v", got[3].Fields)
	}

	// Events that could not be decoded never reach the store. The one call with
	// an empty id is the `null` element.
	if calls := strings.Join(s.got, ","); calls != "new,dup,conflict,badfield,,last" {
		t.Errorf("store calls = %s", calls)
	}
	for _, org := range s.gotOrg {
		if org != testOrg {
			t.Fatalf("org = %s, want %s", org, testOrg)
		}
	}
}

func TestBatchHandlerRejectsWholeRequest(t *testing.T) {
	full := make([]string, api.MaxBatchEvents+1)
	for i := range full {
		full[i] = eventJSON(fmt.Sprintf("e%d", i))
	}
	tests := []struct {
		name string
		body string
		want int
	}{
		{"malformed json", `{"events":[`, http.StatusBadRequest},
		{"not an object", `[` + validBody + `]`, http.StatusBadRequest},
		{"events not an array", `{"events":{}}`, http.StatusBadRequest},
		{"unknown top-level field", `{"events":[` + validBody + `],"bogus":1}`, http.StatusBadRequest},
		{"trailing data", batchBody(validBody) + `{}`, http.StatusBadRequest},
		{"missing events", `{}`, http.StatusBadRequest},
		{"empty events", `{"events":[]}`, http.StatusBadRequest},
		{"over the event limit", batchBody(full...), http.StatusBadRequest},
		{"too large", batchBody(`{"event_id":"` + strings.Repeat("a", 5<<20) + `"}`), http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &scriptedIngester{}
			if got := postBatch(t, s, tt.body).Code; got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
			if len(s.got) != 0 {
				t.Fatalf("store was called %d times", len(s.got))
			}
		})
	}
}

func TestBatchHandlerAcceptsMaxEvents(t *testing.T) {
	full := make([]string, api.MaxBatchEvents)
	for i := range full {
		full[i] = eventJSON(fmt.Sprintf("e%d", i))
	}
	s := &scriptedIngester{}
	rec := postBatch(t, s, batchBody(full...))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := len(decodeResults(t, rec)); got != api.MaxBatchEvents || len(s.got) != api.MaxBatchEvents {
		t.Fatalf("results = %d, store calls = %d, want %d", got, len(s.got), api.MaxBatchEvents)
	}
}

// A store failure is not a per-event outcome: the request fails, no partial
// results are returned, and later events are not attempted.
func TestBatchHandlerStoreFailure(t *testing.T) {
	s := &scriptedIngester{errs: map[string]error{"b": errors.New("db down")}}
	rec := postBatch(t, s, batchBody(eventJSON("a"), eventJSON("b"), eventJSON("c")))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "results") || strings.Contains(rec.Body.String(), "db down") {
		t.Fatalf("body = %s", rec.Body)
	}
	if calls := strings.Join(s.got, ","); calls != "a,b" {
		t.Fatalf("store calls = %s, want a,b", calls)
	}
}

func TestBatchHandlerWithoutIdentity(t *testing.T) {
	s := &scriptedIngester{}
	h := api.NewBatchIngestHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/events/batch", strings.NewReader(batchBody(validBody))))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(s.got) != 0 {
		t.Fatal("store was called")
	}
}
