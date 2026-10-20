package layer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/ingest"
	"github.com/adonm/postfgb/internal/spatial"
	"github.com/adonm/postfgb/internal/store"
)

type city struct {
	name     string
	lng, lat float64
}

var (
	paris = city{"paris", 2.35, 48.85}
	dhaka = city{"dhaka", 90.40, 23.78}
	lagos = city{"lagos", 3.38, 6.52}
)

var testColumns = []chunk.Column{
	{Name: "id", Type: flattypes.ColumnTypeString},
	{Name: "name", Type: flattypes.ColumnTypeString},
}

func openTest(t *testing.T) (*Layer, context.Context) {
	t.Helper()
	dsn := os.Getenv("POSTFGB_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTFGB_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return New(st.Pool), ctx
}

func dropCollection(t *testing.T, l *Layer, name string) {
	t.Helper()
	ctx := context.Background()
	for _, sql := range []string{
		`delete from pfgb.collections where name = $1`,
		`delete from pfgb.blocks where file_id in (select file_id from pfgb.files where collection = $1)`,
		`delete from pfgb.files where collection = $1`,
	} {
		if _, err := l.pool.Exec(ctx, sql, name); err != nil {
			t.Fatal(err)
		}
	}
}

func cityFeatures(prefix string, c city, n int, seed int64) []chunk.InputFeature {
	rng := rand.New(rand.NewSource(seed))
	out := make([]chunk.InputFeature, n)
	for i := range out {
		lng := c.lng + (rng.Float64()-0.5)*0.1
		lat := c.lat + (rng.Float64()-0.5)*0.1
		props := map[string]json.RawMessage{
			"id":   json.RawMessage(strconv.Quote(fmt.Sprintf("%s-%04d", prefix, i))),
			"name": json.RawMessage(strconv.Quote(c.name)),
		}
		out[i] = testPoint(testColumns, props, lng, lat)
	}
	return out
}

func datasetOf(t *testing.T, name string, cols []chunk.Column, features []chunk.InputFeature) ingest.Dataset {
	t.Helper()
	return ingest.Dataset{Header: testHeader(cols, flattypes.GeometryTypePoint), Columns: cols, IDColumn: "id", Features: features}
}

func cellsOf(t *testing.T, features []chunk.InputFeature) []uint64 {
	t.Helper()
	seen := map[uint64]bool{}
	var out []uint64
	for i := range features {
		bounds := features[i].Bounds
		cell := spatial.Partition(spatial.Key(bounds))
		if !seen[cell] {
			seen[cell] = true
			out = append(out, cell)
		}
	}
	return out
}

func box(c city) *[4]float64 {
	return &[4]float64{c.lng - 0.3, c.lat - 0.3, c.lng + 0.3, c.lat + 0.3}
}

func readAll(t *testing.T, l *Layer, name string, q Query) []Hit {
	t.Helper()
	var all []Hit
	for {
		page, err := l.Items(context.Background(), name, q)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Hits...)
		if page.Next == nil {
			return all
		}
		q.Cursor = page.Next
	}
}

func checkOrdered(t *testing.T, hits []Hit) {
	t.Helper()
	for i := 1; i < len(hits); i++ {
		if compareKeyID(hits[i-1].Key, hits[i-1].ID, hits[i].Key, hits[i].ID) >= 0 {
			t.Fatalf("hits out of order at %d: %s then %s", i, hits[i-1].ID, hits[i].ID)
		}
	}
}

