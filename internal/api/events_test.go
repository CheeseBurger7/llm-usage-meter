package api_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CheeseBurger7/llm-usage-meter/internal/api"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
)

type fakeIngester struct {
	res ingest.Result
	err error
	got ingest.Event
}

func (f *fakeIngester) Ingest(_ context.Context, ev ingest.Event) (ingest.Result, error) {
	f.got = ev
	return f.res, f.err
}

const validBody = `{"event_id":"e1","tenant_id":"t1","model":"m","input_tokens":10,"output_tokens":5,"latency_ms":120,"timestamp":"2026-01-02T03:04:05Z"}`

func post(t *testing.T, f *fakeIngester, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := api.NewIngestHandler(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body)))
	return rec
}

func TestIngestHandlerStatus(t *testing.T) {
	tests := []struct {
		name string
		f    *fakeIngester
		body string
		want int
	}{
		{"created", &fakeIngester{res: ingest.Created}, validBody, http.StatusCreated},
		{"duplicate", &fakeIngester{res: ingest.Duplicate}, validBody, http.StatusOK},
		{"conflict", &fakeIngester{err: ingest.ErrConflict}, validBody, http.StatusConflict},
		{"validation", &fakeIngester{err: &ingest.ValidationError{Fields: map[string]string{"model": "required"}}}, validBody, http.StatusBadRequest},
		{"internal", &fakeIngester{err: errors.New("db down")}, validBody, http.StatusInternalServerError},
		{"malformed json", &fakeIngester{}, `{`, http.StatusBadRequest},
		{"unknown field", &fakeIngester{}, `{"event_id":"e1","bogus":1}`, http.StatusBadRequest},
		{"trailing data", &fakeIngester{}, validBody + `{}`, http.StatusBadRequest},
		{"too large", &fakeIngester{}, `{"event_id":"` + strings.Repeat("a", 2<<20) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := post(t, tt.f, tt.body).Code; got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIngestHandlerMapsFields(t *testing.T) {
	f := &fakeIngester{res: ingest.Created}
	post(t, f, validBody)
	want := ingest.Event{EventID: "e1", TenantID: "t1", Model: "m", InputTokens: 10, OutputTokens: 5, LatencyMs: 120, Timestamp: f.got.Timestamp}
	if f.got != want || f.got.Timestamp.Year() != 2026 {
		t.Fatalf("got %+v", f.got)
	}
}

func TestIngestHandlerValidationBodyListsFields(t *testing.T) {
	f := &fakeIngester{err: &ingest.ValidationError{Fields: map[string]string{"model": "required"}}}
	rec := post(t, f, validBody)
	if !strings.Contains(rec.Body.String(), `"model":"required"`) {
		t.Fatalf("body = %s", rec.Body)
	}
}
