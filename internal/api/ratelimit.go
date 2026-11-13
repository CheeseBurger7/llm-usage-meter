package api

import (
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ratelimit"
)

// KeyLimiter limits requests per API key. A nil *KeyLimiter means no limit.
type KeyLimiter = ratelimit.Limiter[auth.KeyID]

// rateLimit returns middleware that spends one token of the calling key's
// bucket per request, whatever the request holds: a batch of 500 events
// costs the same as a single event. A request over the limit gets 429 with
// Retry-After in whole seconds, before its body is read.
//
// It must run behind auth.Require, which supplies the key id. Requests that
// fail authentication never reach it and are not limited here.
func rateLimit(l *KeyLimiter, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := auth.IdentityFromContext(r.Context())
			if !ok {
				log.ErrorContext(r.Context(), "rate limiter reached without an authenticated identity")
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
				return
			}
			allowed, retryAfter := l.Allow(id.KeyID)
			if !allowed {
				// Rounded up, so a client that waits this long is not refused again.
				secs := max(int64(math.Ceil(retryAfter.Seconds())), 1)
				w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
				writeJSON(w, http.StatusTooManyRequests, errorResponse{Error: "rate limit exceeded, retry after the number of seconds in Retry-After"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