func idsOf(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func TestLayerLoadPagesAndReplacesPartitions(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_cities"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })

	parisFeatures := cityFeatures("paris", paris, 300, 1)
	dhakaFeatures := cityFeatures("dhaka", dhaka, 300, 2)
	lagosFeatures := cityFeatures("lagos", lagos, 300, 3)
	all := slices.Concat(parisFeatures, dhakaFeatures, lagosFeatures)
	res, err := l.Load(ctx, name, datasetOf(t, name, testColumns, all), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Features != 900 {
		t.Fatalf("load = %+v", res)
	}

	parisHits := readAll(t, l, name, Query{BBox: box(paris), Limit: 100})
	if len(parisHits) != 300 {
		t.Fatalf("paris bbox returned %d features, want 300", len(parisHits))
	}
	checkOrdered(t, parisHits)
	for _, h := range parisHits {
		if h.ID[:5] != "paris" {
			t.Fatalf("paris bbox returned %s", h.ID)
		}
	}

	whole := readAll(t, l, name, Query{Limit: 1000})
	if len(whole) != 900 {
		t.Fatalf("unfiltered read returned %d, want 900", len(whole))
	}
	checkOrdered(t, whole)

	first, err := l.Items(ctx, name, Query{BBox: box(paris), Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if first.Next == nil || len(first.Hits) != 50 {
		t.Fatalf("first page = %d hits, next %v", len(first.Hits), first.Next)
	}
	cursor := first.Next

	newParis := cityFeatures("paris2", paris, 120, 4)
	cells := slices.Concat(cellsOf(t, parisFeatures), cellsOf(t, newParis))
	slices.Sort(cells)
	cells = slices.Compact(cells)
	res, err = l.Load(ctx, name, datasetOf(t, name, testColumns, newParis), cells)
	if err != nil {
		t.Fatal(err)
	}

	current := readAll(t, l, name, Query{BBox: box(paris), Limit: 100})
	if len(current) != 120 {
		t.Fatalf("paris after reload returned %d, want 120", len(current))
	}
	for _, h := range current {
		if h.ID[:6] != "paris2" {
			t.Fatalf("old paris feature %s survived the reload", h.ID)
		}
	}
	dhakaNow := readAll(t, l, name, Query{BBox: box(dhaka), Limit: 100})
	if len(dhakaNow) != 300 {
		t.Fatalf("dhaka changed by a paris load: %d", len(dhakaNow))
	}

	rest := readAll(t, l, name, Query{BBox: box(paris), Limit: 100, Cursor: cursor})
	var want []string
	for _, h := range current {
		if compareKeyID(h.Key, h.ID, cursor.Key, cursor.ID) > 0 {
			want = append(want, h.ID)
		}
	}
	if !slices.Equal(idsOf(rest), want) {
		t.Fatalf("walk resumed after a reload returned %v, want %v", idsOf(rest), want)
	}

	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, dhakaFeatures), cells); !errors.Is(err, ErrInvalid) {
		t.Fatalf("feature outside the listed cells: %v", err)
	}
	dupe := append(cityFeatures("dup", paris, 1, 5), cityFeatures("dup", paris, 1, 5)...)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, dupe), cellsOf(t, dupe)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate ids: %v", err)
	}
	wider := append(slices.Clone(testColumns), chunk.Column{Name: "extra", Type: flattypes.ColumnTypeString})
	if _, err := l.Load(ctx, name, datasetOf(t, name, wider, newParis), cells); !errors.Is(err, ErrInvalid) {
		t.Fatalf("schema change on a partition load: %v", err)
	}

	dhakaCells := cellsOf(t, dhakaFeatures)
	if _, err := l.Load(ctx, name, ingest.Dataset{Columns: testColumns, IDColumn: "id"}, dhakaCells); err != nil {
		t.Fatalf("emptying dhaka's partitions: %v", err)
	}
	emptied := readAll(t, l, name, Query{BBox: box(dhaka), Limit: 100})
	if len(emptied) != 0 {
		t.Fatalf("dhaka still returns %d features after emptying", len(emptied))
	}

	if _, err := l.Items(ctx, name, Query{BBox: box(paris), Limit: 10, Cursor: &Cursor{UID: cursor.UID + 1, Level: cursor.Level, Key: cursor.Key, ID: cursor.ID}}); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("cursor for another collection: %v", err)
	}
	if _, err := l.Items(ctx, name, Query{BBox: box(paris), Limit: 10, Cursor: cursor}); err != nil {
		t.Fatalf("cursor reuse: %v", err)
	}
}

