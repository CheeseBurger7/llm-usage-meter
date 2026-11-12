-- +goose Up
-- An org is one paying customer. Everything else is scoped to an org.
CREATE TABLE orgs (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT        NOT NULL CHECK (name <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- API keys are stored as a SHA-256 hash only; the plaintext is never written.
-- prefix is the short, non-secret part shown to humans to tell keys apart.
CREATE TABLE api_keys (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID        NOT NULL REFERENCES orgs (id),
    key_hash   BYTEA       NOT NULL CHECK (octet_length(key_hash) = 32),
    prefix     TEXT        NOT NULL CHECK (prefix <> ''),
    scope      TEXT        NOT NULL CHECK (scope IN ('ingest', 'read')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);

-- Verification looks a key up by its hash on every request.
CREATE UNIQUE INDEX api_keys_key_hash_idx ON api_keys (key_hash);

-- Listing an org's keys, and the foreign key check when an org is deleted.
CREATE INDEX api_keys_org_idx ON api_keys (org_id, created_at);

-- Revocation addresses keys by prefix.
CREATE INDEX api_keys_prefix_idx ON api_keys (prefix);

-- +goose Down
DROP TABLE api_keys;
DROP TABLE orgs;
