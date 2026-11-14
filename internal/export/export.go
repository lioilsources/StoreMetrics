// Package export writes metrics_daily as one CSV for the dashboard.
package export

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var cols = []string{"date", "store", "kind", "app_id", "country", "device", "source", "campaign", "downloads", "redownloads",
	"updates", "uninstalls", "active_devices", "impressions", "page_views", "units", "proceeds", "proceeds_ccy"}

// CSV writes every row since `since` to path atomically (tmp file + rename).
func CSV(ctx context.Context, pool *pgxpool.Pool, path string, since time.Time) (int, error) {
	rows, err := pool.Query(ctx, `SELECT date::text,store,kind,app_id,country,device,source,campaign,downloads,redownloads,updates,uninstalls,
active_devices,impressions,page_views,units,proceeds::text,proceeds_ccy FROM metrics_daily WHERE date >= $1 ORDER BY date,store,kind,app_id,country`, since)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	w := csv.NewWriter(f)
	_ = w.Write(cols)
	n := 0
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			f.Close()
			return n, err
		}
		rec := make([]string, len(vals))
		for i, v := range vals {
			rec[i] = fmt.Sprint(v)
		}
		_ = w.Write(rec)
		n++
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return n, err
	}
	if err := f.Close(); err != nil {
		return n, err
	}
	return n, os.Rename(tmp, path)
}
