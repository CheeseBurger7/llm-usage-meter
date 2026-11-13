package auth_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/storage"
)

// newService starts a throwaway Postgres, applies migrations and returns a
// Service plus the pool for direct assertions. The test is skipped when
// Docker is not available.
func newService(t *testing.T) (*auth.Service, *pgxpool.Pool) {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("meter"),
		postgres.WithUsername("meter"),
		postgres.WithPassword("meter"),
		// Postgres restarts once during init; this waits for the second "ready"
		// log line and the port, so the first connection cannot hit the restart.
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return auth.New(pool), pool
}

func TestKeyLifecycle(t *testing.T) {
	svc, pool := newService(t)
	ctx := context.Background()

	acme, err := svc.CreateOrg(ctx, "  Acme ")
	if err != nil {
		t.Fatal(err)
	}
	if acme.Name != "Acme" || acme.CreatedAt.IsZero() {
		t.Fatalf("org = %+v", acme)
	}
	globex, err := svc.CreateOrg(ctx, "Globex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateOrg(ctx, "   "); err == nil {
		t.Fatal("blank org name was accepted")
	}

	ingest, err := svc.CreateKey(ctx, acme.ID, auth.ScopeIngest)
	if err != nil {
		t.Fatal(err)
	}
	read, err := svc.CreateKey(ctx, acme.ID, auth.ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.CreateKey(ctx, globex.ID, auth.ScopeIngest)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("verify returns org and scope", func(t *testing.T) {
		for _, tc := range []struct {
			key  auth.NewKey
			want auth.Identity
		}{
			{ingest, auth.Identity{OrgID: acme.ID, Scope: auth.ScopeIngest}},
			{read, auth.Identity{OrgID: acme.ID, Scope: auth.ScopeRead}},
			{other, auth.Identity{OrgID: globex.ID, Scope: auth.ScopeIngest}},
		} {
			got, err := svc.Verify(ctx, tc.key.Plaintext)
			if err != nil || got.OrgID != tc.want.OrgID || got.Scope != tc.want.Scope {
				t.Fatalf("Verify = %+v, %v; want %+v", got, err, tc.want)
			}
		}
	})

	t.Run("verify returns a stable id per key", func(t *testing.T) {
		seen := map[auth.KeyID]string{}
		for name, key := range map[string]auth.NewKey{"ingest": ingest, "read": read, "other": other} {
			first, err := svc.Verify(ctx, key.Plaintext)
			if err != nil {
				t.Fatal(err)
			}
			second, err := svc.Verify(ctx, key.Plaintext)
			if err != nil {
				t.Fatal(err)
			}
			if first.KeyID == (auth.KeyID{}) || first.KeyID != second.KeyID {
				t.Fatalf("%s: key ids %s and %s; want the same non-zero id", name, first.KeyID, second.KeyID)
			}
			// Two keys of one org must not share a rate limit bucket.
			if prev, dup := seen[first.KeyID]; dup {
				t.Fatalf("%s and %s share key id %s", name, prev, first.KeyID)
			}
			seen[first.KeyID] = name
		}
	})

	t.Run("only the hash and prefix are stored", func(t *testing.T) {
		var hash []byte
		var prefix, scope string
		err := pool.QueryRow(ctx,
			`SELECT key_hash, prefix, scope FROM api_keys WHERE prefix = $1`, ingest.Prefix,
		).Scan(&hash, &prefix, &scope)
		if err != nil {
			t.Fatal(err)
		}
		want := sha256.Sum256([]byte(ingest.Plaintext))
		if string(hash) != string(want[:]) {
			t.Fatal("stored hash is not SHA-256 of the plaintext key")
		}
		if len(prefix) != auth.PrefixLen || !strings.HasPrefix(ingest.Plaintext, "lum_ingest_"+prefix) {
			t.Fatalf("prefix %q is not the first %d characters of the secret", prefix, auth.PrefixLen)
		}
		if scope != "ingest" {
			t.Fatalf("scope = %q", scope)
		}
		// No column of any row holds the plaintext.
		var leaked int
		err = pool.QueryRow(ctx,
			`SELECT count(*) FROM api_keys k WHERE position($1 in k::text) > 0`, ingest.Plaintext,
		).Scan(&leaked)
		if err != nil || leaked != 0 {
			t.Fatalf("plaintext found in %d rows (err %v)", leaked, err)
		}
	})

	t.Run("unknown and malformed keys", func(t *testing.T) {
		unknown := "lum_ingest_" + strings.Repeat("0", 64)
		// A real secret under a different scope label is a different key.
		relabelled := strings.Replace(ingest.Plaintext, "lum_ingest_", "lum_read_", 1)
		for _, key := range []string{unknown, relabelled, "", "nonsense", ingest.Plaintext + "0"} {
			if _, err := svc.Verify(ctx, key); !errors.Is(err, auth.ErrInvalidKey) {
				t.Fatalf("Verify(len %d): err = %v; want ErrInvalidKey", len(key), err)
			}
		}
	})

	t.Run("key for a missing org", func(t *testing.T) {
		missing := acme.ID
		missing[0] ^= 0xff
		if _, err := svc.CreateKey(ctx, missing, auth.ScopeRead); !errors.Is(err, auth.ErrOrgNotFound) {
			t.Fatalf("err = %v; want ErrOrgNotFound", err)
		}
		if _, err := svc.CreateKey(ctx, acme.ID, auth.Scope("admin")); err == nil {
			t.Fatal("unknown scope was accepted")
		}
	})

	t.Run("revoke", func(t *testing.T) {
		if _, err := svc.RevokeByPrefix(ctx, "short"); err == nil {
			t.Fatal("short prefix was accepted")
		}
		n, err := svc.RevokeByPrefix(ctx, ingest.Prefix)
		if err != nil || n != 1 {
			t.Fatalf("revoke = %d, %v; want 1", n, err)
		}
		if _, err := svc.Verify(ctx, ingest.Plaintext); !errors.Is(err, auth.ErrRevokedKey) {
			t.Fatalf("revoked key: err = %v; want ErrRevokedKey", err)
		}
		// Re-running revokes nothing more, and other keys still work.
		n, err = svc.RevokeByPrefix(ctx, ingest.Prefix)
		if err != nil || n != 0 {
			t.Fatalf("second revoke = %d, %v; want 0", n, err)
		}
		if _, err := svc.Verify(ctx, read.Plaintext); err != nil {
			t.Fatalf("acme read key stopped working: %v", err)
		}
		if _, err := svc.Verify(ctx, other.Plaintext); err != nil {
			t.Fatalf("globex key stopped working: %v", err)
		}
	})
}
