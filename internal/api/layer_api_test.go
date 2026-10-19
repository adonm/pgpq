package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/spatial"
	"github.com/adonm/postfgb/internal/store"
)

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
		Snapshot int64 `json:"snapshot"`
		Features int   `json:"features"`
	}
	if err := json.Unmarshal(put.Body.Bytes(), &loaded); err != nil || loaded.Snapshot != 1 || loaded.Features != 40 {
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
	missing := call(h, "GET", "/v2/collections/itest_v2_missing/items", "", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing collection = %d", missing.Code)
	}
}
