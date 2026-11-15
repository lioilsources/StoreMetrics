-- Usage telemetry, protocol v1 (PLAN-analytika §5.2). Idempotent like schema.sql: runs at every start,
-- after schema.sql, from both store-metrics and telemetry-ingest.
-- Once deployed, a statement here is never edited; changes are appended as new idempotent statements.
-- No IP, no persistent identifier, no free text: `session` is a random per-session id that lives only in app memory.
CREATE SCHEMA IF NOT EXISTS telemetry;

-- One row per app that may send. Only the SHA-256 of the key is stored; NULL key_hash = revoked / no key.
CREATE TABLE IF NOT EXISTS telemetry.apps (
  id              text        PRIMARY KEY,               -- app id from apps.yaml
  key_hash        text        UNIQUE,                    -- hex sha256 of the X-Telemetry-Key value
  store_country   boolean     NOT NULL DEFAULT true,     -- keep CF-IPCountry (false for a kids mode)
  retention_days  integer     NOT NULL DEFAULT 365 CHECK (retention_days > 0),
  key_created_at  timestamptz,
  revoked_at      timestamptz
);

-- Raw events, deleted after the app's retention (telemetry prune).
CREATE TABLE IF NOT EXISTS telemetry.events (
  app          text        NOT NULL,
  received_at  timestamptz NOT NULL,
  ts           timestamptz NOT NULL,           -- event time; future > 10 min replaced by received_at
  day          date        NOT NULL,           -- UTC day of ts
  session      text        NOT NULL,           -- 32 hex, random per session
  seq          integer,                        -- `i`, order in session; NULL = no duplicate check
  name         text        NOT NULL,
  country      text        NOT NULL DEFAULT '', -- ISO alpha-2 from CF-IPCountry, '' when not kept
  props        jsonb       NOT NULL DEFAULT '{}'
);
-- NULL seq never conflicts, so events without `i` are accepted without a duplicate check.
CREATE UNIQUE INDEX IF NOT EXISTS events_dedup ON telemetry.events (app, session, seq);
CREATE INDEX IF NOT EXISTS events_app_ts ON telemetry.events (app, ts);

-- Daily request/event counts by outcome. reason = accepted | duplicate | clamped_time | <rejection reason>.
CREATE TABLE IF NOT EXISTS telemetry.ingest_stats (
  day       date    NOT NULL,
  app       text    NOT NULL DEFAULT '',  -- '' when the key was not recognised
  reason    text    NOT NULL,
  requests  integer NOT NULL DEFAULT 0,
  events    integer NOT NULL DEFAULT 0,
  PRIMARY KEY (day, app, reason)
);

-- Daily aggregates of pruned events, without session ids. Kept forever.
CREATE TABLE IF NOT EXISTS telemetry.events_daily (
  day          date    NOT NULL,
  app          text    NOT NULL,
  name         text    NOT NULL,
  platform     text    NOT NULL DEFAULT '',
  app_version  text    NOT NULL DEFAULT '',
  country      text    NOT NULL DEFAULT '',
  tester       boolean NOT NULL DEFAULT false,
  count        integer NOT NULL DEFAULT 0,
  PRIMARY KEY (day, app, name, platform, app_version, country, tester)
);
