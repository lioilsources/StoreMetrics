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
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/lioilsources/store-metrics/internal/apple"
	"github.com/lioilsources/store-metrics/internal/cloudflare"
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
	only := flag.String("only", "", "apple | google | web (default all)")
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
	if (*only == "" || *only == "web") && os.Getenv("CF_API_TOKEN") != "" {
		if err := runWeb(ctx, d); err != nil {
			failed = append(failed, "web: "+err.Error())
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

	// Monthly sales reports (all apps in one zip). The raw zip is not kept: it holds buyer city and postal code.
	var playApps []string
	for _, a := range pkgToApp {
		if !slices.Contains(playApps, a) {
			playApps = append(playApps, a)
		}
	}
	objs, err := c.List("sales/")
	if err != nil {
		d.LogRun(ctx, "google", "sales", nil, false, err.Error(), 0)
		return errors.Join(append(errs, err)...)
	}
	months := 0
	for _, o := range objs {
		month, ok := google.SalesMonth(o.Name)
		if !ok || month.Before(cutoff) {
			continue
		}
		body, err := c.Download(o.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		rows, unknown, err := google.ParseSales(body, func(p string) string { return pkgToApp[strings.ToLower(p)] })
		if err == nil {
			err = d.ReplaceSlice(ctx, "google", "sales", month, month.AddDate(0, 1, -1), playApps, rows)
		}
		if err != nil {
			d.LogRun(ctx, "google", "sales", &month, false, fmt.Sprintf("%s: %v", o.Name, err), 0)
			errs = append(errs, fmt.Errorf("%s: %w", o.Name, err))
			continue
		}
		msg := o.Name
		if len(unknown) > 0 {
			msg += "; unknown package: " + strings.Join(unknown, ", ")
		}
		d.LogRun(ctx, "google", "sales", &month, true, msg, len(rows))
		months++
	}
	log.Printf("google: loaded %d sales months", months)
	return errors.Join(errs...)
}

// runWeb reloads the last 30 days of olin.now metrics from Cloudflare (zone traffic always, Web Analytics when
// CF_ACCOUNT_ID and CF_SITE_TAG are set). Recent days are still being filled in, so whole windows are replaced.
func runWeb(ctx context.Context, d *db.DB) error {
	c, err := cloudflare.New(os.Getenv("CF_API_TOKEN"))
	if err != nil {
		return err
	}
	app := env("CF_SITE_APP", "olin.now")
	apps := []string{app}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	from, to := today.AddDate(0, 0, -30), today.AddDate(0, 0, -1)
	var errs []error

	if zoneID := os.Getenv("CF_ZONE_ID"); zoneID != "" {
		zone, country, err := c.Zone(zoneID, app, from, to)
		if err == nil {
			err = d.ReplaceSlice(ctx, "web", cloudflare.KindZone, from, to, apps, zone)
		}
		if err == nil {
			err = d.ReplaceSlice(ctx, "web", cloudflare.KindZoneCountry, from, to, apps, country)
		}
		d.LogRun(ctx, "web", cloudflare.KindZone, &to, err == nil, errMsg(err), len(zone))
		if err != nil {
			errs = append(errs, err)
		} else {
			log.Printf("web: zone %d days", len(zone))
		}
	}

	if acct, site := os.Getenv("CF_ACCOUNT_ID"), os.Getenv("CF_SITE_TAG"); acct != "" && site != "" {
		n := 0
		// Adaptive datasets limit the queried range; a week per call stays well inside it.
		for start := from; !start.After(to); start = start.AddDate(0, 0, 7) {
			end := start.AddDate(0, 0, 6)
			if end.After(to) {
				end = to
			}
			rows, err := c.Visits(acct, site, app, start, end)
			if err == nil {
				err = d.ReplaceSlice(ctx, "web", cloudflare.KindVisits, start, end, apps, rows)
			}
			d.LogRun(ctx, "web", cloudflare.KindVisits, &end, err == nil, errMsg(err), len(rows))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			n += len(rows)
		}
		log.Printf("web: visits %d rows", n)
	}
	return errors.Join(errs...)
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
