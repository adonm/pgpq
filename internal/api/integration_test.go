package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/adonm/postfgb/internal/chunk"
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
				"name": json.RawMessage(fmt.Sprintf("%q", fmt.Sprintf("feature-%d", i))),
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
	if err := st.Pool.QueryRow(ctx, `
		select count(*) from pg_tables
		where tablename in ($1, $2)`,
		"chunks_"+name+"_next", "chunk_props_"+name+"_next").Scan(&leftovers); err != nil {
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
