// store-metrics: daily collector of App Store + Google Play metrics into Postgres (ANALYTICS_PLAN.md).
//
// Runs once and exits (systemd user timer on JODA). Configuration via env (see deploy/env.example)
// and apps.yaml. Exit code 1 when any source failed; partial data is still committed.
// Subcommands `telemetry …` and `report …` manage and read usage telemetry (see telemetry.go).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/lioilsources/store-metrics/internal/apple"
	"github.com/lioilsources/store-metrics/internal/db"
	"github.com/lioilsources/store-metrics/internal/export"
	"github.com/lioilsources/store-metrics/internal/google"
)

type App struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	AppleSKUs   []string `yaml:"apple_skus"`
	PlayPackage string   `yaml:"play_package"`
}

type Config struct {
	Apps []App `yaml:"apps"`
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	if code, ok := subcommand(os.Args[1:]); ok {
		os.Exit(code)
	}
	cfgPath := flag.String("config", env("APPS_CONFIG", "apps.yaml"), "apps.yaml")
	days := flag.Int("apple-days", 45, "how many past days of Apple Sales to make sure are loaded")
	only := flag.String("only", "", "apple | google (default both)")
	flag.Parse()
	log.SetFlags(0)

	b, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		log.Fatal(err)
	}
	skuToApp, pkgToApp := map[string]string{}, map[string]string{}
	var allApps []string
	for _, a := range cfg.Apps {
		allApps = append(allApps, a.ID)
		for _, s := range a.AppleSKUs {
			skuToApp[strings.ToLower(s)] = a.ID
		}
		if a.PlayPackage != "" {
			pkgToApp[strings.ToLower(a.PlayPackage)] = a.ID
		}
	}

	ctx := context.Background()
	d, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal("db: ", err)
	}
	defer d.Pool.Close()
	raw := env("RAW_DIR", "raw")
	var failed []string

	if *only == "" || *only == "apple" {
		if err := runApple(ctx, d, raw, *days, allApps, func(s string) string { return skuToApp[strings.ToLower(s)] }); err != nil {
			failed = append(failed, "apple: "+err.Error())
		}
	}
	if *only == "" || *only == "google" {
		if err := runGoogle(ctx, d, raw, pkgToApp); err != nil {
			failed = append(failed, "google: "+err.Error())
		}
	}
	if p := os.Getenv("EXPORT_CSV"); p != "" {
		n, err := export.CSV(ctx, d.Pool, p, time.Now().AddDate(0, -6, 0))
		if err != nil {
			failed = append(failed, "export: "+err.Error())
		} else {
			log.Printf("export: %d rows -> %s", n, p)
		}
	}
	if len(failed) > 0 {
		for _, f := range failed {
			log.Print("FAIL ", f)
		}
		os.Exit(1)
	}
	log.Print("ok")
}

func runApple(ctx context.Context, d *db.DB, raw string, days int, allApps []string, appOf func(string) string) error {
	c, err := apple.New(os.Getenv("ASC_KEY_ID"), os.Getenv("ASC_ISSUER_ID"), os.Getenv("ASC_KEY_PATH"), os.Getenv("ASC_VENDOR"))
	if err != nil {
		return err
	}
	dir := filepath.Join(raw, "apple")
	_ = os.MkdirAll(dir, 0o755)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	var errs []error
	loaded := 0
	for i := days; i >= 1; i-- {
		day := today.AddDate(0, 0, -i)
		if ok, _ := d.HaveDay(ctx, "apple", "sales", day); ok {
			continue
		}
		tsv, err := c.SalesTSV(day)
		if errors.Is(err, apple.ErrNotYet) {
			continue // Apple publishes with a ~1 day delay; retried next run
		}
		if err != nil {
			d.LogRun(ctx, "apple", "sales", &day, false, err.Error(), 0)
			errs = append(errs, err)
			continue
		}
		_ = os.WriteFile(filepath.Join(dir, "sales_"+day.Format("2006-01-02")+".tsv"), tsv, 0o644)
		rows, unknown, err := apple.Parse(tsv, appOf)
		if err == nil {
			err = d.ReplaceSlice(ctx, "apple", "sales", day, day, allApps, rows)
		}
		msg := ""
		if len(unknown) > 0 {
			msg = "unknown SKU: " + strings.Join(unknown, ", ")
		}
		if err != nil {
			d.LogRun(ctx, "apple", "sales", &day, false, err.Error(), 0)
			errs = append(errs, err)
			continue
		}
		d.LogRun(ctx, "apple", "sales", &day, true, msg, len(rows))
		loaded++
	}
	log.Printf("apple: loaded %d new days", loaded)
	return errors.Join(errs...)
}

func runGoogle(ctx context.Context, d *db.DB, raw string, pkgToApp map[string]string) error {
	c, err := google.New(os.Getenv("GOOGLE_SA_JSON"), os.Getenv("GCS_BUCKET"))
	if err != nil {
		return err
	}
	// First successful run backfills everything the bucket has; later runs reload the last two months
	// (monthly files are rewritten during the month).
	var everOK bool
	_ = d.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM collector_runs WHERE store='google' AND ok)`).Scan(&everOK)
	cutoff := time.Time{}
	if everOK {
		n := time.Now().UTC()
		cutoff = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0)
	}
	var errs []error
	files := 0
	for _, prefix := range []string{"stats/installs/", "stats/store_performance/"} {
		objs, err := c.List(prefix)
		if err != nil {
			d.LogRun(ctx, "google", strings.Trim(prefix, "/"), nil, false, err.Error(), 0)
			return err // most likely permissions; no point continuing
		}
		for _, o := range objs {
			kind, pkg, month := google.Classify(o.Name)
			app := pkgToApp[strings.ToLower(pkg)]
			if kind == "" || app == "" || month.Before(cutoff) {
				continue
			}
			body, err := c.Download(o.Name)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			dst := filepath.Join(raw, "google", o.Name)
			_ = os.MkdirAll(filepath.Dir(dst), 0o755)
			_ = os.WriteFile(dst, body, 0o644)
			rows, err := google.Parse(kind, app, body)
			if err == nil {
				end := month.AddDate(0, 1, -1)
				err = d.ReplaceSlice(ctx, "google", kind, month, end, []string{app}, rows)
			}
			if err != nil {
				d.LogRun(ctx, "google", kind, &month, false, fmt.Sprintf("%s: %v", o.Name, err), 0)
				errs = append(errs, fmt.Errorf("%s: %w", o.Name, err))
				continue
			}
			d.LogRun(ctx, "google", kind, &month, true, o.Name, len(rows))
			files++
		}
	}
	log.Printf("google: loaded %d files", files)
	return errors.Join(errs...)
}
