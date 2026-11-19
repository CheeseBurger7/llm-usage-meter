-- +goose Up
-- provider must match ^[a-z0-9._-]{1,256}$, the rule ingest applies (Q20), so
-- a row written around ingest cannot hold a label that the price book, SQL and
-- the dashboard would compare differently. The values stored so far pass:
-- 'unknown' from migration 00005, 'anthropic' from the proxy, and lowercase
-- ASCII test data. If a database holds anything else, ADD CONSTRAINT fails and
-- the migration stops without changing data; fix the rows and run it again.
--
-- The length is checked with length() because a Postgres regex bound cannot
-- exceed 255. Every allowed character is one byte, so 256 characters is 256
-- bytes. Ranges in a Postgres regex bracket compare code points, not the
-- collation, so [a-z] is exactly the 26 ASCII letters.
ALTER TABLE events ADD CONSTRAINT events_provider_charset
    CHECK (provider ~ '^[a-z0-9._-]+$' AND length(provider) <= 256);
ALTER TABLE hourly_usage ADD CONSTRAINT hourly_usage_provider_charset
    CHECK (provider ~ '^[a-z0-9._-]+$' AND length(provider) <= 256);

-- +goose Down
ALTER TABLE hourly_usage DROP CONSTRAINT hourly_usage_provider_charset;
ALTER TABLE events DROP CONSTRAINT events_provider_charset;
