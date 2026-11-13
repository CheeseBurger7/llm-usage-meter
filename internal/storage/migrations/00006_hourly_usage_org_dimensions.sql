-- +goose Up
-- Rollups become per org and gain the provider and feature dimensions and the
-- cache token sums. Existing rows have no org and mix orgs that share a
-- tenant_id and model, so they are removed. Nothing is lost: every row is
-- recomputed from events, and the startup backfill rebuilds them all.
TRUNCATE hourly_usage;

ALTER TABLE hourly_usage ADD COLUMN org_id UUID NOT NULL REFERENCES orgs (id);
ALTER TABLE hourly_usage ADD COLUMN provider TEXT NOT NULL;
-- '' means "no feature", as in events: a primary key column cannot be NULL.
ALTER TABLE hourly_usage ADD COLUMN feature TEXT NOT NULL;
ALTER TABLE hourly_usage ADD COLUMN cache_read_tokens  BIGINT NOT NULL;
ALTER TABLE hourly_usage ADD COLUMN cache_write_tokens BIGINT NOT NULL;

-- org_id leads, so every per-org read is a prefix scan of the primary key.
ALTER TABLE hourly_usage DROP CONSTRAINT hourly_usage_pkey;
ALTER TABLE hourly_usage ADD PRIMARY KEY (org_id, tenant_id, provider, model, feature, hour_start);

-- +goose Down
-- The old key cannot hold two orgs, providers or features in one bucket.
-- Rows are rebuilt from events by the startup backfill.
TRUNCATE hourly_usage;

ALTER TABLE hourly_usage DROP CONSTRAINT hourly_usage_pkey;
ALTER TABLE hourly_usage DROP COLUMN cache_write_tokens;
ALTER TABLE hourly_usage DROP COLUMN cache_read_tokens;
ALTER TABLE hourly_usage DROP COLUMN feature;
ALTER TABLE hourly_usage DROP COLUMN provider;
ALTER TABLE hourly_usage DROP COLUMN org_id;
ALTER TABLE hourly_usage ADD PRIMARY KEY (tenant_id, model, hour_start);
