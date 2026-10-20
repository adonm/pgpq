package fgb_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
)

func TestReaderSkipsTheIndex(t *testing.T) {
	cols := []chunk.Column{{Name: "id", Type: flattypes.ColumnTypeString}}
	features := make([]chunk.InputFeature, 5)
	for i := range features {
		props := map[string]json.RawMessage{"id": json.RawMessage(strconv.Quote(strconv.Itoa(i)))}
		features[i] = testPoint(cols, props, float64(i), 0)
	}
	body, err := chunk.EncodePage("reader", testHeader(cols), features)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := fgb.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if reader.Header().FeaturesCount() != 5 {
		t.Fatalf("header features = %d", reader.Header().FeaturesCount())
	}
	count := 0
	for {
		if _, err := reader.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 5 {
		t.Fatalf("read %d features, want 5", count)
	}
}
