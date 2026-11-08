-- +goose Up
-- Hourly rollup of events per tenant and model. Rows are a pure function of
-- the events table: they are recomputed, never incremented, so re-running a
-- window is safe.
CREATE TABLE hourly_usage (
    tenant_id        TEXT        NOT NULL,
    model            TEXT        NOT NULL,
    hour_start       TIMESTAMPTZ NOT NULL,
    event_count      BIGINT      NOT NULL,
    input_tokens     BIGINT      NOT NULL,
    output_tokens    BIGINT      NOT NULL,
    latency_ms_sum   BIGINT      NOT NULL,
    latency_ms_max   BIGINT      NOT NULL,
    -- Newest ingested_at among the events counted; a later event makes the row stale.
    source_ingested_at TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, model, hour_start)
);

-- Backfill scans events by event_time alone.
CREATE INDEX events_time_idx ON events (event_time);

-- +goose Down
DROP INDEX events_time_idx;
DROP TABLE hourly_usage;
