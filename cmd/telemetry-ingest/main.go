// telemetry-ingest: HTTP receiver for anonymous usage events, protocol v1 (PLAN-analytika §5.1).
//
// Runs as the `ingest` service in deploy/docker-compose.yml behind Caddy and a Cloudflare tunnel.
// POST /v1/batch, GET /healthz. Never logs or stores client addresses.
//
//	telemetry-ingest              serve (env: DATABASE_URL, LISTEN, TELEMETRY_RATE_PER_MIN, TELEMETRY_DAILY_CAP)
//	telemetry-ingest healthcheck  exit 0 when the local /healthz answers 200 (compose healthcheck, no shell in the image)
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/lioilsources/store-metrics/internal/db"
	"github.com/lioilsources/store-metrics/internal/telemetry"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	addr := env("LISTEN", ":8080")
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(addr))
	}
	if err := run(logger, addr); err != nil {
		logger.Error("telemetry-ingest stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, addr string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d, err := db.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer d.Pool.Close()
	store := telemetry.PG{Pool: d.Pool}

	srv := telemetry.New(telemetry.Config{
		Store:         store,
		Log:           logger,
		RatePerMinute: envInt("TELEMETRY_RATE_PER_MIN", 60),
		Burst:         envInt("TELEMETRY_BURST", 20),
		DailyCap:      envInt("TELEMETRY_DAILY_CAP", 200_000),
	})
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// net/http's own error lines carry the client address; drop them (panics are handled in the handler).
		ErrorLog: log.New(io.Discard, "", 0),
	}

	go loop(ctx, 30*time.Second, func() {
		if err := srv.FlushStats(ctx); err != nil {
			logger.Warn("flush stats", "err", err)
		}
	})
	// Retention: once a day (and shortly after start), delete raw events past each app's retention.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		prune := func() {
			n, err := store.Prune(ctx, time.Now())
			if err != nil {
				logger.Warn("prune", "err", err)
				return
			}
			logger.Info("prune", "deleted", n)
		}
		prune()
		loop(ctx, 24*time.Hour, prune)
	}()

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		logger.Warn("shutdown", "err", err)
	}
	if err := srv.FlushStats(sctx); err != nil {
		logger.Warn("final stats flush", "err", err)
	}
	return nil
}

func loop(ctx context.Context, every time.Duration, f func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f()
		}
	}
}

func healthcheck(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}
