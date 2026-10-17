package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/adonm/postfgb/internal/api"
	"github.com/adonm/postfgb/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: postfgb serve [flags]")
}

func fatal(err error) {
	slog.Error(err.Error())
	os.Exit(1)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dsn := fs.String("dsn", os.Getenv("POSTFGB_DSN"), "postgres dsn")
	adminToken := fs.String("admin-token", os.Getenv("POSTFGB_ADMIN_TOKEN"), "bearer token for the manifest, chunk and ingest endpoints")
	runMigrations := fs.Bool("migrate", true, "run migrations at startup")
	fs.Parse(args)
	ctx := context.Background()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fatal(err)
	}
	defer st.Close()
	if *runMigrations {
		if err := st.Migrate(ctx); err != nil {
			fatal(err)
		}
	}
	caps, err := st.Capabilities(ctx)
	if err != nil {
		fatal(err)
	}
	for _, name := range store.TrackedExtensions {
		if caps[name] {
			slog.Info("extension available", "name", name)
		}
	}
	slog.Info("listening", "addr", *addr)
	fatal(http.ListenAndServe(*addr, api.NewWithConfig(api.Config{
		Store:      st,
		AdminToken: *adminToken,
	})))
}
