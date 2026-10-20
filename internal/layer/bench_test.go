package layer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/ingest"
	"github.com/adonm/postfgb/internal/store"
)

const benchCollection = "layer_bench_cell_files"

func benchFeatures(n int) []chunk.InputFeature {
	rng := rand.New(rand.NewSource(2026))
	out := make([]chunk.InputFeature, n)
	for i := range out {
		x := 2.35 + (rng.Float64()-0.5)*0.02
		y := 48.85 + (rng.Float64()-0.5)*0.02
		coords, _ := json.Marshal([]float64{x, y})
		out[i] = chunk.InputFeature{
			Geometry: chunk.RawGeometry{Type: "Point", Coordinates: coords},
			Properties: map[string]json.RawMessage{
				"id":   json.RawMessage(strconv.Quote(fmt.Sprintf("bench-%05d", i))),
				"name": json.RawMessage(strconv.Quote("bench")),
				"n":    json.RawMessage(strconv.Itoa(i)),
			},
		}
	}
	return out
}

func benchLayer(b *testing.B) (*Layer, context.Context, []chunk.InputFeature) {
	b.Helper()
	dsn := os.Getenv("POSTFGB_TEST_DSN")
	if dsn == "" {
		b.Skip("POSTFGB_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		b.Fatal(err)
	}
	l := New(st.Pool)
	dropCollectionBench(ctx, b, l, benchCollection)
	b.Cleanup(func() { dropCollectionBench(context.Background(), b, l, benchCollection) })
	features := benchFeatures(50000)
	cols := []chunk.Column{
		{Name: "id", Type: flattypes.ColumnTypeString},
		{Name: "name", Type: flattypes.ColumnTypeString},
		{Name: "n", Type: flattypes.ColumnTypeLong},
	}
	body, err := chunk.EncodeCols(benchCollection, cols, features)
	if err != nil {
		b.Fatal(err)
	}
	ds, err := ingest.ReadDataset(bytes.NewReader(body), ingest.Options{Flatten: true})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := l.Load(ctx, benchCollection, ds, nil); err != nil {
		b.Fatal(err)
	}
	return l, ctx, features
}

func dropCollectionBench(ctx context.Context, b *testing.B, l *Layer, name string) {
	b.Helper()
	for _, sql := range []string{
		`delete from pfgb.collections where name = $1`,
		`delete from pfgb.blocks where file_id in (select file_id from pfgb.files where collection = $1)`,
		`delete from pfgb.files where collection = $1`,
	} {
		if _, err := l.pool.Exec(ctx, sql, name); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCellFileItemsBBox(b *testing.B) {
	l, ctx, _ := benchLayer(b)
	bbox := [4]float64{2.350, 48.850, 2.355, 48.855}
	q := Query{BBox: &bbox, Limit: 100}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.Items(ctx, benchCollection, q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCellFileItemsFilter(b *testing.B) {
	l, ctx, _ := benchLayer(b)
	expr, err := cql.Parse("n BETWEEN 25000 AND 30000")
	if err != nil {
		b.Fatal(err)
	}
	q := Query{Filter: expr, Limit: 100}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.Items(ctx, benchCollection, q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCellFileLookup(b *testing.B) {
	l, ctx, _ := benchLayer(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.Lookup(ctx, benchCollection, "bench-25000"); err != nil {
			b.Fatal(err)
		}
	}
}
