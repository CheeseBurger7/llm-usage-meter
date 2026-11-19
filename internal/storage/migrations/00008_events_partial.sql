-- +goose Up
-- partial marks an event recorded from a call that ended before its final
-- usage report, such as a stream the caller hung up on (Q19). Its counts can
-- only be too low, never too high. Existing events were all complete, so they
-- get false. An event cannot be marked partial afterwards.
ALTER TABLE events ADD COLUMN partial BOOLEAN NOT NULL DEFAULT false;

-- partial_count is how many of a row's events are partial, so statements can
-- show it next to each line. Existing rows count only complete events, so 0 is
-- exact for them and no rebuild is needed.
ALTER TABLE hourly_usage ADD COLUMN partial_count BIGINT NOT NULL DEFAULT 0;
ALTER TABLE hourly_usage ADD CONSTRAINT hourly_usage_partial_count_range
    CHECK (partial_count >= 0 AND partial_count <= event_count);

-- +goose Down
ALTER TABLE hourly_usage DROP CONSTRAINT hourly_usage_partial_count_range;
ALTER TABLE hourly_usage DROP COLUMN partial_count;
ALTER TABLE events DROP COLUMN partial;
