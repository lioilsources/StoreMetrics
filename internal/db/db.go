// Package db holds the store-metrics schema and idempotent upserts.
package db

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// telemetrySchema is applied after schema.sql (protocol v1 events, see internal/telemetry).
//
//go:embed telemetry.sql
var telemetrySchema string

// Row is one line of metrics_daily. Empty strings (not NULL) in key columns keep the primary key simple.
type Row struct {
	Date          time.Time
	Store         string // apple | google
	AppID         string
	Country       string
	Device        string
	Source        string
	Campaign      string
	Downloads     int
	Redownloads   int
	Updates       int
	Uninstalls    int
	ActiveDevices int
	Impressions   int
	PageViews     int
	Units         int
	Proceeds      float64
	ProceedsCcy   string
}

type DB struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, url string) (*DB, error) {
	p, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := p.Exec(ctx, schema); err != nil {
		p.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	if _, err := p.Exec(ctx, telemetrySchema); err != nil {
		p.Close()
		return nil, fmt.Errorf("telemetry schema: %w", err)
	}
	return &DB{p}, nil
}

// ReplaceSlice deletes all rows for (store, app, date range, kind) and inserts rows, in one transaction.
// Reports are re-downloaded whole (Play monthly CSVs are rewritten during the month), so replace beats merge.
func (d *DB) ReplaceSlice(ctx context.Context, store, kind string, from, to time.Time, apps []string, rows []Row) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM metrics_daily WHERE store=$1 AND kind=$2 AND date BETWEEN $3 AND $4 AND app_id = ANY($5)`,
		store, kind, from, to, apps); err != nil {
		return err
	}
	b := &pgx.Batch{}
	for _, r := range rows {
		b.Queue(`INSERT INTO metrics_daily (date,store,kind,app_id,country,device,source,campaign,downloads,redownloads,updates,uninstalls,active_devices,impressions,page_views,units,proceeds,proceeds_ccy)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
ON CONFLICT (date,store,kind,app_id,country,device,source,campaign,proceeds_ccy) DO UPDATE SET
 downloads=metrics_daily.downloads+EXCLUDED.downloads, redownloads=metrics_daily.redownloads+EXCLUDED.redownloads,
 updates=metrics_daily.updates+EXCLUDED.updates, uninstalls=metrics_daily.uninstalls+EXCLUDED.uninstalls,
 active_devices=metrics_daily.active_devices+EXCLUDED.active_devices, impressions=metrics_daily.impressions+EXCLUDED.impressions,
 page_views=metrics_daily.page_views+EXCLUDED.page_views, units=metrics_daily.units+EXCLUDED.units, proceeds=metrics_daily.proceeds+EXCLUDED.proceeds`,
			r.Date, r.Store, kind, r.AppID, r.Country, r.Device, r.Source, r.Campaign, r.Downloads, r.Redownloads, r.Updates, r.Uninstalls,
			r.ActiveDevices, r.Impressions, r.PageViews, r.Units, r.Proceeds, r.ProceedsCcy)
	}
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// HaveDay reports whether any row exists for store/kind/date (used to skip Apple days already loaded).
func (d *DB) HaveDay(ctx context.Context, store, kind string, day time.Time) (bool, error) {
	var ok bool
	err := d.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM collector_runs WHERE store=$1 AND kind=$2 AND day=$3 AND ok)`, store, kind, day).Scan(&ok)
	return ok, err
}

func (d *DB) LogRun(ctx context.Context, store, kind string, day *time.Time, ok bool, msg string, rows int) {
	_, _ = d.Pool.Exec(ctx, `INSERT INTO collector_runs (store,kind,day,ok,message,rows) VALUES ($1,$2,$3,$4,$5,$6)`, store, kind, day, ok, msg, rows)
}
