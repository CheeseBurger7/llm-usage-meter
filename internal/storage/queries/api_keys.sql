-- name: CreateAPIKey :one
INSERT INTO api_keys (org_id, key_hash, prefix, scope)
VALUES ($1, $2, $3, $4)
RETURNING id, org_id, key_hash, prefix, scope, created_at, revoked_at;

-- name: GetAPIKeyByHash :one
-- Returns revoked keys too, so the caller can tell "revoked" from "unknown".
SELECT id, org_id, key_hash, prefix, scope, created_at, revoked_at
FROM api_keys
WHERE key_hash = $1;

-- name: ListAPIKeysByOrg :many
-- key_hash is left out: listings are for humans and never need it.
SELECT id, org_id, prefix, scope, created_at, revoked_at
FROM api_keys
WHERE org_id = $1
ORDER BY created_at, id;

-- name: RevokeAPIKeysByPrefix :execrows
-- Revokes every active key with this prefix and returns how many. A key that
-- is already revoked keeps its original revoked_at, so re-running is a no-op.
UPDATE api_keys
SET revoked_at = now()
WHERE prefix = $1 AND revoked_at IS NULL;
