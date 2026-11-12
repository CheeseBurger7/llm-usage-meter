package auth_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
)

const testKey = "lum_ingest_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fakeVerifier answers Verify from fixed values and records the key it saw.
type fakeVerifier struct {
	id     auth.Identity
	err    error
	got    string
	called bool
}

func (f *fakeVerifier) Verify(_ context.Context, key string) (auth.Identity, error) {
	f.called = true
	f.got = key
	return f.id, f.err
}

// serve runs one request through Require(ScopeIngest) and reports whether the
// wrapped handler ran, with the identity it saw.
func serve(t *testing.T, v auth.Verifier, logs *bytes.Buffer, header string) (rec *httptest.ResponseRecorder, reached bool, seen auth.Identity) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		seen, _ = auth.IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	log := slog.New(slog.NewTextHandler(logs, nil))
	req := httptest.NewRequest(http.MethodPost, "/v1/events", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec = httptest.NewRecorder()
	auth.Require(v, auth.ScopeIngest, log)(next).ServeHTTP(rec, req)
	return rec, reached, seen
}

func TestRequireStatus(t *testing.T) {
	org := auth.OrgID{1}
	ingest := auth.Identity{OrgID: org, Scope: auth.ScopeIngest}
	tests := []struct {
		name       string
		header     string
		v          *fakeVerifier
		want       int
		wantVerify bool
	}{
		{"valid key", "Bearer " + testKey, &fakeVerifier{id: ingest}, http.StatusNoContent, true},
		{"scheme is case-insensitive", "bearer " + testKey, &fakeVerifier{id: ingest}, http.StatusNoContent, true},
		{"no header", "", &fakeVerifier{id: ingest}, http.StatusUnauthorized, false},
		{"other scheme", "Basic " + testKey, &fakeVerifier{id: ingest}, http.StatusUnauthorized, false},
		{"scheme only", "Bearer", &fakeVerifier{id: ingest}, http.StatusUnauthorized, false},
		{"empty token", "Bearer   ", &fakeVerifier{id: ingest}, http.StatusUnauthorized, false},
		{"two tokens", "Bearer " + testKey + " extra", &fakeVerifier{id: ingest}, http.StatusUnauthorized, false},
		{"bare key", testKey, &fakeVerifier{id: ingest}, http.StatusUnauthorized, false},
		{"unknown key", "Bearer " + testKey, &fakeVerifier{err: auth.ErrInvalidKey}, http.StatusUnauthorized, true},
		{"revoked key", "Bearer " + testKey, &fakeVerifier{err: auth.ErrRevokedKey}, http.StatusUnauthorized, true},
		{"wrapped invalid key", "Bearer " + testKey, &fakeVerifier{err: fmt.Errorf("verify: %w", auth.ErrInvalidKey)}, http.StatusUnauthorized, true},
		{"wrong scope", "Bearer " + testKey, &fakeVerifier{id: auth.Identity{OrgID: org, Scope: auth.ScopeRead}}, http.StatusForbidden, true},
		{"verifier failure", "Bearer " + testKey, &fakeVerifier{err: errors.New("db down")}, http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, reached, seen := serve(t, tt.v, &bytes.Buffer{}, tt.header)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.v.called != tt.wantVerify {
				t.Fatalf("Verify called = %v, want %v", tt.v.called, tt.wantVerify)
			}
			ok := tt.want == http.StatusNoContent
			if reached != ok {
				t.Fatalf("handler reached = %v, want %v", reached, ok)
			}
			if ok {
				if seen != ingest {
					t.Fatalf("identity in context = %+v, want %+v", seen, ingest)
				}
				if tt.v.got != testKey {
					t.Fatalf("Verify got %q, want the key alone", tt.v.got)
				}
				return
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q", ct)
			}
			if !strings.Contains(rec.Body.String(), `"error":`) {
				t.Fatalf("body = %s", rec.Body)
			}
			challenge := rec.Header().Get("WWW-Authenticate")
			if (tt.want == http.StatusUnauthorized) != (challenge == "Bearer") {
				t.Fatalf("WWW-Authenticate = %q for status %d", challenge, tt.want)
			}
		})
	}
}

// Unknown and revoked keys must look the same from outside.
func TestRequireRevokedLooksLikeUnknown(t *testing.T) {
	unknown, _, _ := serve(t, &fakeVerifier{err: auth.ErrInvalidKey}, &bytes.Buffer{}, "Bearer "+testKey)
	revoked, _, _ := serve(t, &fakeVerifier{err: auth.ErrRevokedKey}, &bytes.Buffer{}, "Bearer "+testKey)
	if unknown.Code != revoked.Code || unknown.Body.String() != revoked.Body.String() {
		t.Fatalf("unknown = %d %s, revoked = %d %s", unknown.Code, unknown.Body, revoked.Code, revoked.Body)
	}
}

// The key must not reach the log or the response on any path.
func TestRequireNeverLeaksKey(t *testing.T) {
	secret := testKey[len("lum_ingest_"):]
	for name, v := range map[string]*fakeVerifier{
		"valid":            {id: auth.Identity{Scope: auth.ScopeIngest}},
		"unknown":          {err: auth.ErrInvalidKey},
		"revoked":          {err: auth.ErrRevokedKey},
		"wrong scope":      {id: auth.Identity{Scope: auth.ScopeRead}},
		"verifier failure": {err: errors.New("db down")},
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			rec, _, _ := serve(t, v, &logs, "Bearer "+testKey)
			if strings.Contains(logs.String(), secret) {
				t.Fatalf("key in log: %s", logs.String())
			}
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("key in response: %s", rec.Body)
			}
		})
	}
	// The one path that does log still says what went wrong.
	var logs bytes.Buffer
	serve(t, &fakeVerifier{err: errors.New("db down")}, &logs, "Bearer "+testKey)
	if !strings.Contains(logs.String(), "db down") {
		t.Fatalf("verifier failure not logged: %q", logs.String())
	}
}

func TestIdentityFromContextEmpty(t *testing.T) {
	if _, ok := auth.IdentityFromContext(context.Background()); ok {
		t.Fatal("identity found in an empty context")
	}
}
