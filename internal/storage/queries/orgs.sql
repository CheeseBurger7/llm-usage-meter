-- name: CreateOrg :one
INSERT INTO orgs (name)
VALUES ($1)
RETURNING id, name, created_at;

-- name: GetOrg :one
SELECT id, name, created_at
FROM orgs
WHERE id = $1;
