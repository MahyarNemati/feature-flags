// Command flagd serves the feature-flag API and dashboard.
//
//	FLAGD_ADMIN_TOKEN=... FLAGD_SDK_TOKEN=... flagd
//	flagd -seed flags.example.json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	flagmodel "github.com/MahyarNemati/feature-flags/internal/flag"

	"github.com/MahyarNemati/feature-flags/internal/api"
	"github.com/MahyarNemati/feature-flags/internal/store"
	"github.com/MahyarNemati/feature-flags/internal/web"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr        = flag.String("addr", envOr("FLAGD_ADDR", ":8080"), "listen address")
		seed        = flag.String("seed", os.Getenv("FLAGD_SEED"), "JSON file of flags to load at startup")
		logFormat   = flag.String("log-format", envOr("FLAGD_LOG_FORMAT", "text"), "text or json")
		logLevel    = flag.String("log-level", envOr("FLAGD_LOG_LEVEL", "info"), "debug, info, warn or error")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("flagd", version)
		return nil
	}

	log, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		return err
	}
	slog.SetDefault(log)

	adminToken := os.Getenv("FLAGD_ADMIN_TOKEN")
	sdkToken := os.Getenv("FLAGD_SDK_TOKEN")

	var tokens []api.Token
	if adminToken != "" {
		if len(adminToken) < 16 {
			return errors.New("FLAGD_ADMIN_TOKEN must be at least 16 characters")
		}
		tokens = append(tokens, api.Token{Value: adminToken, Name: "admin", Admin: true})
	}
	if sdkToken != "" {
		if len(sdkToken) < 16 {
			return errors.New("FLAGD_SDK_TOKEN must be at least 16 characters")
		}
		tokens = append(tokens, api.Token{Value: sdkToken, Name: "sdk", Admin: false})
	}
	if len(tokens) == 0 {
		// Refusing is the safe reading: a flag service with no auth lets
		// anyone turn features on in production.
		return errors.New("set FLAGD_ADMIN_TOKEN (and optionally FLAGD_SDK_TOKEN); refusing to run unauthenticated")
	}

	st := store.NewMemory()
	defer st.Close()

	if *seed != "" {
		n, err := loadSeed(context.Background(), st, *seed)
		if err != nil {
			return fmt.Errorf("seed %s: %w", *seed, err)
		}
		log.Info("seeded flags", "file", *seed, "count", n)
	}

	apiSrv := api.New(st, tokens, log)
	dash, err := web.New(st, adminToken, log)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	// The dashboard owns "/" and the API owns "/api/...", so mount the API
	// handler on its prefix and let the dashboard take the rest.
	mux.Handle("/api/", apiSrv.Handler())
	mux.Handle("/healthz", apiSrv.Handler())
	mux.Handle("/readyz", apiSrv.Handler())
	dash.Mount(mux)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would cut off the SSE stream, which is meant to
		// stay open indefinitely.
		IdleTimeout: 120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr, "version", version,
			"dashboard", "http://localhost"+*addr, "admin_token_set", adminToken != "")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// loadSeed creates the flags in a JSON file. Used for development and for the
// demo in CI; a real deployment uses a persistent store.
func loadSeed(ctx context.Context, st store.Store, path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var flags []*flagmodel.Flag
	if err := json.Unmarshal(raw, &flags); err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	for _, f := range flags {
		if _, err := st.Create(ctx, f, "seed", "loaded from "+path); err != nil {
			return 0, fmt.Errorf("create %q: %w", f.Key, err)
		}
	}
	return len(flags), nil
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(format) {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
