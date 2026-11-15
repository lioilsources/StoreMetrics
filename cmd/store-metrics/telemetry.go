package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/lioilsources/store-metrics/internal/db"
	"github.com/lioilsources/store-metrics/internal/telemetry"
)

const telemetryUsage = `usage:
  store-metrics telemetry key new <app>      new key for app (printed once on stdout, only its hash is stored)
  store-metrics telemetry key revoke <app>   revoke the key: released builds stop sending within a minute
  store-metrics telemetry prune              delete raw events past retention (ingest also does it daily)
  store-metrics report <app> [--since 7d]    Markdown answers to PLAN-analytika §1`

// subcommand runs `telemetry …` and `report …`. ok is false when args are not a subcommand (collector run).
func subcommand(args []string) (code int, ok bool) {
	if len(args) == 0 || (args[0] != "telemetry" && args[0] != "report") {
		return 0, false
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := runSubcommand(context.Background(), args, os.Stdout); err != nil {
		logger.Error(args[0], "err", err)
		return 1, true
	}
	return 0, true
}

func runSubcommand(ctx context.Context, args []string, out io.Writer) error {
	now := time.Now()
	switch {
	case args[0] == "report":
		fs := flag.NewFlagSet("report", flag.ContinueOnError)
		since := fs.String("since", "7d", "window: 7d, 36h or 2026-11-01")
		app, rest := splitApp(args[1:])
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if app == "" && fs.NArg() > 0 {
			app = fs.Arg(0)
		}
		if app == "" {
			return fmt.Errorf("missing app\n%s", telemetryUsage)
		}
		from, err := telemetry.ParseSince(*since, now)
		if err != nil {
			return err
		}
		d, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer d.Pool.Close()
		return telemetry.Report(ctx, telemetry.PG{Pool: d.Pool}, out, app, from, now)

	case len(args) == 3 && args[1] == "key" && (args[2] == "new" || args[2] == "revoke"):
		return fmt.Errorf("missing app\n%s", telemetryUsage)

	case len(args) == 4 && args[1] == "key" && args[2] == "new":
		app := args[3]
		if err := knownApp(app); err != nil {
			return err
		}
		d, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer d.Pool.Close()
		key, replaced, err := telemetry.PG{Pool: d.Pool}.NewKey(ctx, app, now)
		if err != nil {
			return err
		}
		if replaced {
			fmt.Fprintf(os.Stderr, "%s: previous key replaced; builds with the old key stop sending within a minute\n", app)
		}
		// No trailing newline when piped, so `| gh secret set TELEMETRY_KEY` gets the bare key.
		if isTerminal(os.Stdout) {
			fmt.Fprintln(out, key)
		} else {
			fmt.Fprint(out, key)
		}
		return nil

	case len(args) == 4 && args[1] == "key" && args[2] == "revoke":
		d, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer d.Pool.Close()
		ok, err := telemetry.PG{Pool: d.Pool}.RevokeKey(ctx, args[3], now)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s: no active key", args[3])
		}
		fmt.Fprintf(os.Stderr, "%s: key revoked\n", args[3])
		return nil

	case len(args) == 2 && args[1] == "prune":
		d, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer d.Pool.Close()
		n, err := telemetry.PG{Pool: d.Pool}.Prune(ctx, now)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "prune: %d raw events deleted (daily aggregates kept)\n", n)
		return nil
	}
	return fmt.Errorf("unknown command\n%s", telemetryUsage)
}

// splitApp lets the app come before flags: `report lexify --since 30d`.
func splitApp(args []string) (string, []string) {
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		return args[0], args[1:]
	}
	return "", args
}

func openDB(ctx context.Context) (*db.DB, error) {
	d, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}
	return d, nil
}

// knownApp keeps keys to app ids from apps.yaml, so telemetry joins metrics_daily.
func knownApp(app string) error {
	path := env("APPS_CONFIG", "apps.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("apps config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("apps config: %w", err)
	}
	ids := make([]string, 0, len(cfg.Apps))
	for _, a := range cfg.Apps {
		ids = append(ids, a.ID)
	}
	if !slices.Contains(ids, app) {
		return fmt.Errorf("%q is not an app id in %s (known: %v)", app, path, ids)
	}
	return nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
