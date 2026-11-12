-- +goose Up
-- provider is required on new events. Rows stored before this migration never
-- recorded one, so they are labelled 'unknown' instead of being deleted. The
-- default is dropped afterwards: an insert that omits provider must fail.
ALTER TABLE events ADD COLUMN provider TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE events ALTER COLUMN provider DROP DEFAULT;

-- Cache tokens are counted separately from input_tokens because providers
-- price them at different rates.
ALTER TABLE events ADD COLUMN cache_read_tokens  BIGINT NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0);
ALTER TABLE events ADD COLUMN cache_write_tokens BIGINT NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0);

-- feature is an optional label. '' means "not set": the column becomes part of
-- the hourly_usage primary key, which cannot hold NULL.
ALTER TABLE events ADD COLUMN feature TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE events DROP COLUMN feature;
ALTER TABLE events DROP COLUMN cache_write_tokens;
ALTER TABLE events DROP COLUMN cache_read_tokens;
ALTER TABLE events DROP COLUMN provider;
