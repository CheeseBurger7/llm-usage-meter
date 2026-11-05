-- +goose Up
CREATE TABLE events (
    event_id      TEXT        PRIMARY KEY,
    tenant_id     TEXT        NOT NULL,
    model         TEXT        NOT NULL,
    input_tokens  BIGINT      NOT NULL CHECK (input_tokens >= 0),
    output_tokens BIGINT      NOT NULL CHECK (output_tokens >= 0),
    latency_ms    BIGINT      NOT NULL CHECK (latency_ms >= 0),
    event_time    TIMESTAMPTZ NOT NULL,
    ingested_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Rollups scan by tenant and time window.
CREATE INDEX events_tenant_time_idx ON events (tenant_id, event_time);

-- +goose Down
DROP TABLE events;
