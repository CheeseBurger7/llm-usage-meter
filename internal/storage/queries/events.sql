-- name: InsertEvent :one
-- Returns the event_id when a row was inserted; no row when the id already exists.
INSERT INTO events (event_id, tenant_id, model, input_tokens, output_tokens, latency_ms, event_time)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (event_id) DO NOTHING
RETURNING event_id;

-- name: GetEvent :one
SELECT event_id, tenant_id, model, input_tokens, output_tokens, latency_ms, event_time
FROM events
WHERE event_id = $1;