func TestWholeLoadRemovesDroppedCells(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_whole"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	both := slices.Concat(cityFeatures("paris", paris, 20, 81), cityFeatures("dhaka", dhaka, 20, 82))
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, both), nil); err != nil {
		t.Fatal(err)
	}
	parisOnly := cityFeatures("paris", paris, 20, 83)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, parisOnly), nil); err != nil {
		t.Fatal(err)
	}
	if hits := readAll(t, l, name, Query{Limit: 64}); len(hits) != 20 {
		t.Fatalf("whole reload left %d features, want 20", len(hits))
	}
	if _, err := l.Lookup(ctx, name, "dhaka-0000"); !errors.Is(err, ErrFeatureNotFound) {
		t.Fatalf("dropped cell still resolves: %v", err)
	}
	var rows int
	if err := l.pool.QueryRow(ctx, `select count(*) from pfgb.ids where collection_uid = (select uid from pfgb.collections where name = $1)`, name).Scan(&rows); err != nil || rows != 20 {
		t.Fatalf("id index rows after a whole reload = %d (%v)", rows, err)
	}
}

func TestDecodeCursorRoundTrip(t *testing.T) {
	want := Cursor{UID: 7, Level: 8, Key: 1<<63 + 5, ID: "a.b.c"}
	got, err := DecodeCursor(want.String())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %+v", got)
	}
	if _, err := DecodeCursor("!!!"); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("garbage cursor: %v", err)
	}
	old := base64.RawURLEncoding.EncodeToString([]byte("7.42.0.5.a.b"))
	if _, err := DecodeCursor(old); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("cursor in the old format: %v", err)
	}
}

func TestLookupFollowsLoadsAndFlagsDuplicates(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_lookup"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })

	parisBase := cityFeatures("paris", paris, 200, 11)
	dhakaBase := cityFeatures("dhaka", dhaka, 200, 12)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, slices.Concat(parisBase, dhakaBase)), nil); err != nil {
		t.Fatal(err)
	}
	var live int
	if err := l.pool.QueryRow(ctx, `select count(*) from pfgb.ids where collection_uid = (select uid from pfgb.collections where name = $1)`, name).Scan(&live); err != nil || live != 400 {
		t.Fatalf("id index rows after a whole load = %d (%v)", live, err)
	}
	if hit, err := l.Lookup(ctx, name, "paris-0007"); err != nil || hit.ID != "paris-0007" {
		t.Fatalf("lookup paris-0007: %v", err)
	}
	if _, err := l.Lookup(ctx, name, "no-such-id"); !errors.Is(err, ErrFeatureNotFound) {
		t.Fatalf("missing id: %v", err)
	}

	moveParis := cityFeatures("moved", paris, 1, 13)
	moveDhaka := cityFeatures("moved", dhaka, 1, 13)
	parisNow := slices.Concat(parisBase, moveParis)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, parisNow), cellsOf(t, parisNow)); err != nil {
		t.Fatal(err)
	}
	hit, err := l.Lookup(ctx, name, "moved-0000")
	if err != nil || spatial.Partition(hit.Key) != cellsOf(t, moveParis)[0] {
		t.Fatalf("moved feature in paris: %v", err)
	}

	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, parisBase), cellsOf(t, parisNow)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Lookup(ctx, name, "moved-0000"); !errors.Is(err, ErrFeatureNotFound) {
		t.Fatalf("removed feature still found: %v", err)
	}

	dhakaNow := slices.Concat(dhakaBase, moveDhaka)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, dhakaNow), cellsOf(t, dhakaNow)); err != nil {
		t.Fatal(err)
	}
	hit, err = l.Lookup(ctx, name, "moved-0000")
	if err != nil || spatial.Partition(hit.Key) != cellsOf(t, moveDhaka)[0] {
		t.Fatalf("feature moved to dhaka: %v", err)
	}

	dupParis := cityFeatures("dup", paris, 1, 14)
	parisDup := slices.Concat(parisBase, dupParis)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, parisDup), cellsOf(t, parisDup)); err != nil {
		t.Fatal(err)
	}
	dupDhaka := cityFeatures("dup", dhaka, 1, 14)
	dhakaDup := slices.Concat(dhakaNow, dupDhaka)
	if _, err := l.Load(ctx, name, datasetOf(t, name, testColumns, dhakaDup), cellsOf(t, dhakaDup)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Lookup(ctx, name, "dup-0000"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a duplicate created by two partition loads: %v", err)
	}
}
