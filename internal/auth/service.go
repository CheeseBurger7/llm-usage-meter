package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/CheeseBurger7/llm-usage-meter/internal/storage/gen"
)

// ErrOrgNotFound means a key was requested for an org that does not exist.
var ErrOrgNotFound = errors.New("auth: org not found")

// OrgID identifies an org. It is the UUID from the orgs table.
type OrgID [16]byte

// String formats the id as a canonical UUID.
func (id OrgID) String() string {
	return pgtype.UUID{Bytes: id, Valid: true}.String()
}

// ParseOrgID parses a canonical UUID.
func ParseOrgID(s string) (OrgID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil || !u.Valid {
		return OrgID{}, fmt.Errorf("invalid org id %q", s)
	}
	return u.Bytes, nil
}

func (id OrgID) pg() pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// Org is one customer.
type Org struct {
	ID        OrgID
	Name      string
	CreatedAt time.Time
}

// Identity is what a verified key proves about a request.
type Identity struct {
	OrgID OrgID
	Scope Scope
}

// NewKey is the result of creating a key. Plaintext is the only copy of the
// key: it is not stored and cannot be recovered later.
type NewKey struct {
	Plaintext string
	Prefix    string
	OrgID     OrgID
	Scope     Scope
}

// Service creates orgs and issues, verifies and revokes API keys.
type Service struct {
	q *gen.Queries
}

func New(db gen.DBTX) *Service {
	return &Service{q: gen.New(db)}
}

// CreateOrg creates an org with the given display name.
func (s *Service) CreateOrg(ctx context.Context, name string) (Org, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Org{}, errors.New("org name is required")
	}
	o, err := s.q.CreateOrg(ctx, name)
	if err != nil {
		return Org{}, fmt.Errorf("create org: %w", err)
	}
	return Org{ID: o.ID.Bytes, Name: o.Name, CreatedAt: o.CreatedAt.Time}, nil
}

// CreateKey issues a new key for the org. Only its hash and prefix are stored.
func (s *Service) CreateKey(ctx context.Context, org OrgID, scope Scope) (NewKey, error) {
	if _, err := ParseScope(string(scope)); err != nil {
		return NewKey{}, err
	}
	key, err := generateKey(scope)
	if err != nil {
		return NewKey{}, err
	}
	_, prefix, err := parseKey(key)
	if err != nil {
		return NewKey{}, fmt.Errorf("generated key does not parse: %w", err)
	}
	_, err = s.q.CreateAPIKey(ctx, gen.CreateAPIKeyParams{
		OrgID:   org.pg(),
		KeyHash: hashKey(key),
		Prefix:  prefix,
		Scope:   string(scope),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
			return NewKey{}, ErrOrgNotFound
		}
		return NewKey{}, fmt.Errorf("store key: %w", err)
	}
	return NewKey{Plaintext: key, Prefix: prefix, OrgID: org, Scope: scope}, nil
}

// Verify returns the org and scope a plaintext key belongs to. It returns
// ErrInvalidKey for a malformed or unknown key and ErrRevokedKey for a revoked
// one. Callers must not log the key.
func (s *Service) Verify(ctx context.Context, key string) (Identity, error) {
	if _, _, err := parseKey(key); err != nil {
		return Identity{}, err
	}
	row, err := s.q.GetAPIKeyByHash(ctx, hashKey(key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrInvalidKey
	}
	if err != nil {
		return Identity{}, fmt.Errorf("look up key: %w", err)
	}
	if row.RevokedAt.Valid {
		return Identity{}, ErrRevokedKey
	}
	// The scope comes from the stored row, never from the text of the key.
	return Identity{OrgID: row.OrgID.Bytes, Scope: Scope(row.Scope)}, nil
}

// RevokeByPrefix revokes every active key with the prefix and returns how
// many there were. Prefixes are not unique, so the count can exceed one.
func (s *Service) RevokeByPrefix(ctx context.Context, prefix string) (int64, error) {
	if len(prefix) != PrefixLen {
		return 0, fmt.Errorf("prefix must be %d characters", PrefixLen)
	}
	n, err := s.q.RevokeAPIKeysByPrefix(ctx, prefix)
	if err != nil {
		return 0, fmt.Errorf("revoke keys: %w", err)
	}
	return n, nil
}
