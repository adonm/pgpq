package ingest

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/adonm/postfgb/internal/chunk"
)

func TestSortWindowClusters(t *testing.T) {
	feature := func(x, y float64, cluster string, id int) chunk.InputFeature {
		return chunk.InputFeature{
			Geometry: chunk.RawGeometry{
				Type:        "Point",
				Coordinates: json.RawMessage("[" + strconv.FormatFloat(x, 'f', 2, 64) + "," + strconv.FormatFloat(y, 'f', 2, 64) + "]"),
			},
			Properties: map[string]json.RawMessage{
				"cluster": json.RawMessage(`"` + cluster + `"`),
				"id":      json.RawMessage(strconv.Itoa(id)),
			},
		}
	}
	features := []chunk.InputFeature{
		feature(1, 1, "a", 0), feature(120, -40, "b", 1),
		feature(2, 2, "a", 2), feature(121, -41, "b", 3),
		feature(3, 3, "a", 4), feature(122, -42, "b", 5),
		feature(4, 4, "a", 6), feature(123, -43, "b", 7),
	}
	sortWindow(features)
	clusterOf := func(f chunk.InputFeature) string {
		var value string
		if err := json.Unmarshal(f.Properties["cluster"], &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	transitions := 0
	for i := 1; i < len(features); i++ {
		if clusterOf(features[i]) != clusterOf(features[i-1]) {
			transitions++
		}
	}
	if transitions > 1 {
		t.Fatalf("clusters not contiguous after sort: %d transitions", transitions)
	}
}
