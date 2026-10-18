package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/store"
)

func integrationStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("POSTFGB_TEST_DSN")
	if dsn == "" {
		t.Skip("POSTFGB_TEST_DSN is not set")
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
	return st
}

func fixtureFGB(t *testing.T, count int) []byte {
	return sizedFixtureFGB(t, count, 0)
}

func sizedFixtureFGB(t *testing.T, count, pad int) []byte {
	t.Helper()
	features := make([]chunk.InputFeature, 0, count)
	for i := 0; i < count; i++ {
		coords, err := json.Marshal([]float64{7.4 + float64(i%10)*0.002, 43.72 + float64(i/10)*0.002})
		if err != nil {
			t.Fatal(err)
		}
		features = append(features, chunk.InputFeature{
			Geometry: chunk.RawGeometry{Type: "Point", Coordinates: coords},
			Properties: map[string]json.RawMessage{
				"id":   json.RawMessage(strconv.Itoa(i)),
				"name": json.RawMessage(fmt.Sprintf("%q", fmt.Sprintf("feature-%d%s", i, strings.Repeat("x", pad)))),
			},
		})
	}
	body, err := chunk.Encode("itest", features)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func call(h http.Handler, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func featureCount(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	info, err := st.Collection(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return info.FeatureCount
}

func TestPostgresIngestQuery(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	name := "itest_ops"
	st.DropCollection(ctx, name)
	t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	h := New(st)

	w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", fixtureFGB(t, 40))
	if w.Code != 201 {
		t.Fatalf("ingest = %d: %s", w.Code, w.Body.String())
	}
	var ingested struct {
		Features int `json:"features"`
		Chunks   int `json:"chunks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ingested); err != nil {
		t.Fatal(err)
	}
	if ingested.Features != 40 || ingested.Chunks < 1 {
		t.Fatalf("ingest = %+v", ingested)
	}
	if got := featureCount(t, st, name); got != 40 {
		t.Fatalf("feature count = %d, want 40", got)
	}

	w = call(h, "GET", "/collections/"+name+"/items?limit=5&overview=0", "", nil)
	if w.Code != 200 {
		t.Fatalf("items = %d: %s", w.Code, w.Body.String())
	}
	var page struct {
		NumberReturned int `json:"numberReturned"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.NumberReturned != 5 {
		t.Fatalf("numberReturned = %d, want 5", page.NumberReturned)
	}

	w = call(h, "GET", "/collections/"+name+"/items?limit=100&overview=0&bbox=7.4,43.72,7.402,43.722", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.NumberReturned != 4 {
		t.Fatalf("bbox page = %d, want 4", page.NumberReturned)
	}

	w = call(h, "GET", "/collections/"+name+"/items/7", "", nil)
	if w.Code != 200 {
		t.Fatalf("feature = %d: %s", w.Code, w.Body.String())
	}

	w = call(h, "GET", "/collections/"+name+"/items?limit=1&crs="+crs84, "", nil)
	if w.Code != 200 || w.Header().Get("Content-Crs") != crs84 {
		t.Fatalf("crs84 items = %d, Content-Crs %q", w.Code, w.Header().Get("Content-Crs"))
	}
	w = call(h, "GET", "/collections/"+name+"/items?crs=EPSG:3857", "", nil)
	if w.Code != 400 {
		t.Fatalf("epsg:3857 items = %d, want 400", w.Code)
	}
	w = call(h, "GET", "/collections/"+name+"/items?sortby=name", "", nil)
	if w.Code != 400 {
		t.Fatalf("sortby items = %d, want 400", w.Code)
	}
	w = call(h, "GET", "/collections/"+name+"/items?filter=S_INTERSECTS(geometry,POINT(1%202))", "", nil)
	if w.Code != 400 || !bytes.Contains(w.Body.Bytes(), []byte("spatial functions are not supported")) {
		t.Fatalf("spatial filter items = %d: %s", w.Code, w.Body.String())
	}

	w = call(h, "GET", "/stac/collections/"+name, "", nil)
	if w.Code != 200 {
		t.Fatalf("stac collection = %d: %s", w.Code, w.Body.String())
	}
	var collection struct {
		ID     string `json:"id"`
		Extent struct {
			Spatial struct {
				BBox [][]float64 `json:"bbox"`
			} `json:"spatial"`
		} `json:"extent"`
		Links []struct {
			Rel string `json:"rel"`
		} `json:"links"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &collection); err != nil {
		t.Fatal(err)
	}
	if collection.ID != name || len(collection.Extent.Spatial.BBox) != 1 || collection.Extent.Spatial.BBox[0][0] < 7.39 {
		t.Fatalf("stac collection = %+v", collection)
	}
	if len(collection.Links) != 2 || collection.Links[0].Rel != "self" || collection.Links[1].Rel != "parent" {
		t.Fatalf("stac links = %+v", collection.Links)
	}

	w = call(h, "PUT", "/collections/"+name, "application/flatgeobuf", fixtureFGB(t, 7))
	if w.Code != 200 {
		t.Fatalf("replace = %d: %s", w.Code, w.Body.String())
	}
	if got := featureCount(t, st, name); got != 7 {
		t.Fatalf("feature count after replace = %d, want 7", got)
	}
	var leftovers int
	if err := st.Pool.QueryRow(ctx, `select count(*) from pg_tables where schemaname = 'staging'`).Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Fatalf("staging tables left behind: %d", leftovers)
	}

	w = call(h, "PUT", "/collections/"+name, "application/flatgeobuf", []byte("not flatgeobuf"))
	if w.Code != 422 {
		t.Fatalf("invalid ingest = %d: %s", w.Code, w.Body.String())
	}
	if got := featureCount(t, st, name); got != 7 {
		t.Fatalf("feature count after failed ingest = %d, want 7", got)
	}

	ro := NewWithConfig(Config{Store: st, ReadOnly: true})
	w = call(ro, "PUT", "/collections/"+name, "application/flatgeobuf", fixtureFGB(t, 3))
	if w.Code != 405 && w.Code != 404 {
		t.Fatalf("readonly ingest = %d", w.Code)
	}
	w = call(ro, "GET", "/collections/"+name+"/items?limit=1", "", nil)
	if w.Code != 200 {
		t.Fatalf("readonly items = %d: %s", w.Code, w.Body.String())
	}
	w = call(ro, "GET", "/readyz", "", nil)
	if w.Code != 200 {
		t.Fatalf("readyz = %d: %s", w.Code, w.Body.String())
	}
	w = call(ro, "GET", "/metrics", "", nil)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("postfgb_http_requests_total")) {
		t.Fatalf("metrics = %d", w.Code)
	}

	w = call(h, "DELETE", "/collections/"+name, "", nil)
	if w.Code != 204 {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	w = call(h, "GET", "/collections/"+name+"/items?limit=1", "", nil)
	if w.Code != 404 {
		t.Fatalf("items after delete = %d", w.Code)
	}
}

func TestPostgresViewPaging(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	view := "itest_view"
	members := []string{"itest_view_a", "itest_view_b"}
	for _, name := range append(members, view) {
		st.DropCollection(ctx, name)
		t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	}
	h := New(st)
	for _, name := range members {
		if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", fixtureFGB(t, 5)); w.Code != 201 {
			t.Fatalf("ingest %s = %d: %s", name, w.Code, w.Body.String())
		}
	}
	if w := call(h, "PUT", "/collections/"+view, "application/json", []byte(`{"members":["itest_view_a","itest_view_b"]}`)); w.Code != 201 {
		t.Fatalf("view = %d: %s", w.Code, w.Body.String())
	}

	page := call(h, "GET", "/collections/"+view+"/items?limit=3&offset=6", "", nil)
	var geo struct {
		Features []struct {
			Properties struct {
				Name string `json:"name"`
			} `json:"properties"`
		} `json:"features"`
	}
	if err := json.Unmarshal(page.Body.Bytes(), &geo); err != nil || page.Code != 200 {
		t.Fatalf("view page = %d: %s", page.Code, page.Body.String())
	}
	var fromJSON []string
	for _, f := range geo.Features {
		fromJSON = append(fromJSON, f.Properties.Name)
	}

	export := call(h, "GET", "/collections/"+view+"/items?limit=3&offset=6&f=flatgeobuf", "", nil)
	if export.Code != 200 {
		t.Fatalf("view export = %d: %s", export.Code, export.Body.String())
	}
	c, err := chunk.Open(export.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	features, err := c.SearchAll()
	if err != nil {
		t.Fatal(err)
	}
	var fromFGB []string
	for _, f := range features {
		fromFGB = append(fromFGB, fmt.Sprint(c.GeoJSON(f).Properties["name"]))
	}

	slices.Sort(fromJSON)
	slices.Sort(fromFGB)
	if len(fromJSON) != 3 || !slices.Equal(fromJSON, fromFGB) {
		t.Fatalf("view offset 6 page = %v, export = %v", fromJSON, fromFGB)
	}
}

func TestPostgresViewOverview(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	view := "itest_tier_view"
	members := []string{"itest_tier_a", "itest_tier_b"}
	for _, name := range append(members, view) {
		st.DropCollection(ctx, name)
		t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	}
	h := New(st)
	for _, name := range members {
		if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", fixtureFGB(t, 40)); w.Code != 201 {
			t.Fatalf("ingest %s = %d: %s", name, w.Code, w.Body.String())
		}
	}
	if w := call(h, "PUT", "/collections/"+view, "application/json", []byte(`{"members":["itest_tier_a","itest_tier_b"]}`)); w.Code != 201 {
		t.Fatalf("view = %d: %s", w.Code, w.Body.String())
	}

	ids := func(path string) []string {
		t.Helper()
		w := call(h, "GET", path, "", nil)
		var doc struct {
			Features []struct {
				ID any `json:"id"`
			} `json:"features"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || w.Code != 200 {
			t.Fatalf("%s = %d: %s", path, w.Code, w.Body.String())
		}
		var out []string
		for _, f := range doc.Features {
			out = append(out, fmt.Sprint(f.ID))
		}
		return out
	}
	if got := ids("/collections/" + members[0] + "/items?overview=1&limit=100"); len(got) != 4 {
		t.Fatalf("native overview returned %d features, want the 4 tier-1 features", len(got))
	}
	full := ids("/collections/" + view + "/items?limit=100")
	if len(full) != 80 {
		t.Fatalf("view returned %d features, want 80", len(full))
	}
	for _, overview := range []string{"0", "1", "2"} {
		if got := ids("/collections/" + view + "/items?limit=100&overview=" + overview); !slices.Equal(got, full) {
			t.Fatalf("view overview=%s returned %v, want full detail %v", overview, got, full)
		}
	}
	export := call(h, "GET", "/collections/"+view+"/items?limit=100&f=flatgeobuf&overview=1", "", nil)
	c, err := chunk.Open(export.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if exported, err := c.SearchAll(); err != nil || len(exported) != 80 {
		t.Fatalf("view export with overview=1 returned %d features (%v), want 80", len(exported), err)
	}
	plain := call(h, "GET", "/collections/"+view+"/items.fgb", "", nil)
	overview := call(h, "GET", "/collections/"+view+"/items.fgb?overview=1", "", nil)
	if plain.Code != 200 || overview.Code != 200 || !bytes.Equal(plain.Body.Bytes(), overview.Body.Bytes()) {
		t.Fatalf("view items.fgb with overview=1 differs from full detail")
	}
	var numbered struct {
		NumberMatched *int64 `json:"numberMatched"`
	}
	w := call(h, "GET", "/collections/"+view+"/items?limit=1&overview=1&numberMatched=true", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &numbered); err != nil || numbered.NumberMatched == nil || *numbered.NumberMatched != 80 {
		t.Fatalf("view numberMatched with overview=1 = %s, want 80", w.Body.String())
	}

	var tier0, tier1 int64
	if err := st.Pool.QueryRow(ctx, `select id from chunks where collection = $1 and tier = 0`, members[0]).Scan(&tier0); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `select id from chunks where collection = $1 and tier = 1`, members[0]).Scan(&tier1); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/collections/" + members[0] + "/items/%d-0",
		"/collections/" + view + "/items/%d-0",
	} {
		if w := call(h, "GET", fmt.Sprintf(path, tier1), "", nil); w.Code != 404 {
			t.Fatalf("tier-1 feature id via %s = %d, want 404", path, w.Code)
		}
		if w := call(h, "GET", fmt.Sprintf(path, tier0), "", nil); w.Code != 200 {
			t.Fatalf("tier-0 feature id via %s = %d, want 200", path, w.Code)
		}
	}
}

func TestPostgresViewMemberDrop(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	view := "itest_drop_view"
	members := []string{"itest_drop_a", "itest_drop_b"}
	for _, name := range append(members, view) {
		st.DropCollection(ctx, name)
		t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	}
	h := New(st)
	for _, name := range members {
		if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", fixtureFGB(t, 5)); w.Code != 201 {
			t.Fatalf("ingest %s = %d: %s", name, w.Code, w.Body.String())
		}
	}
	if w := call(h, "PUT", "/collections/"+view, "application/json", []byte(`{"members":["itest_drop_a","itest_drop_b"]}`)); w.Code != 201 {
		t.Fatalf("view = %d: %s", w.Code, w.Body.String())
	}
	checkView := func(want int) {
		t.Helper()
		w := call(h, "GET", "/collections/"+view+"/items?limit=1&numberMatched=true", "", nil)
		var counted struct {
			NumberMatched *int64 `json:"numberMatched"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &counted); err != nil || counted.NumberMatched == nil || *counted.NumberMatched != int64(want) {
			t.Fatalf("numberMatched = %s, want %d", w.Body.String(), want)
		}
		file := call(h, "GET", "/collections/"+view+"/items.fgb", "", nil)
		if file.Code != 200 {
			t.Fatalf("items.fgb = %d: %s", file.Code, file.Body.String())
		}
		if got := fgbHeader(file.Body.Bytes()).FeaturesCount(); got != uint64(want) {
			t.Fatalf("items.fgb declares %d features, want %d", got, want)
		}
		c, err := chunk.Open(file.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if features, err := c.SearchAll(); err != nil || len(features) != want {
			t.Fatalf("items.fgb holds %d features (%v), want %d", len(features), err, want)
		}
	}
	checkView(10)
	if w := call(h, "DELETE", "/collections/"+members[0], "", nil); w.Code != 204 {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	checkView(5)

	var raw []byte
	if err := st.Pool.QueryRow(ctx, `select members from collections where name = $1`, view).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var listed []string
	if err := json.Unmarshal(raw, &listed); err != nil || !slices.Equal(listed, members[1:]) {
		t.Fatalf("view members after drop = %s, want %v", raw, members[1:])
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	w := call(h, "GET", "/collections/"+view+"/queryables", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &schema); err != nil || schema.Properties["name"] == nil {
		t.Fatalf("queryables after drop = %s", w.Body.String())
	}
}

func TestPostgresViewDroppedWithLastMember(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	view := "itest_gone_view"
	members := []string{"itest_gone_a", "itest_gone_b"}
	for _, name := range append(members, view) {
		st.DropCollection(ctx, name)
		t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	}
	h := New(st)
	for _, name := range members {
		if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", fixtureFGB(t, 5)); w.Code != 201 {
			t.Fatalf("ingest %s = %d: %s", name, w.Code, w.Body.String())
		}
	}
	if w := call(h, "PUT", "/collections/"+view, "application/json", []byte(`{"members":["itest_gone_a","itest_gone_b"]}`)); w.Code != 201 {
		t.Fatalf("view = %d: %s", w.Code, w.Body.String())
	}
	for _, name := range members {
		if w := call(h, "DELETE", "/collections/"+name, "", nil); w.Code != 204 {
			t.Fatalf("delete %s = %d: %s", name, w.Code, w.Body.String())
		}
	}
	if w := call(h, "GET", "/collections/"+view, "", nil); w.Code != 404 {
		t.Fatalf("view after its last member = %d, want 404", w.Code)
	}
	var rows int
	if err := st.Pool.QueryRow(ctx, `
		select (select count(*) from virtual_file where collection = $1) + (select count(*) from virtual_segment where collection = $1)`,
		view).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("view layout rows left behind: %d", rows)
	}
}

func TestPostgresGeneratedNames(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	long := "itest_" + strings.Repeat("n", 42)
	names := []string{long, "itest_gen", "itest_gen_pkey", "pkey", "tier_idx", "gin_idx", "Itest-Mixed.Name"}
	for _, name := range names {
		st.DropCollection(ctx, name)
		t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	}
	h := New(st)
	for _, name := range names {
		if w := call(h, "PUT", "/collections/"+name+"?sortable=id,name", "application/flatgeobuf", fixtureFGB(t, 5)); w.Code != 201 {
			t.Fatalf("ingest %s = %d: %s", name, w.Code, w.Body.String())
		}
		w := call(h, "GET", "/collections/"+name+"/items?limit=10&numberMatched=true&filter="+url.QueryEscape("id >= 3"), "", nil)
		var counted struct {
			NumberMatched *int64 `json:"numberMatched"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &counted); err != nil || counted.NumberMatched == nil || *counted.NumberMatched != 2 {
			t.Fatalf("%s range filter = %s, want 2 matches", name, w.Body.String())
		}
	}
	for _, bad := range []string{"itest_" + strings.Repeat("n", 43), "itest%20space", "itest%27quote", ".hidden", "-dash"} {
		if w := call(h, "PUT", "/collections/"+bad, "application/flatgeobuf", fixtureFGB(t, 5)); w.Code != 422 {
			t.Fatalf("PUT %s = %d, want 422", bad, w.Code)
		}
		if w := call(h, "DELETE", "/collections/"+bad, "", nil); w.Code != 422 {
			t.Fatalf("DELETE %s = %d, want 422", bad, w.Code)
		}
	}
}

func chunkStarts(t *testing.T, st *store.Store, name string) []int {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(),
		`select feature_count from chunks where collection = $1 and tier = 0 order by id`, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var starts []int
	at := 0
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		starts = append(starts, at)
		at += n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return starts
}

func walkIDs(t *testing.T, s *Server, info store.CollectionInfo, q featureQuery, limit int) []string {
	t.Helper()
	out := &collectSink{limit: limit}
	if err := s.walkFeatures(context.Background(), info, q, out); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(out.hits))
	for _, h := range out.hits {
		ids = append(ids, fmt.Sprint(h.geo.ID))
	}
	return ids
}

func TestPostgresFilteredPaging(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	name := "itest_filtered"
	st.DropCollection(ctx, name)
	t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	h := New(st)
	const count = 25000
	if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", sizedFixtureFGB(t, count, 200)); w.Code != 201 {
		t.Fatalf("ingest = %d: %s", w.Code, w.Body.String())
	}
	info, err := st.Collection(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	full := walkIDs(t, &Server{Store: st, MaxChunks: 64, MaxBytes: 32 << 20, virtuals: newVirtualCache(), chunks: newChunkByteCache()}, info, featureQuery{}, count)
	expr, err := cql.Parse("name LIKE 'feature-1%'")
	if err != nil {
		t.Fatal(err)
	}
	box := [4]float64{7.4, 43.72, 7.405, 45}
	var want []string
	var prefixed int
	for _, id := range full {
		i, _ := strconv.Atoi(id)
		x, y := 7.4+float64(i%10)*0.002, 43.72+float64(i/10)*0.002
		if !strings.HasPrefix(id, "1") {
			continue
		}
		prefixed++
		if x >= box[0] && x <= box[2] && y >= box[1] && y <= box[3] {
			want = append(want, id)
		}
	}
	if len(want) < 100 {
		t.Fatalf("fixture leaves %d matches in the box, want a set spanning several pages", len(want))
	}
	rng := rand.New(rand.NewPCG(9, 9))
	offsets := []int{0}
	for range 20 {
		offsets = append(offsets, rng.IntN(len(want)-5))
	}
	for _, batch := range []struct{ chunks, bytes int }{{1, 32 << 20}, {64, 32 << 20}, {64, 1 << 20}} {
		s := &Server{Store: st, MaxChunks: batch.chunks, MaxBytes: batch.bytes, virtuals: newVirtualCache(), chunks: newChunkByteCache()}
		for _, k := range offsets {
			got := walkIDs(t, s, info, featureQuery{bbox: &box, filter: expr, offset: k}, 5)
			if wantPage := want[k:min(k+5, len(want))]; !slices.Equal(got, wantPage) {
				t.Fatalf("batch %+v offset %d: got %v, want %v", batch, k, got, wantPage)
			}
		}
	}

	query := url.Values{"bbox": {"7.4,43.72,7.405,45"}, "filter": {"name LIKE 'feature-1%'"}, "limit": {"7"}}
	next := fmt.Sprintf("/collections/%s/items?%s", name, query.Encode())
	var got []string
	for next != "" {
		w := call(h, "GET", next, "", nil)
		var doc struct {
			Features []struct {
				ID any `json:"id"`
			} `json:"features"`
			NumberMatched *int64 `json:"numberMatched"`
			Links         []struct {
				Rel  string `json:"rel"`
				Href string `json:"href"`
			} `json:"links"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || w.Code != 200 {
			t.Fatalf("items %s = %d: %s", next, w.Code, w.Body.String())
		}
		if doc.NumberMatched != nil {
			t.Fatalf("numberMatched %d with a bbox, want it omitted", *doc.NumberMatched)
		}
		next = ""
		for _, link := range doc.Links {
			if link.Rel == "next" {
				next = link.Href
			}
		}
		if next != "" && len(doc.Features) != 7 {
			t.Fatalf("page %s has %d features but links to more", next, len(doc.Features))
		}
		for _, f := range doc.Features {
			got = append(got, fmt.Sprint(f.ID))
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("bbox+filter pages walked %d features, want %d", len(got), len(want))
	}

	w := call(h, "GET", "/collections/"+name+"/items?limit=1&numberMatched=true&filter="+url.QueryEscape("name LIKE 'feature-1%'"), "", nil)
	var counted struct {
		NumberMatched *int64 `json:"numberMatched"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &counted); err != nil || counted.NumberMatched == nil || *counted.NumberMatched != int64(prefixed) {
		t.Fatalf("filter-only numberMatched = %s, want %d", w.Body.String(), prefixed)
	}
}

func TestPostgresChunkOffsets(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	name := "itest_offsets"
	st.DropCollection(ctx, name)
	t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	h := New(st)
	const count, page = 25000, 50
	if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", sizedFixtureFGB(t, count, 200)); w.Code != 201 {
		t.Fatalf("ingest = %d: %s", w.Code, w.Body.String())
	}
	info, err := st.Collection(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	starts := chunkStarts(t, st, name)
	if len(starts) < 3 {
		t.Fatalf("want several chunks, got %d", len(starts))
	}
	full := walkIDs(t, &Server{Store: st, MaxChunks: 64, MaxBytes: 32 << 20, virtuals: newVirtualCache(), chunks: newChunkByteCache()}, info, featureQuery{}, count)
	if len(full) != count {
		t.Fatalf("full walk = %d features, want %d", len(full), count)
	}
	rng := rand.New(rand.NewPCG(9, 9))
	var offsets []int
	for _, start := range starts[1:] {
		offsets = append(offsets, start-1, start, start+1)
	}
	for range 40 {
		offsets = append(offsets, rng.IntN(count-page))
	}
	box := [4]float64{7.4, 43.72, 7.405, 45}
	var inside []string
	for _, id := range full {
		i, _ := strconv.Atoi(id)
		x, y := 7.4+float64(i%10)*0.002, 43.72+float64(i/10)*0.002
		if x >= box[0] && x <= box[2] && y >= box[1] && y <= box[3] {
			inside = append(inside, id)
		}
	}
	bboxOffsets := make([]int, 20)
	for i := range bboxOffsets {
		bboxOffsets[i] = rng.IntN(len(inside) - page)
	}
	for _, batch := range []struct{ chunks, bytes int }{{1, 32 << 20}, {64, 32 << 20}, {64, 1 << 20}} {
		s := &Server{Store: st, MaxChunks: batch.chunks, MaxBytes: batch.bytes, virtuals: newVirtualCache(), chunks: newChunkByteCache()}
		for _, k := range offsets {
			got := walkIDs(t, s, info, featureQuery{offset: k}, page)
			if want := full[k:min(k+page, count)]; !slices.Equal(got, want) {
				t.Fatalf("batch %+v offset %d: got %v, want %v", batch, k, got, want)
			}
		}
		if got := walkIDs(t, s, info, featureQuery{bbox: &box}, count); !slices.Equal(got, inside) {
			t.Fatalf("batch %+v bbox walk = %d features, want %d", batch, len(got), len(inside))
		}
		for _, k := range bboxOffsets {
			got := walkIDs(t, s, info, featureQuery{bbox: &box, offset: k}, page)
			if want := inside[k : k+page]; !slices.Equal(got, want) {
				t.Fatalf("batch %+v bbox offset %d: got %v, want %v", batch, k, got, want)
			}
		}
	}

	w := call(h, "GET", fmt.Sprintf("/collections/%s/items?limit=%d&offset=%d", name, page, starts[1]-1), "", nil)
	var doc struct {
		Features []struct {
			ID any `json:"id"`
		} `json:"features"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || w.Code != 200 {
		t.Fatalf("items = %d: %s", w.Code, w.Body.String())
	}
	var got []string
	for _, f := range doc.Features {
		got = append(got, fmt.Sprint(f.ID))
	}
	if want := full[starts[1]-1 : starts[1]-1+page]; !slices.Equal(got, want) {
		t.Fatalf("handler offset %d: got %v, want %v", starts[1]-1, got, want)
	}
}

func assertCurrentFile(t *testing.T, h, fresh http.Handler, path string, features uint64, staleTag string) {
	t.Helper()
	truth := call(fresh, "GET", path, "", nil)
	if header := fgbHeader(truth.Body.Bytes()); truth.Code != 200 || header == nil || header.FeaturesCount() != features {
		t.Fatalf("fresh worker %s = %d: %s", path, truth.Code, truth.Body.String())
	}
	for _, worker := range []http.Handler{h, fresh} {
		w := call(worker, "GET", path, "", nil)
		if w.Code != 200 || w.Header().Get("ETag") == staleTag || !bytes.Equal(w.Body.Bytes(), truth.Body.Bytes()) {
			t.Fatalf("%s = %d, ETag %s (stale %s), %d bytes, want the current %d bytes", path, w.Code, w.Header().Get("ETag"), staleTag, w.Body.Len(), truth.Body.Len())
		}
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("If-None-Match", staleTag)
		conditional := httptest.NewRecorder()
		worker.ServeHTTP(conditional, req)
		if conditional.Code != 200 {
			t.Fatalf("%s with the stale ETag = %d, want 200", path, conditional.Code)
		}
	}
}

func TestPostgresRecreatedNameServesCurrentBytes(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	name, view := "itest_recreated", "itest_recreated_view"
	members := []string{"itest_recreated_a", "itest_recreated_b"}
	for _, n := range append([]string{name, view}, members...) {
		st.DropCollection(ctx, n)
		t.Cleanup(func() { st.DropCollection(context.Background(), n) })
	}
	h := New(st)
	fresh := NewWithConfig(Config{Store: st, ReadOnly: true})

	if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", fixtureFGB(t, 40)); w.Code != 201 {
		t.Fatalf("ingest = %d: %s", w.Code, w.Body.String())
	}
	staleTag := call(h, "GET", "/collections/"+name+"/items.fgb", "", nil).Header().Get("ETag")
	if w := call(h, "DELETE", "/collections/"+name, "", nil); w.Code != 204 {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", fixtureFGB(t, 41)); w.Code != 201 {
		t.Fatalf("re-ingest = %d: %s", w.Code, w.Body.String())
	}
	assertCurrentFile(t, h, fresh, "/collections/"+name+"/items.fgb", 41, staleTag)
	tier0 := call(h, "GET", "/collections/"+name+"/items.fgb", "", nil)
	tier1 := call(h, "GET", "/collections/"+name+"/items.fgb?overview=1", "", nil)
	if tier0.Code != 200 || tier1.Code != 200 || tier0.Header().Get("ETag") == tier1.Header().Get("ETag") {
		t.Fatalf("tier 0 and tier 1 share ETag %q", tier0.Header().Get("ETag"))
	}

	for _, member := range members {
		if w := call(h, "PUT", "/collections/"+member, "application/flatgeobuf", fixtureFGB(t, 5)); w.Code != 201 {
			t.Fatalf("ingest %s = %d: %s", member, w.Code, w.Body.String())
		}
	}
	if w := call(h, "PUT", "/collections/"+view, "application/json", []byte(`{"members":["itest_recreated_a"]}`)); w.Code != 201 {
		t.Fatalf("view = %d: %s", w.Code, w.Body.String())
	}
	staleTag = call(h, "GET", "/collections/"+view+"/items.fgb", "", nil).Header().Get("ETag")
	if w := call(h, "DELETE", "/collections/"+view, "", nil); w.Code != 204 {
		t.Fatalf("delete view = %d: %s", w.Code, w.Body.String())
	}
	if w := call(h, "PUT", "/collections/"+view, "application/json", []byte(`{"members":["itest_recreated_a","itest_recreated_b"]}`)); w.Code != 201 {
		t.Fatalf("re-create view = %d: %s", w.Code, w.Body.String())
	}
	assertCurrentFile(t, h, fresh, "/collections/"+view+"/items.fgb", 10, staleTag)
}

func extraColumnFGB(t *testing.T, count int) []byte {
	t.Helper()
	features := make([]chunk.InputFeature, 0, count)
	for i := 0; i < count; i++ {
		coords, err := json.Marshal([]float64{7.4 + float64(i%10)*0.002, 43.72 + float64(i/10)*0.002})
		if err != nil {
			t.Fatal(err)
		}
		features = append(features, chunk.InputFeature{
			Geometry: chunk.RawGeometry{Type: "Point", Coordinates: coords},
			Properties: map[string]json.RawMessage{
				"id":    json.RawMessage(strconv.Itoa(i)),
				"name":  json.RawMessage(fmt.Sprintf("%q", fmt.Sprintf("feature-%d", i))),
				"extra": json.RawMessage(`"x"`),
			},
		})
	}
	body, err := chunk.Encode("itest", features)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestPostgresViewMemberSchemaReplace(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	view := "itest_schema_view"
	members := []string{"itest_schema_a", "itest_schema_b"}
	for _, name := range append(members, view) {
		st.DropCollection(ctx, name)
		t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	}
	h := New(st)
	for _, name := range members {
		if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", fixtureFGB(t, 5)); w.Code != 201 {
			t.Fatalf("ingest %s = %d: %s", name, w.Code, w.Body.String())
		}
	}
	if w := call(h, "PUT", "/collections/"+view, "application/json", []byte(`{"members":["itest_schema_a","itest_schema_b"]}`)); w.Code != 201 {
		t.Fatalf("view = %d: %s", w.Code, w.Body.String())
	}
	before := call(h, "GET", "/collections/"+view+"/items.fgb", "", nil)
	if header := fgbHeader(before.Body.Bytes()); before.Code != 200 || header == nil || header.FeaturesCount() != 10 {
		t.Fatalf("view items.fgb = %d: %s", before.Code, before.Body.String())
	}
	memberVersion := func() int64 {
		info, err := st.Collection(ctx, members[0])
		if err != nil {
			t.Fatal(err)
		}
		return info.Version
	}
	version := memberVersion()

	w := call(h, "PUT", "/collections/"+members[0], "application/flatgeobuf", extraColumnFGB(t, 5))
	if w.Code != 422 || !bytes.Contains(w.Body.Bytes(), []byte("different schema")) {
		t.Fatalf("replace with another schema = %d: %s", w.Code, w.Body.String())
	}
	if got := memberVersion(); got != version {
		t.Fatalf("member version = %d after a rejected replace, want %d", got, version)
	}
	if got := featureCount(t, st, members[0]); got != 5 {
		t.Fatalf("member feature count = %d after a rejected replace, want 5", got)
	}
	after := call(h, "GET", "/collections/"+view+"/items.fgb", "", nil)
	if after.Code != 200 || after.Header().Get("ETag") != before.Header().Get("ETag") || !bytes.Equal(after.Body.Bytes(), before.Body.Bytes()) {
		t.Fatalf("view changed after a rejected replace: %d, ETag %s, want %s", after.Code, after.Header().Get("ETag"), before.Header().Get("ETag"))
	}
	var leftovers int
	if err := st.Pool.QueryRow(ctx, `select count(*) from pg_tables where schemaname = 'staging'`).Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Fatalf("staging tables left behind: %d", leftovers)
	}

	if w := call(h, "PUT", "/collections/"+members[0], "application/flatgeobuf", fixtureFGB(t, 6)); w.Code != 200 {
		t.Fatalf("same-schema replace = %d: %s", w.Code, w.Body.String())
	}
	var counted struct {
		NumberMatched *int64 `json:"numberMatched"`
	}
	w = call(h, "GET", "/collections/"+view+"/items?limit=1&numberMatched=true", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &counted); err != nil || counted.NumberMatched == nil || *counted.NumberMatched != 11 {
		t.Fatalf("view after a same-schema replace = %s, want 11 matches", w.Body.String())
	}
}
