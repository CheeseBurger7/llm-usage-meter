-- name: RollupWindow :execrows
-- Recomputes every (org, tenant, provider, model, feature, hour) bucket in
-- [from, to) from the raw events. One statement covers all orgs; org_id is part
-- of the grouping, so no bucket mixes two orgs.
INSERT INTO hourly_usage (org_id, tenant_id, provider, model, feature, hour_start,
                          event_count, input_tokens, output_tokens,
                          cache_read_tokens, cache_write_tokens,
                          latency_ms_sum, latency_ms_max, source_ingested_at)
SELECT org_id, tenant_id, provider, model, feature,
       date_trunc('hour', event_time AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
       COUNT(*), SUM(input_tokens), SUM(output_tokens),
       SUM(cache_read_tokens), SUM(cache_write_tokens),
       SUM(latency_ms), MAX(latency_ms), MAX(ingested_at)
FROM events
WHERE event_time >= @window_start AND event_time < @window_end
GROUP BY 1, 2, 3, 4, 5, 6
ON CONFLICT (org_id, tenant_id, provider, model, feature, hour_start) DO UPDATE SET
    event_count        = EXCLUDED.event_count,
    input_tokens       = EXCLUDED.input_tokens,
    output_tokens      = EXCLUDED.output_tokens,
    cache_read_tokens  = EXCLUDED.cache_read_tokens,
    cache_write_tokens = EXCLUDED.cache_write_tokens,
    latency_ms_sum     = EXCLUDED.latency_ms_sum,
    latency_ms_max     = EXCLUDED.latency_ms_max,
    source_ingested_at = EXCLUDED.source_ingested_at,
    updated_at         = now();

-- name: ListStaleHours :many
-- Hours in which some bucket has events but no rollup row, or a row older than
-- its newest event. An hour is listed once, whichever org made it stale.
SELECT DISTINCT e.hour_start::timestamptz AS hour_start
FROM (
    SELECT org_id, tenant_id, provider, model, feature,
           date_trunc('hour', event_time AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS hour_start,
           MAX(ingested_at) AS max_ingested_at
    FROM events
    GROUP BY 1, 2, 3, 4, 5, 6
) e
LEFT JOIN hourly_usage h
       ON h.org_id = e.org_id AND h.tenant_id = e.tenant_id AND h.provider = e.provider
      AND h.model = e.model AND h.feature = e.feature AND h.hour_start = e.hour_start
WHERE h.org_id IS NULL OR h.source_ingested_at < e.max_ingested_at
ORDER BY 1;
