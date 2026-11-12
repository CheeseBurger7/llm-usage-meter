package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// Verifier checks a plaintext API key. *Service implements it.
type Verifier interface {
	Verify(ctx context.Context, key string) (Identity, error)
}

type identityKey struct{}

// WithIdentity returns a context that carries the verified identity.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFromContext returns the identity that Require stored, and false when
// the request did not pass through it.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Require returns middleware that lets a request through only when it carries
// `Authorization: Bearer <key>` with a valid key of the given scope. The
// handler behind it finds the org with IdentityFromContext.
//
//   - 401: header missing or malformed, key unknown or revoked.
//   - 403: the key is valid but has a different scope.
//   - 500: the key could not be checked. The request is refused, never let
//     through.
//
// The key and the Authorization header are never logged.
func Require(v Verifier, scope Scope, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				unauthorized(w, "missing or malformed Authorization header, want: Bearer <key>")
				return
			}
			id, err := v.Verify(r.Context(), key)
			switch {
			case errors.Is(err, ErrInvalidKey), errors.Is(err, ErrRevokedKey):
				// One answer for both, so a caller cannot tell that a key once existed.
				unauthorized(w, "invalid API key")
				return
			case err != nil:
				log.ErrorContext(r.Context(), "key verification failed", "err", err)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			if id.Scope != scope {
				writeError(w, http.StatusForbidden, "this route needs a key with the "+string(scope)+" scope")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
		})
	}
}

// bearerToken extracts the token from an Authorization header value. The
// scheme is case-insensitive (RFC 9110).
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, msg)
}

// writeError writes the same {"error": "..."} body the API handlers use.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
