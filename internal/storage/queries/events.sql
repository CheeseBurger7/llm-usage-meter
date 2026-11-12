-- name: InsertEvent :one
-- Returns the event_id when a row was inserted; no row when the org already has that id.
INSERT INTO events (org_id, event_id, tenant_id, provider, model, feature, input_tokens, output_tokens,
                    cache_read_tokens, cache_write_tokens, latency_ms, event_time)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (org_id, event_id) DO NOTHING
RETURNING event_id;

-- name: GetEvent :one
SELECT event_id, tenant_id, provider, model, feature, input_tokens, output_tokens,
       cache_read_tokens, cache_write_tokens, latency_ms, event_time
FROM events
WHERE org_id = $1 AND event_id = $2;
