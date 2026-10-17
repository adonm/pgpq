package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"

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
	readonlyHelp := "serve the read-only surface: no ingest, no delete, no raw chunk or manifest access"
	readonly := fs.Bool("readonly", envBool("POSTFGB_READONLY"), readonlyHelp)
	runMigrations := fs.Bool("migrate", true, "run migrations at startup (skipped by default in --readonly)")
	fs.Parse(args)
	ctx := context.Background()
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fatal(err)
	}
	defer st.Close()
	migrate := *runMigrations
	if *readonly {
		explicit := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "migrate" {
				explicit = true
			}
		})
		migrate = migrate && explicit
	}
	if migrate {
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
	if *readonly {
		slog.Info("read-only worker")
	}
	slog.Info("listening", "addr", *addr)
	fatal(http.ListenAndServe(*addr, api.NewWithConfig(api.Config{
		Store:    st,
		ReadOnly: *readonly,
	})))
}

func envBool(name string) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	return err == nil && value
}
