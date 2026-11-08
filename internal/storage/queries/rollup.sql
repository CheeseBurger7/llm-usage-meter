-- name: RollupWindow :execrows
-- Recomputes every (tenant, model, hour) bucket in [from, to) from the raw events.
INSERT INTO hourly_usage (tenant_id, model, hour_start, event_count, input_tokens, output_tokens,
                          latency_ms_sum, latency_ms_max, source_ingested_at)
SELECT tenant_id, model, date_trunc('hour', event_time AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
       COUNT(*), SUM(input_tokens), SUM(output_tokens), SUM(latency_ms), MAX(latency_ms), MAX(ingested_at)
FROM events
WHERE event_time >= @window_start AND event_time < @window_end
GROUP BY 1, 2, 3
ON CONFLICT (tenant_id, model, hour_start) DO UPDATE SET
    event_count        = EXCLUDED.event_count,
    input_tokens       = EXCLUDED.input_tokens,
    output_tokens      = EXCLUDED.output_tokens,
    latency_ms_sum     = EXCLUDED.latency_ms_sum,
    latency_ms_max     = EXCLUDED.latency_ms_max,
    source_ingested_at = EXCLUDED.source_ingested_at,
    updated_at         = now();

-- name: ListStaleHours :many
-- Hours that have events but no rollup row, or a row older than its newest event.
SELECT DISTINCT e.hour_start::timestamptz AS hour_start
FROM (
    SELECT tenant_id, model,
           date_trunc('hour', event_time AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS hour_start,
           MAX(ingested_at) AS max_ingested_at
    FROM events
    GROUP BY 1, 2, 3
) e
LEFT JOIN hourly_usage h
       ON h.tenant_id = e.tenant_id AND h.model = e.model AND h.hour_start = e.hour_start
WHERE h.tenant_id IS NULL OR h.source_ingested_at < e.max_ingested_at
ORDER BY 1;
