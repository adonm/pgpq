package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/ingest"
	"github.com/adonm/postfgb/internal/layer"
	"github.com/adonm/postfgb/internal/spatial"
)

func bundleFor(t *testing.T, name string, body []byte) ([]byte, uint64) {
	t.Helper()
	ds, err := ingest.ReadDataset(bytes.NewReader(body), ingest.Options{Flatten: true})
	if err != nil {
		t.Fatal(err)
	}
	fgb, err := chunk.Open(body)
	if err != nil {
		t.Fatal(err)
	}
	features, err := fgb.SearchAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(features) == 0 {
		t.Fatal("fixture has no features")
	}
	cell := featureCell(t, fgb, features[0])
	for _, feature := range features[1:] {
		if featureCell(t, fgb, feature) != cell {
			t.Fatal("fixture spans more than one cell")
		}
	}
	packed, err := layer.BuildBundle(name, ds, cell)
	if err != nil {
		t.Fatal(err)
	}
	return packed, cell
}

func featureCell(t *testing.T, fgb *chunk.Chunk, feature *flattypes.Feature) uint64 {
	t.Helper()
	shape, err := chunk.ParseFlat(fgb.Header(), feature)
	if err != nil || shape == nil {
		t.Fatalf("fixture feature: %v", err)
	}
	bounds, ok := shape.Bound()
	if !ok {
		t.Fatal("fixture feature has no bounds")
	}
	return spatial.Partition(spatial.Key(bounds))
}

func TestLayerBundleRoute(t *testing.T) {
	st := integrationStore(t)
	name := "itest_v2_bundle"
	cleanLayer(t, st, name)
	t.Cleanup(func() { cleanLayer(t, st, name) })
	h := New(st)
	body := layerBody(t, name, 2.35, 48.85, 40)
	packed, cell := bundleFor(t, name, body)
	path := "/v2/collections/" + name + "/cells/" + spatial.Token(cell)
	put := call(h, "PUT", path, "application/x-tar", packed)
	if put.Code != http.StatusOK {
		t.Fatalf("bundle put = %d: %s", put.Code, put.Body.String())
	}
	var loaded struct {
		Features int `json:"features"`
	}
	if err := json.Unmarshal(put.Body.Bytes(), &loaded); err != nil || loaded.Features != 40 {
		t.Fatalf("bundle response %s (%v)", put.Body.String(), err)
	}
	var seen []string
	query := "/v2/collections/" + name + "/items?limit=15"
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("bundle paging did not terminate")
		}
		page := call(h, "GET", query, "", nil)
		if page.Code != http.StatusOK {
			t.Fatalf("bundle items = %d: %s", page.Code, page.Body.String())
		}
		seen = append(seen, geoIDs(t, page.Body.Bytes())...)
		cursor := nextCursor(t, page.Body.Bytes())
		if cursor == "" {
			break
		}
		query = "/v2/collections/" + name + "/items?limit=15&cursor=" + url.QueryEscape(cursor)
	}
	if len(seen) != 40 {
		t.Fatalf("bundle paged %d features, want 40", len(seen))
	}
	item := call(h, "GET", "/v2/collections/"+name+"/items/"+name+"-003", "", nil)
	if item.Code != http.StatusOK {
		t.Fatalf("bundle item = %d: %s", item.Code, item.Body.String())
	}
	filtered := call(h, "GET", "/v2/collections/"+name+"/items?numberMatched=true&filter="+url.QueryEscape("id = '"+name+"-003'"), "", nil)
	if filtered.Code != http.StatusOK || !bytes.Contains(filtered.Body.Bytes(), []byte(`"numberMatched":1,`)) {
		t.Fatalf("bundle filter = %d: %s", filtered.Code, filtered.Body.String())
	}
	other := spatial.Token(spatial.Partition(spatial.Key([4]float64{90.40, 23.78, 90.40, 23.78})))
	if wrong := call(h, "PUT", "/v2/collections/"+name+"/cells/"+other, "application/x-tar", packed); wrong.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bundle for the wrong cell = %d: %s", wrong.Code, wrong.Body.String())
	}
	if garbage := call(h, "PUT", path, "application/x-tar", []byte("not a bundle")); garbage.Code != http.StatusUnprocessableEntity {
		t.Fatalf("garbage bundle = %d: %s", garbage.Code, garbage.Body.String())
	}
}
