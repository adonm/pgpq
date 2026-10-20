package ingest

import (
	"os"
	"testing"
)

func TestReadDatasetKeepsRawFeatures(t *testing.T) {
	f, err := os.Open("../../testdata/corpus/mixed.fgb")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ds, err := ReadDataset(f, Options{IDColumn: "id"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Features) != 30000 {
		t.Fatalf("read %d features, want 30000", len(ds.Features))
	}
	if ds.IDColumn != "id" || ds.Header == nil {
		t.Fatalf("dataset metadata: id=%q header=%v", ds.IDColumn, ds.Header != nil)
	}
	for i, feature := range ds.Features {
		if len(feature.Raw) == 0 {
			t.Fatalf("feature %d has no raw bytes", i)
		}
		if feature.Properties["id"] == nil {
			t.Fatalf("feature %d has no id property", i)
		}
	}
}
