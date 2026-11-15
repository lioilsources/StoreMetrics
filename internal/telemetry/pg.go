package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultRetentionDays applies to apps without a telemetry.apps row (12 months).
const DefaultRetentionDays = 365

// PG is the Postgres Store (schema in internal/db/telemetry.sql).
type PG struct{ Pool *pgxpool.Pool }

func (p PG) Ping(ctx context.Context) error { return p.Pool.Ping(ctx) }

func (p PG) LookupKey(ctx context.Context, keyHash string) (App, bool, error) {
	var a App
	err := p.Pool.QueryRow(ctx, `SELECT id, store_country FROM telemetry.apps WHERE key_hash = $1 AND revoked_at IS NULL`, keyHash).
		Scan(&a.ID, &a.StoreCountry)
	if errors.Is(err, pgx.ErrNoRows) {
		return App{}, false, nil
	}
	if err != nil {
		return App{}, false, fmt.Errorf("lookup key: %w", err)
	}
	return a, true, nil
}

func (p PG) Placements(ctx context.Context) (map[string]bool, error) {
	rows, err := p.Pool.Query(ctx, `SELECT place FROM qr_placements`)
	if err != nil {
		return nil, fmt.Errorf("qr placements: %w", err)
	}
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("qr placements: %w", err)
		}
		out[s] = true
	}
	return out, rows.Err()
}

// Insert stores events in one transaction. Duplicates of (app, session, seq) are skipped; every inserted
// first app_open adds one to public.first_open (ANALYTICS_PLAN §5).
func (p PG) Insert(ctx context.Context, app string, receivedAt time.Time, evs []Event) (int, error) {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	b := &pgx.Batch{}
	for _, e := range evs {
		props, err := json.Marshal(e.Props)
		if err != nil {
			return 0, fmt.Errorf("props: %w", err)
		}
		b.Queue(`INSERT INTO telemetry.events (app, received_at, ts, day, session, seq, name, country, props)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (app, session, seq) DO NOTHING`,
			app, receivedAt, e.Time, e.Day, e.Session, e.Seq, e.Name, e.Country, props)
	}
	res := tx.SendBatch(ctx, b)
	inserted := 0
	fb := &pgx.Batch{}
	for _, e := range evs {
		tag, err := res.Exec()
		if err != nil {
			res.Close()
			return 0, fmt.Errorf("insert event: %w", err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		inserted++
		if platform, campaign, ok := FirstOpen(e); ok {
			fb.Queue(`INSERT INTO first_open (date, app_id, platform, campaign, count) VALUES ($1,$2,$3,$4,1)
ON CONFLICT (date, app_id, platform, campaign) DO UPDATE SET count = first_open.count + 1`, e.Day, app, platform, campaign)
		}
	}
	if err := res.Close(); err != nil {
		return 0, fmt.Errorf("insert events: %w", err)
	}
	if fb.Len() > 0 {
		if err := tx.SendBatch(ctx, fb).Close(); err != nil {
			return 0, fmt.Errorf("first_open: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return inserted, nil
}

func (p PG) AcceptedOn(ctx context.Context, app string, day time.Time) (int, error) {
	var n int
	err := p.Pool.QueryRow(ctx, `SELECT coalesce(sum(events), 0) FROM telemetry.ingest_stats WHERE day = $1 AND app = $2 AND reason = $3`,
		day, app, string(ReasonAccepted)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("accepted on: %w", err)
	}
	return n, nil
}

func (p PG) AddStats(ctx context.Context, rows []StatRow) error {
	b := &pgx.Batch{}
	for _, r := range rows {
		b.Queue(`INSERT INTO telemetry.ingest_stats (day, app, reason, requests, events) VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (day, app, reason) DO UPDATE SET requests = ingest_stats.requests + EXCLUDED.requests, events = ingest_stats.events + EXCLUDED.events`,
			r.Day, r.App, string(r.Reason), r.Requests, r.Events)
	}
	if err := p.Pool.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("ingest stats: %w", err)
	}
	return nil
}

// NewKey creates (or replaces) the app's key and returns it. Only its hash is stored; the old key stops working.
func (p PG) NewKey(ctx context.Context, app string, now time.Time) (key string, replaced bool, err error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("random: %w", err)
	}
	key = "tk1_" + hex.EncodeToString(buf)
	err = p.Pool.QueryRow(ctx, `WITH old AS (SELECT key_hash FROM telemetry.apps WHERE id = $1)
INSERT INTO telemetry.apps (id, key_hash, key_created_at, revoked_at) VALUES ($1, $2, $3, NULL)
ON CONFLICT (id) DO UPDATE SET key_hash = EXCLUDED.key_hash, key_created_at = EXCLUDED.key_created_at, revoked_at = NULL
RETURNING coalesce((SELECT key_hash IS NOT NULL FROM old), false)`, app, HashKey(key), now).Scan(&replaced)
	if err != nil {
		return "", false, fmt.Errorf("store key: %w", err)
	}
	return key, replaced, nil
}

// RevokeKey removes the app's key: every released build of the app stops sending (within the receiver's cache TTL).
func (p PG) RevokeKey(ctx context.Context, app string, now time.Time) (bool, error) {
	tag, err := p.Pool.Exec(ctx, `UPDATE telemetry.apps SET key_hash = NULL, revoked_at = $2 WHERE id = $1 AND key_hash IS NOT NULL`, app, now)
	if err != nil {
		return false, fmt.Errorf("revoke key: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// Prune deletes raw events older than each app's retention and folds them into telemetry.events_daily
// (no session ids), in one statement. Returns the number of deleted events.
func (p PG) Prune(ctx context.Context, now time.Time) (int64, error) {
	var n int64
	err := p.Pool.QueryRow(ctx, `WITH gone AS (
  DELETE FROM telemetry.events e
  WHERE e.ts < $1::timestamptz - make_interval(days => coalesce((SELECT a.retention_days FROM telemetry.apps a WHERE a.id = e.app), $2))
  RETURNING e.day, e.app, e.name, e.country, e.props
), agg AS (
  INSERT INTO telemetry.events_daily (day, app, name, platform, app_version, country, tester, count)
  SELECT day, app, name, coalesce(props->>'platform', ''), coalesce(props->>'app_version', ''), country,
         props @> '{"tester": true}', count(*)
  FROM gone GROUP BY 1, 2, 3, 4, 5, 6, 7
  ON CONFLICT (day, app, name, platform, app_version, country, tester)
  DO UPDATE SET count = events_daily.count + EXCLUDED.count
  RETURNING 1
)
SELECT count(*) FROM gone`, now, DefaultRetentionDays).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("prune: %w", err)
	}
	return n, nil
}

// Query runs a report query and returns column names and values (see Querier).
func (p PG) Query(ctx context.Context, sql string, args ...any) ([]string, [][]any, error) {
	rows, err := p.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var cols []string
	for _, f := range rows.FieldDescriptions() {
		cols = append(cols, f.Name)
	}
	var out [][]any
	for rows.Next() {
		v, err := rows.Values()
		if err != nil {
			return nil, nil, err
		}
		out = append(out, v)
	}
	return cols, out, rows.Err()
}
