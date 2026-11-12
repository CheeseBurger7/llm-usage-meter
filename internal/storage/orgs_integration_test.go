package storage_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/CheeseBurger7/llm-usage-meter/internal/storage"
	"github.com/CheeseBurger7/llm-usage-meter/internal/storage/gen"
)

// Postgres error codes asserted below.
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	checkViolation      = "23514"
)

// newQueries starts a throwaway Postgres, applies migrations and returns the
// generated queries. The test is skipped when Docker is not available.
func newQueries(t *testing.T) *gen.Queries {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("meter"),
		postgres.WithUsername("meter"),
		postgres.WithPassword("meter"),
		postgres.BasicWaitStrategies(),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
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
	return gen.New(pool)
}

// hashOf stands in for the SHA-256 of a plaintext key.
func hashOf(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func wantPgCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("err = %v; want Postgres error %s", err, code)
	}
}

func TestOrgsAndAPIKeys(t *testing.T) {
	q := newQueries(t)
	ctx := context.Background()

	acme, err := q.CreateOrg(ctx, "Acme")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if !acme.ID.Valid || acme.Name != "Acme" || !acme.CreatedAt.Valid {
		t.Fatalf("created org = %+v", acme)
	}
	globex, err := q.CreateOrg(ctx, "Globex")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if acme.ID == globex.ID {
		t.Fatal("two orgs got the same id")
	}
	if got, err := q.GetOrg(ctx, acme.ID); err != nil || got != acme {
		t.Fatalf("GetOrg = %+v, %v; want %+v", got, err, acme)
	}

	newKey := func(org gen.Org, secret, prefix, scope string) gen.ApiKey {
		t.Helper()
		k, err := q.CreateAPIKey(ctx, gen.CreateAPIKeyParams{
			OrgID: org.ID, KeyHash: hashOf(secret), Prefix: prefix, Scope: scope,
		})
		if err != nil {
			t.Fatalf("create key %s: %v", prefix, err)
		}
		return k
	}
	ingestKey := newKey(acme, "acme-ingest", "aaaaaaaa", "ingest")
	readKey := newKey(acme, "acme-read", "bbbbbbbb", "read")
	otherKey := newKey(globex, "globex-ingest", "cccccccc", "ingest")

	t.Run("lookup by hash", func(t *testing.T) {
		got, err := q.GetAPIKeyByHash(ctx, hashOf("acme-ingest"))
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != ingestKey.ID || got.OrgID != acme.ID || got.Scope != "ingest" ||
			got.Prefix != "aaaaaaaa" || got.RevokedAt.Valid {
			t.Fatalf("key = %+v", got)
		}
		if _, err := q.GetAPIKeyByHash(ctx, hashOf("nobody")); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unknown hash: err = %v; want pgx.ErrNoRows", err)
		}
	})

	t.Run("list is scoped to the org", func(t *testing.T) {
		keys, err := q.ListAPIKeysByOrg(ctx, acme.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 2 || keys[0].ID != ingestKey.ID || keys[1].ID != readKey.ID {
			t.Fatalf("acme keys = %+v", keys)
		}
		keys, err = q.ListAPIKeysByOrg(ctx, globex.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 1 || keys[0].ID != otherKey.ID {
			t.Fatalf("globex keys = %+v", keys)
		}
	})

	t.Run("constraints", func(t *testing.T) {
		// The same key hash cannot be stored twice, even under another org.
		_, err := q.CreateAPIKey(ctx, gen.CreateAPIKeyParams{
			OrgID: globex.ID, KeyHash: hashOf("acme-ingest"), Prefix: "dddddddd", Scope: "ingest",
		})
		wantPgCode(t, err, uniqueViolation)

		_, err = q.CreateAPIKey(ctx, gen.CreateAPIKeyParams{
			OrgID: acme.ID, KeyHash: hashOf("admin"), Prefix: "eeeeeeee", Scope: "admin",
		})
		wantPgCode(t, err, checkViolation)

		// Anything that is not 32 bytes is not a SHA-256 hash.
		_, err = q.CreateAPIKey(ctx, gen.CreateAPIKeyParams{
			OrgID: acme.ID, KeyHash: []byte("plaintext"), Prefix: "ffffffff", Scope: "read",
		})
		wantPgCode(t, err, checkViolation)

		missing := acme.ID
		missing.Bytes[0] ^= 0xff
		_, err = q.CreateAPIKey(ctx, gen.CreateAPIKeyParams{
			OrgID: missing, KeyHash: hashOf("orphan"), Prefix: "gggggggg", Scope: "read",
		})
		wantPgCode(t, err, foreignKeyViolation)

		_, err = q.CreateOrg(ctx, "")
		wantPgCode(t, err, checkViolation)
	})

	t.Run("revoke by prefix", func(t *testing.T) {
		if n, err := q.RevokeAPIKeysByPrefix(ctx, "aaaaaaaa"); err != nil || n != 1 {
			t.Fatalf("revoke = %d, %v; want 1", n, err)
		}
		revoked, err := q.GetAPIKeyByHash(ctx, hashOf("acme-ingest"))
		if err != nil {
			t.Fatal(err)
		}
		if !revoked.RevokedAt.Valid {
			t.Fatal("revoked_at not set")
		}

		// Re-running revokes nothing and keeps the original timestamp.
		if n, err := q.RevokeAPIKeysByPrefix(ctx, "aaaaaaaa"); err != nil || n != 0 {
			t.Fatalf("second revoke = %d, %v; want 0", n, err)
		}
		again, err := q.GetAPIKeyByHash(ctx, hashOf("acme-ingest"))
		if err != nil {
			t.Fatal(err)
		}
		if again.RevokedAt != revoked.RevokedAt {
			t.Fatalf("revoked_at moved from %v to %v", revoked.RevokedAt, again.RevokedAt)
		}

		if n, err := q.RevokeAPIKeysByPrefix(ctx, "zzzzzzzz"); err != nil || n != 0 {
			t.Fatalf("unknown prefix revoke = %d, %v; want 0", n, err)
		}

		// Other keys are untouched.
		for _, secret := range []string{"acme-read", "globex-ingest"} {
			k, err := q.GetAPIKeyByHash(ctx, hashOf(secret))
			if err != nil {
				t.Fatal(err)
			}
			if k.RevokedAt.Valid {
				t.Fatalf("key %s was revoked", k.Prefix)
			}
		}
	})
}
