package layer

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/spatial"
)

func TestPreparedBundleRoundTrip(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_bundle"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	features := withValues(denseFeatures("bundle", 2.35, 48.85, 0.004, 600, 201), 0)
	cols := []chunk.Column{
		{Name: "id", Type: flattypes.ColumnTypeString},
		{Name: "name", Type: flattypes.ColumnTypeString},
		{Name: "n", Type: flattypes.ColumnTypeLong},
		{Name: "score", Type: flattypes.ColumnTypeDouble},
		{Name: "flag", Type: flattypes.ColumnTypeBool},
		{Name: "small", Type: flattypes.ColumnTypeShort},
		{Name: "when", Type: flattypes.ColumnTypeDateTime},
	}
	for i := range features {
		features[i].Properties["small"] = json.RawMessage(fmt.Sprintf("%d", i%1000))
		features[i].Properties["when"] = json.RawMessage(`"2026-10-20T00:00:00Z"`)
	}
	cells := cellsOf(t, features)
	if len(cells) != 1 {
		t.Fatalf("features span %d cells", len(cells))
	}
	cell := cells[0]
	ds := datasetOf(t, name, cols, features)
	bundle, err := BuildBundle(name, ds, cell)
	if err != nil {
		t.Fatal(err)
	}
	result, err := l.LoadBundle(ctx, name, cell, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if result.Features != len(features) || result.Partitions != 1 {
		t.Fatalf("bundle load = %+v", result)
	}
	all := readAll(t, l, name, Query{Limit: 97})
	if len(all) != len(features) {
		t.Fatalf("bundle read %d features, want %d", len(all), len(features))
	}
	checkOrdered(t, all)
	var want []string
	for i := range features {
		want = append(want, fmt.Sprintf("bundle-%05d", i))
	}
	slices.Sort(want)
	if !slices.Equal(sortedIDs(all), want) {
		t.Fatal("bundle returned different ids")
	}
	bbox := [4]float64{2.3495, 48.8495, 2.3505, 48.8505}
	boxed := sortedIDs(readAll(t, l, name, Query{Limit: 1000, BBox: &bbox}))
	if !slices.Equal(boxed, featureIDsIn(t, features, bbox)) {
		t.Fatalf("bundle bbox returned %d ids", len(boxed))
	}
	expr, err := cql.Parse("n BETWEEN 100 AND 200")
	if err != nil {
		t.Fatal(err)
	}
	var filtered []string
	for i := 100; i <= 200; i++ {
		filtered = append(filtered, fmt.Sprintf("bundle-%05d", i))
	}
	got := sortedIDs(readAll(t, l, name, Query{Limit: 1000, Filter: expr}))
	if !slices.Equal(got, filtered) {
		t.Fatalf("bundle filter returned %d ids", len(got))
	}
	for _, i := range []int{0, 255, 256, 599} {
		id := fmt.Sprintf("bundle-%05d", i)
		hit, err := l.Lookup(ctx, name, id)
		if err != nil || hit.ID != id {
			t.Fatalf("bundle lookup %s: %v", id, err)
		}
	}
	if hit, err := l.Lookup(ctx, name, "bundle-9999"); !errors.Is(err, ErrFeatureNotFound) {
		t.Fatalf("missing bundle lookup: %v (%+v)", err, hit)
	}
	var encoding string
	if err := l.pool.QueryRow(ctx, `select f.encoding from pfgb.files f join pfgb.chunks c on c.file_id = f.file_id where c.collection = $1 and c.level = 0`, name).Scan(&encoding); err != nil {
		t.Fatal(err)
	}
	if encoding != "zstd-block" {
		t.Fatalf("bundle FGB encoding = %q", encoding)
	}
}

func tamperBundle(t *testing.T, data []byte, edit func(*bundleManifest, map[string][]byte)) []byte {
	t.Helper()
	parts := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		part, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		parts[header.Name] = part
	}
	var manifest bundleManifest
	if err := json.Unmarshal(parts[bundleManifestName], &manifest); err != nil {
		t.Fatal(err)
	}
	edit(&manifest, parts)
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parts[bundleManifestName] = encoded
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range append([]string{bundleManifestName}, bundleParts...) {
		if err := writeTarPart(tw, name, parts[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPreparedBundleVerification(t *testing.T) {
	l, ctx := openTest(t)
	name := "layer_test_bundle_verify"
	dropCollection(t, l, name)
	t.Cleanup(func() { dropCollection(t, l, name) })
	features := withValues(denseFeatures("verify", 2.35, 48.85, 0.004, 300, 202), 0)
	cols := []chunk.Column{
		{Name: "id", Type: flattypes.ColumnTypeString},
		{Name: "name", Type: flattypes.ColumnTypeString},
		{Name: "n", Type: flattypes.ColumnTypeLong},
		{Name: "score", Type: flattypes.ColumnTypeDouble},
		{Name: "flag", Type: flattypes.ColumnTypeBool},
	}
	cell := cellsOf(t, features)[0]
	ds := datasetOf(t, name, cols, features)
	bundle, err := BuildBundle(name, ds, cell)
	if err != nil {
		t.Fatal(err)
	}
	other := cellsOf(t, denseFeatures("other", 90.40, 23.78, 0.004, 1, 203))[0]
	cases := map[string]func() []byte{
		"block hash": func() []byte {
			return tamperBundle(t, bundle, func(m *bundleManifest, parts map[string][]byte) {
				m.Blocks[0] = strings.Repeat("0", 64)
			})
		},
		"index root": func() []byte {
			return tamperBundle(t, bundle, func(m *bundleManifest, parts map[string][]byte) {
				m.IndexRoot[0] += 1
			})
		},
		"sample": func() []byte {
			return tamperBundle(t, bundle, func(m *bundleManifest, parts map[string][]byte) {
				m.Samples[0].ID = "wrong"
			})
		},
		"file hash": func() []byte {
			return tamperBundle(t, bundle, func(m *bundleManifest, parts map[string][]byte) {
				file := m.Files[bundleFence]
				file.SHA256 = strings.Repeat("0", 64)
				m.Files[bundleFence] = file
			})
		},
		"count": func() []byte {
			return tamperBundle(t, bundle, func(m *bundleManifest, parts map[string][]byte) {
				m.Features++
			})
		},
	}
	for label, build := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := l.LoadBundle(ctx, name, cell, build())
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("tampered bundle: %v", err)
			}
		})
	}
	t.Run("wrong cell", func(t *testing.T) {
		if _, err := l.LoadBundle(ctx, name, other, bundle); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bundle for another cell: %v", err)
		}
	})
	t.Run("schema mismatch", func(t *testing.T) {
		otherName := name + "_schema"
		dropCollection(t, l, otherName)
		t.Cleanup(func() { dropCollection(t, l, otherName) })
		narrow := []chunk.Column{{Name: "id", Type: flattypes.ColumnTypeString}}
		if _, err := l.Load(ctx, otherName, datasetOf(t, otherName, narrow, features[:10]), nil); err != nil {
			t.Fatal(err)
		}
		otherBundle, err := BuildBundle(otherName, ds, cell)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.LoadBundle(ctx, otherName, cell, otherBundle); !errors.Is(err, ErrInvalid) {
			t.Fatalf("schema-mismatched bundle: %v", err)
		}
	})
	if _, err := l.LoadBundle(ctx, name, cell, bundle); err != nil {
		t.Fatalf("valid bundle after rejected ones: %v", err)
	}
	if hits := readAll(t, l, name, Query{Limit: 64}); len(hits) != len(features) {
		t.Fatalf("valid bundle read %d, want %d", len(hits), len(features))
	}
}

func TestPreparedBundleBuildRejectsOutsideCells(t *testing.T) {
	features := denseFeatures("outside", 2.35, 48.85, 0.004, 10, 204)
	other := cellsOf(t, denseFeatures("outside-other", 90.40, 23.78, 0.004, 1, 205))[0]
	cols := []chunk.Column{{Name: "id", Type: flattypes.ColumnTypeString}}
	ds := datasetOf(t, "outside", cols, features)
	if _, err := BuildBundle("outside", ds, other); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bundle for a cell without features: %v", err)
	}
	if got := spatial.Partition(spatial.Key([4]float64{2.35, 48.85, 2.35, 48.85})); got != cellsOf(t, features)[0] {
		t.Fatal("test fixture drifted")
	}
}
