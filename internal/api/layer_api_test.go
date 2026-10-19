package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/spatial"
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

func call(h http.Handler, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func layerBody(t *testing.T, name string, lng, lat float64, n int) []byte {
	t.Helper()
	cols := []chunk.Column{
		{Name: "id", Type: flattypes.ColumnTypeString},
		{Name: "name", Type: flattypes.ColumnTypeString},
	}
	rng := rand.New(rand.NewSource(int64(n)))
	features := make([]chunk.InputFeature, n)
	for i := range features {
		coords, err := json.Marshal([]float64{lng + (rng.Float64()-0.5)*0.05, lat + (rng.Float64()-0.5)*0.05})
		if err != nil {
			t.Fatal(err)
		}
		features[i] = chunk.InputFeature{
			Geometry: chunk.RawGeometry{Type: "Point", Coordinates: coords},
			Properties: map[string]json.RawMessage{
				"id":   json.RawMessage(strconv.Quote(fmt.Sprintf("%s-%03d", name, i))),
				"name": json.RawMessage(strconv.Quote("feature")),
			},
		}
	}
	body, err := chunk.EncodeCols(name, cols, features)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func cleanLayer(t *testing.T, st *store.Store, name string) {
	t.Helper()
	for _, sql := range []string{
		`delete from pfgb.collections where name = $1`,
		`delete from pfgb.blocks where file_id in (select file_id from pfgb.files where collection = $1)`,
		`delete from pfgb.files where collection = $1`,
	} {
		if _, err := st.Pool.Exec(context.Background(), sql, name); err != nil {
			t.Fatal(err)
		}
	}
}

func nextCursor(t *testing.T, body []byte) string {
	t.Helper()
	var page struct {
		Links []struct {
			Href string `json:"href"`
			Rel  string `json:"rel"`
		} `json:"links"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	for _, l := range page.Links {
		if l.Rel == "next" {
			u, err := url.Parse(l.Href)
			if err != nil {
				t.Fatal(err)
			}
			return u.Query().Get("cursor")
		}
	}
	return ""
}

func geoIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var page struct {
		Features []struct {
			ID string `json:"id"`
		} `json:"features"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(page.Features))
	for i, f := range page.Features {
		ids[i] = f.ID
	}
	return ids
}

func TestLayerRoutesRoundTrip(t *testing.T) {
	st := integrationStore(t)
	name := "itest_v2_paris"
	cleanLayer(t, st, name)
	t.Cleanup(func() { cleanLayer(t, st, name) })
	h := New(st)

	put := call(h, "PUT", "/v2/collections/"+name, "application/flatgeobuf", layerBody(t, name, 2.35, 48.85, 40))
	if put.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", put.Code, put.Body.String())
	}
	var loaded struct {
		Features int `json:"features"`
	}
	if err := json.Unmarshal(put.Body.Bytes(), &loaded); err != nil || loaded.Features != 40 {
		t.Fatalf("put response %s (%v)", put.Body.String(), err)
	}

	var seen []string
	path := "/v2/collections/" + name + "/items?limit=15"
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("paging did not terminate")
		}
		page := call(h, "GET", path, "", nil)
		if page.Code != http.StatusOK {
			t.Fatalf("items = %d: %s", page.Code, page.Body.String())
		}
		seen = append(seen, geoIDs(t, page.Body.Bytes())...)
		cursor := nextCursor(t, page.Body.Bytes())
		if cursor == "" {
			break
		}
		path = "/v2/collections/" + name + "/items?limit=15&cursor=" + url.QueryEscape(cursor)
	}
	if len(seen) != 40 {
		t.Fatalf("paged %d features, want 40", len(seen))
	}
	distinct := map[string]bool{}
	for _, id := range seen {
		distinct[id] = true
	}
	if len(distinct) != 40 {
		t.Fatalf("paging repeated features: %d distinct of %d", len(distinct), len(seen))
	}

	fgbPage := call(h, "GET", "/v2/collections/"+name+"/items?f=flatgeobuf&limit=15", "", nil)
	if fgbPage.Code != http.StatusOK || fgbPage.Header().Get("Content-Type") != "application/flatgeobuf" {
		t.Fatalf("fgb items = %d %s", fgbPage.Code, fgbPage.Header().Get("Content-Type"))
	}
	reader, err := fgb.NewReader(bytes.NewReader(fgbPage.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		if _, err := reader.Next(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 15 {
		t.Fatalf("flatgeobuf page holds %d features, want 15", count)
	}

	bad := call(h, "PUT", "/v2/collections/"+name+"?cells=zzz", "application/flatgeobuf", layerBody(t, name, 2.35, 48.85, 5))
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid cell = %d", bad.Code)
	}
	dhaka := spatial.Token(spatial.Partition(spatial.Key([4]float64{90.40, 23.78, 90.40, 23.78})))
	outside := call(h, "PUT", "/v2/collections/"+name+"?cells="+dhaka, "application/flatgeobuf", layerBody(t, name, 2.35, 48.85, 5))
	if outside.Code != http.StatusUnprocessableEntity {
		t.Fatalf("paris features into a dhaka cell = %d: %s", outside.Code, outside.Body.String())
	}
	item := call(h, "GET", "/v2/collections/"+name+"/items/"+name+"-003", "", nil)
	var feature struct {
		ID string `json:"id"`
	}
	if item.Code != http.StatusOK || json.Unmarshal(item.Body.Bytes(), &feature) != nil || feature.ID != name+"-003" {
		t.Fatalf("item lookup = %d: %s", item.Code, item.Body.String())
	}
	if gone := call(h, "GET", "/v2/collections/"+name+"/items/nope", "", nil); gone.Code != http.StatusNotFound {
		t.Fatalf("missing item = %d", gone.Code)
	}
	byName := call(h, "GET", "/v2/collections/"+name+"/items?limit=5&numberMatched=true&name=feature", "", nil)
	var filtered struct {
		NumberMatched *int64            `json:"numberMatched"`
		Features      []json.RawMessage `json:"features"`
	}
	if byName.Code != http.StatusOK || json.Unmarshal(byName.Body.Bytes(), &filtered) != nil || filtered.NumberMatched == nil || *filtered.NumberMatched != 40 || len(filtered.Features) != 5 {
		t.Fatalf("property filter page = %d: %s", byName.Code, byName.Body.String())
	}
	one := call(h, "GET", "/v2/collections/"+name+"/items?numberMatched=true&filter="+url.QueryEscape("id = '"+name+"-003'"), "", nil)
	if one.Code != http.StatusOK || !bytes.Contains(one.Body.Bytes(), []byte(`"numberMatched":1,`)) {
		t.Fatalf("cql filter = %d: %s", one.Code, one.Body.String())
	}
	boxed := call(h, "GET", "/v2/collections/"+name+"/items?numberMatched=true&bbox=2,48,2.7,49.2", "", nil)
	if boxed.Code != http.StatusOK || bytes.Contains(boxed.Body.Bytes(), []byte("numberMatched")) {
		t.Fatalf("numberMatched with a bbox = %d: %s", boxed.Code, boxed.Body.String())
	}
	if broken := call(h, "GET", "/v2/collections/"+name+"/items?filter="+url.QueryEscape("id ="), "", nil); broken.Code != http.StatusBadRequest {
		t.Fatalf("unparsable filter = %d", broken.Code)
	}
	missing := call(h, "GET", "/v2/collections/itest_v2_missing/items", "", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing collection = %d", missing.Code)
	}
}
