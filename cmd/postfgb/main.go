package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strconv"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/api"
	"github.com/adonm/postfgb/internal/chunk"
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
	case "gen":
		genFeatures(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: postfgb <serve|gen> [flags]")
}

func fatal(err error) {
	slog.Error(err.Error())
	os.Exit(1)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dsn := fs.String("dsn", os.Getenv("POSTFGB_DSN"), "postgres dsn")
	adminToken := fs.String("admin-token", os.Getenv("POSTFGB_ADMIN_TOKEN"), "bearer token for the manifest, chunk, export and ingest endpoints")
	rustgisCLI := fs.String("rustgis-cli", os.Getenv("POSTFGB_RUSTGIS_CLI"), "rustgis-cli binary used by the export endpoint")
	ogr2ogr := fs.String("ogr2ogr", os.Getenv("POSTFGB_OGR2OGR"), "ogr2ogr binary used by the export endpoint")
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
		RustgisCLI: *rustgisCLI,
		OGR2OGR:    *ogr2ogr,
	})))
}

func genFeatures(args []string) {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	n := fs.Int("n", 1000000, "feature count")
	out := fs.String("out", "features.fgb", "output flatgeobuf file")
	minX := fs.Float64("minx", -180, "minimum longitude")
	minY := fs.Float64("miny", -85, "minimum latitude")
	maxX := fs.Float64("maxx", 180, "maximum longitude")
	maxY := fs.Float64("maxy", 85, "maximum latitude")
	seed := fs.Int64("seed", 42, "random seed")
	fs.Parse(args)
	rng := rand.New(rand.NewSource(*seed))
	features := make([]chunk.InputFeature, *n)
	for i := range features {
		x := *minX + rng.Float64()*(*maxX-*minX)
		y := *minY + rng.Float64()*(*maxY-*minY)
		features[i] = chunk.InputFeature{
			Geometry: chunk.RawGeometry{
				Type:        "Point",
				Coordinates: json.RawMessage(fmt.Sprintf("[%g,%g]", x, y)),
			},
			Properties: map[string]json.RawMessage{
				"i":    json.RawMessage(strconv.Itoa(i)),
				"name": json.RawMessage(strconv.Quote(fmt.Sprintf("feature-%d", i))),
			},
		}
	}
	f, err := os.Create(*out)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	cols := []chunk.Column{
		{Name: "i", Type: flattypes.ColumnTypeLong},
		{Name: "name", Type: flattypes.ColumnTypeString},
	}
	if _, err := chunk.WriteFile(f, "features", cols, features); err != nil {
		fatal(err)
	}
}
