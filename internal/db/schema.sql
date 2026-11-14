-- store-metrics schema (ANALYTICS_PLAN.md §4). Idempotent: runs at every collector start.
CREATE TABLE IF NOT EXISTS metrics_daily (
  date            date        NOT NULL,
  store           text        NOT NULL,             -- apple | google
  kind            text        NOT NULL,             -- report family: sales | installs | store_performance | traffic | earnings
  app_id          text        NOT NULL,             -- internal id from apps.yaml
  country         text        NOT NULL DEFAULT '',  -- ISO 3166 alpha-2
  device          text        NOT NULL DEFAULT '',
  source          text        NOT NULL DEFAULT '',  -- search | browse | web | app_referrer | qr | campaign | other
  campaign        text        NOT NULL DEFAULT '',
  downloads       integer     NOT NULL DEFAULT 0,   -- first-time downloads / new user installs
  redownloads     integer     NOT NULL DEFAULT 0,
  updates         integer     NOT NULL DEFAULT 0,
  uninstalls      integer     NOT NULL DEFAULT 0,
  active_devices  integer     NOT NULL DEFAULT 0,
  impressions     integer     NOT NULL DEFAULT 0,
  page_views      integer     NOT NULL DEFAULT 0,   -- product page / store listing visitors
  units           integer     NOT NULL DEFAULT 0,   -- paid units (IAP, sales)
  proceeds        numeric(14,4) NOT NULL DEFAULT 0, -- developer proceeds in proceeds_ccy
  proceeds_ccy    text        NOT NULL DEFAULT '',
  PRIMARY KEY (date, store, kind, app_id, country, device, source, campaign, proceeds_ccy)
);
CREATE INDEX IF NOT EXISTS metrics_daily_app_date ON metrics_daily (app_id, date);

CREATE TABLE IF NOT EXISTS collector_runs (
  id       bigserial PRIMARY KEY,
  at       timestamptz NOT NULL DEFAULT now(),
  store    text NOT NULL,
  kind     text NOT NULL,
  day      date,
  ok       boolean NOT NULL,
  message  text NOT NULL DEFAULT '',
  rows     integer NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS collector_runs_lookup ON collector_runs (store, kind, day) WHERE ok;

-- QR attribution (§5); filled later by the olin.now/q shortlink and first-open pings.
CREATE TABLE IF NOT EXISTS qr_scans   (date date NOT NULL, campaign text NOT NULL, os text NOT NULL DEFAULT '', country text NOT NULL DEFAULT '', scans integer NOT NULL DEFAULT 0, PRIMARY KEY (date, campaign, os, country));
CREATE TABLE IF NOT EXISTS first_open (date date NOT NULL, app_id text NOT NULL, platform text NOT NULL, campaign text NOT NULL DEFAULT '', count integer NOT NULL DEFAULT 0, PRIMARY KEY (date, app_id, platform, campaign));
CREATE TABLE IF NOT EXISTS qr_placements (place text PRIMARY KEY, placed_on date, print_cost_czk numeric(10,2));
