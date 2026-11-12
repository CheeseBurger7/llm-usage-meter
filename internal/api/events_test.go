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
	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
)

type fakeIngester struct {
	res    ingest.Result
	err    error
	got    ingest.Event
	gotOrg auth.OrgID
	calls  int
}

func (f *fakeIngester) Ingest(_ context.Context, org auth.OrgID, ev ingest.Event) (ingest.Result, error) {
	f.calls++
	f.gotOrg = org
	f.got = ev
	return f.res, f.err
}

// testOrg is the org of the identity that post puts in the request context.
var testOrg = auth.OrgID{0xA1}

const validBody = `{"event_id":"e1","tenant_id":"t1","provider":"p","model":"m","input_tokens":10,"output_tokens":5,"latency_ms":120,"timestamp":"2026-01-02T03:04:05Z"}`

func post(t *testing.T, f *fakeIngester, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := api.NewIngestHandler(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	ctx := auth.WithIdentity(req.Context(), auth.Identity{OrgID: testOrg, Scope: auth.ScopeIngest})
	h.ServeHTTP(rec, req.WithContext(ctx))
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
	want := ingest.Event{EventID: "e1", TenantID: "t1", Provider: "p", Model: "m", InputTokens: 10, OutputTokens: 5, LatencyMs: 120, Timestamp: f.got.Timestamp}
	if f.got != want || f.got.Timestamp.Year() != 2026 {
		t.Fatalf("got %+v", f.got)
	}
}

// feature and the cache token counts are optional in the payload.
func TestIngestHandlerMapsOptionalFields(t *testing.T) {
	f := &fakeIngester{res: ingest.Created}
	body := strings.Replace(validBody, `{`, `{"feature":"chat","cache_read_tokens":30,"cache_write_tokens":20,`, 1)
	if got := post(t, f, body).Code; got != http.StatusCreated {
		t.Fatalf("status = %d, want 201", got)
	}
	if f.got.Feature != "chat" || f.got.CacheReadTokens != 30 || f.got.CacheWriteTokens != 20 {
		t.Fatalf("got %+v", f.got)
	}
}

func TestIngestHandlerTakesOrgFromContext(t *testing.T) {
	f := &fakeIngester{res: ingest.Created}
	post(t, f, validBody)
	if f.gotOrg != testOrg {
		t.Fatalf("org = %s, want %s", f.gotOrg, testOrg)
	}
}

// The payload cannot name an org: the field is unknown and the request is rejected.
func TestIngestHandlerRejectsOrgInPayload(t *testing.T) {
	f := &fakeIngester{res: ingest.Created}
	body := strings.Replace(validBody, `{`, `{"org_id":"00000000-0000-0000-0000-000000000001",`, 1)
	if got := post(t, f, body).Code; got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got)
	}
	if f.calls != 0 {
		t.Fatal("store was called")
	}
}

// Without the auth middleware there is no org, so nothing may be stored.
func TestIngestHandlerWithoutIdentity(t *testing.T) {
	f := &fakeIngester{res: ingest.Created}
	h := api.NewIngestHandler(f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(validBody)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if f.calls != 0 {
		t.Fatal("store was called")
	}
}

func TestIngestHandlerValidationBodyListsFields(t *testing.T) {
	f := &fakeIngester{err: &ingest.ValidationError{Fields: map[string]string{"model": "required"}}}
	rec := post(t, f, validBody)
	if !strings.Contains(rec.Body.String(), `"model":"required"`) {
		t.Fatalf("body = %s", rec.Body)
	}
}
