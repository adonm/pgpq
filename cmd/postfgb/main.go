package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"

	"github.com/adonm/postfgb/internal/api"
	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cover"
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
	case "ingest":
		ingest(os.Args[2:])
	case "gen":
		genFeatures(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: postfgb <serve|ingest|gen> [flags]")
}

func fatal(err error) {
	slog.Error(err.Error())
	os.Exit(1)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dsn := fs.String("dsn", os.Getenv("POSTFGB_DSN"), "postgres dsn")
	adminToken := fs.String("admin-token", os.Getenv("POSTFGB_ADMIN_TOKEN"), "bearer token for the manifest, chunk and export endpoints")
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

type rawFeature struct {
	Type       string                     `json:"type"`
	Geometry   chunk.RawGeometry          `json:"geometry"`
	Properties map[string]json.RawMessage `json:"properties"`
}

func ingest(args []string) {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("POSTFGB_DSN"), "postgres dsn")
	collection := fs.String("collection", "", "collection name")
	file := fs.String("file", "", "geojson FeatureCollection file")
	chunkSize := fs.Int("chunk-size", 50000, "features per chunk")
	runMigrations := fs.Bool("migrate", true, "run migrations at startup")
	fs.Parse(args)
	if *collection == "" || *file == "" {
		fs.Usage()
		os.Exit(2)
	}
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
	if err := st.EnsureCollection(ctx, *collection); err != nil {
		fatal(err)
	}
	f, err := os.Open(*file)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(f, 1<<20))
	tok, err := dec.Token()
	if err != nil {
		fatal(err)
	}
	if tok != json.Delim('{') {
		fatal(errors.New("expected a GeoJSON FeatureCollection object"))
	}
	batch := make([]chunk.InputFeature, 0, *chunkSize)
	total, chunks := 0, 0
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			fatal(err)
		}
		if key != "features" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				fatal(err)
			}
			continue
		}
		if _, err := dec.Token(); err != nil {
			fatal(err)
		}
		for dec.More() {
			var rf rawFeature
			if err := dec.Decode(&rf); err != nil {
				fatal(err)
			}
			batch = append(batch, chunk.InputFeature{Geometry: rf.Geometry, Properties: rf.Properties})
			total++
			if len(batch) == *chunkSize {
				putChunk(ctx, st, *collection, batch)
				chunks++
				slog.Info("chunk stored", "features", total, "chunks", chunks)
				batch = batch[:0]
			}
		}
		if _, err := dec.Token(); err != nil {
			fatal(err)
		}
	}
	if len(batch) > 0 {
		putChunk(ctx, st, *collection, batch)
		chunks++
	}
	slog.Info("done", "features", total, "chunks", chunks)
}

func putChunk(ctx context.Context, st *store.Store, collection string, batch []chunk.InputFeature) {
	data, err := chunk.Encode(collection, batch)
	if err != nil {
		fatal(err)
	}
	c, err := chunk.Open(data)
	if err != nil {
		fatal(err)
	}
	env, ok := c.Envelope()
	if !ok {
		fatal(errors.New("chunk missing envelope"))
	}
	payload := store.Payload{
		Data:  data,
		Count: c.Count(),
		MinX:  env[0], MinY: env[1], MaxX: env[2], MaxY: env[3],
		Cover: cover.Chunk(env[0], env[1], env[2], env[3]),
	}
	sum := sha256.Sum256(data)
	payload.Checksum = hex.EncodeToString(sum[:])
	type column struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	cols := c.Columns()
	list := make([]column, len(cols))
	for i, col := range cols {
		list[i] = column{Name: col.Name, Type: col.Type.String()}
	}
	columns, err := json.Marshal(list)
	if err != nil {
		fatal(err)
	}
	payload.Columns = string(columns)
	id, err := st.PutChunk(ctx, collection, payload)
	if err != nil {
		fatal(err)
	}
	docs := make([]string, len(batch))
	for i := range batch {
		docs[i] = chunk.Text(batch[i].Properties)
	}
	if err := st.PutChunkText(ctx, id, docs); err != nil {
		fatal(err)
	}
	props := make([]string, len(batch))
	for i := range batch {
		body, err := json.Marshal(batch[i].Properties)
		if err != nil {
			fatal(err)
		}
		props[i] = string(body)
	}
	if err := st.PutChunkProps(ctx, id, props); err != nil {
		fatal(err)
	}
}

func genFeatures(args []string) {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	n := fs.Int("n", 1000000, "feature count")
	out := fs.String("out", "features.geojson", "output file")
	minX := fs.Float64("minx", -180, "minimum longitude")
	minY := fs.Float64("miny", -85, "minimum latitude")
	maxX := fs.Float64("maxx", 180, "maximum longitude")
	maxY := fs.Float64("maxy", 85, "maximum latitude")
	seed := fs.Int64("seed", 42, "random seed")
	fs.Parse(args)
	f, err := os.Create(*out)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	defer w.Flush()
	rng := rand.New(rand.NewSource(*seed))
	w.WriteString(`{"type":"FeatureCollection","features":[`)
	for i := 0; i < *n; i++ {
		x := *minX + rng.Float64()*(*maxX-*minX)
		y := *minY + rng.Float64()*(*maxY-*minY)
		if i > 0 {
			w.WriteByte(',')
		}
		fmt.Fprintf(w,
			`{"type":"Feature","geometry":{"type":"Point","coordinates":[%g,%g]},"properties":{"i":%d,"name":"feature-%d"}}`,
			x, y, i, i)
	}
	w.WriteString(`]}`)
	slog.Info("generated", "features", *n, "file", *out)
}
