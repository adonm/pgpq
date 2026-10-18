package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/adonm/postfgb/internal/api"
	"github.com/adonm/postfgb/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "backfill-stats":
		backfillStats(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: postfgb serve [flags]")
	fmt.Fprintln(os.Stderr, "       postfgb backfill-stats [flags] [collection]")
	os.Exit(2)
}

func fatal(err error) {
	slog.Error(err.Error())
	os.Exit(1)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	dsn := fs.String("dsn", os.Getenv("POSTFGB_DSN"), "postgres dsn")
	maxConns := fs.Int("max-conns", 0, "maximum PostgreSQL pool connections (0 keeps the driver default)")
	minConns := fs.Int("min-conns", 0, "minimum idle PostgreSQL pool connections (0 keeps the driver default)")
	maxChunkBytes := fs.Int("max-chunk-bytes", 32<<20, "per-request chunk fetch budget in bytes")
	readonly := fs.Bool("readonly", envBool("POSTFGB_READONLY"), "serve the read-only surface: no ingest and no delete")
	migrate := fs.Bool("migrate", true, "run migrations at startup (skipped by default in --readonly)")
	fs.Parse(args)
	ctx := context.Background()
	st, err := store.OpenWithPool(ctx, *dsn, store.PoolOptions{MaxConns: int32(*maxConns), MinConns: int32(*minConns)})
	if err != nil {
		fatal(err)
	}
	defer st.Close()
	migrateSet := false
	fs.Visit(func(f *flag.Flag) { migrateSet = migrateSet || f.Name == "migrate" })
	if *migrate && (migrateSet || !*readonly) {
		if err := st.Migrate(ctx); err != nil {
			fatal(err)
		}
	}
	if *readonly {
		slog.Info("read-only worker")
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.NewWithConfig(api.Config{Store: st, ReadOnly: *readonly, MaxBytes: *maxChunkBytes}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	slog.Info("listening", "addr", *addr)
	select {
	case err := <-errCh:
		fatal(err)
	case sig := <-stop:
		slog.Info("shutting down", "signal", sig.String())
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "error", err)
	}
}

func envBool(name string) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	return err == nil && value
}

func backfillStats(args []string) {
	fs := flag.NewFlagSet("backfill-stats", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("POSTFGB_DSN"), "postgres dsn")
	fs.Parse(args)
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "usage: postfgb backfill-stats [flags] [collection]")
		os.Exit(2)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		fatal(err)
	}
	results, err := st.BackfillStats(ctx, fs.Arg(0))
	if err != nil {
		fatal(err)
	}
	for _, result := range results {
		slog.Info("backfilled statistics", "collection", result.Collection, "chunks", result.Chunks,
			"rows", result.Rows, "bloom_bytes", result.Bytes)
	}
}
