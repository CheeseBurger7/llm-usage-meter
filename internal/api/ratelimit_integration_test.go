package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
)

// The bucket is chosen by the key id that Verify reads from api_keys, so this
// runs against Postgres: two real keys of one org must not share a bucket,
// and a refused request must store nothing.
func TestRateLimitPerStoredKey(t *testing.T) {
	srv, keys, pool := startServerWithLimit(t, newLimiter(t, 2))
	ctx := context.Background()

	org, err := keys.CreateOrg(ctx, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	newKey := func() string {
		t.Helper()
		k, err := keys.CreateKey(ctx, org.ID, auth.ScopeIngest)
		if err != nil {
			t.Fatal(err)
		}
		return k.Plaintext
	}
	first, second := newKey(), newKey()

	send := func(key, eventID string) (status int, retryAfter string) {
		t.Helper()
		body := strings.Replace(validBody, `"event_id":"e1"`, `"event_id":"`+eventID+`"`, 1)
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/events", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header.Get("Retry-After")
	}

	for _, id := range []string{"a1", "a2"} {
		if status, _ := send(first, id); status != http.StatusCreated {
			t.Fatalf("%s: status = %d, want 201", id, status)
		}
	}
	status, retryAfter := send(first, "a3")
	if status != http.StatusTooManyRequests || retryAfter == "" {
		t.Fatalf("over the limit: status = %d, Retry-After = %q; want 429 with a value", status, retryAfter)
	}
	if status, _ := send(second, "b1"); status != http.StatusCreated {
		t.Fatalf("second key of the same org: status = %d, want 201", status)
	}

	var stored []string
	rows, err := pool.Query(ctx, `SELECT event_id FROM events ORDER BY event_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stored, ","); got != "a1,a2,b1" {
		t.Fatalf("stored events = %s, want a1,a2,b1 (a3 was refused)", got)
	}
}
