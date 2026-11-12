-- +goose Up
-- Events belong to an org. Rows written before this migration have no org and
-- cannot be attributed, so they are removed together with the rollups derived
-- from them. No production data exists yet.
TRUNCATE events, hourly_usage;

ALTER TABLE events ADD COLUMN org_id UUID NOT NULL REFERENCES orgs (id);

-- event_id is unique within an org, not globally: two orgs may use the same id.
ALTER TABLE events DROP CONSTRAINT events_pkey;
ALTER TABLE events ADD PRIMARY KEY (org_id, event_id);

-- Per-org reads by tenant and time window.
DROP INDEX events_tenant_time_idx;
CREATE INDEX events_org_tenant_time_idx ON events (org_id, tenant_id, event_time);

-- events_time_idx (event_time) stays: the rollup scans an hour across all orgs.

-- +goose Down
-- The old primary key cannot hold the same event_id from two orgs.
TRUNCATE events, hourly_usage;

DROP INDEX events_org_tenant_time_idx;
CREATE INDEX events_tenant_time_idx ON events (tenant_id, event_time);

ALTER TABLE events DROP CONSTRAINT events_pkey;
ALTER TABLE events DROP COLUMN org_id;
ALTER TABLE events ADD PRIMARY KEY (event_id);
