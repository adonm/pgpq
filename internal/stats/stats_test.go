package stats

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
)

func TestNumber(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"2", "2"},
		{"2.0", "2"},
		{"2.00", "2"},
		{"20e-1", "2"},
		{"-0", "0"},
		{"-0.0", "0"},
		{"0", "0"},
		{"0.0", "0"},
		{"0.1", "0.1"},
		{"007", "7"},
		{"1e3", "1000"},
		{"2.50", "2.5"},
		{"-9223372036854775808", "-9223372036854775808"},
		{"9223372036854775807", "9223372036854775807"},
		{"18446744073709551615", "18446744073709551615"},
	}
	for _, tc := range cases {
		got, ok := Number(tc.in)
		if !ok || got != tc.want {
			t.Errorf("Number(%q) = %q, %v; want %q", tc.in, got, ok, tc.want)
		}
	}
	if a, _ := Number("2"); a != "2" {
		t.Fatalf("2 = %q", a)
	}
	if b, _ := Number("2.0"); b != "2" {
		t.Fatalf("2.0 = %q", b)
	}
	if a, _ := Number("-0"); a != "0" {
		t.Fatalf("-0 = %q", a)
	}
	if b, _ := Number("0"); b != "0" {
		t.Fatalf("0 = %q", b)
	}
}

func TestCanonicalText(t *testing.T) {
	max := int64(math.MaxInt64)
	if got, ok := CanonicalText(max); !ok || got != "9223372036854775807" {
		t.Fatalf("int64 max = %q, %v", got, ok)
	}
	if got, ok := CanonicalText(int64(math.MinInt64)); !ok || got != "-9223372036854775808" {
		t.Fatalf("int64 min = %q, %v", got, ok)
	}
	if got, ok := CanonicalText(float64(2) * 1); !ok || got != "2" {
		t.Fatalf("float 2 = %q, %v", got, ok)
	}
	if got, ok := CanonicalText(float32(0.1)); !ok || got != "0.1" {
		t.Fatalf("float32 0.1 = %q, %v", got, ok)
	}
	if got, ok := CanonicalText(0.1); !ok || got != "0.1" {
		t.Fatalf("float64 0.1 = %q, %v", got, ok)
	}
	if got, ok := CanonicalText(math.Copysign(0, -1)); !ok || got != "0" {
		t.Fatalf("negative zero = %q, %v", got, ok)
	}
	if got, ok := CanonicalText("hello"); !ok || got != "hello" {
		t.Fatalf("string = %q, %v", got, ok)
	}
	if got, ok := CanonicalText(true); !ok || got != "true" {
		t.Fatalf("bool = %q, %v", got, ok)
	}
	if _, ok := CanonicalText(nil); ok {
		t.Fatal("nil produced a canonical text")
	}
}

func TestCanonicalTexts(t *testing.T) {
	got := CanonicalTexts([]any{"a", 1.0, true, nil, []any{2}})
	want := []string{"a", "1", "true"}
	if len(got) != len(want) {
		t.Fatalf("array members = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("array members = %v, want %v", got, want)
		}
	}
	if single := CanonicalTexts("solo"); len(single) != 1 || single[0] != "solo" {
		t.Fatalf("scalar = %v", single)
	}
	if object := CanonicalTexts(map[string]any{"a": 1.0}); object != nil {
		t.Fatalf("object produced member texts %v", object)
	}
}

func TestJsonObjectHasNoBloom(t *testing.T) {
	cols := []chunk.Column{{Name: "tags", Type: flattypes.ColumnTypeJson}}
	rows := Compute(cols, []map[string]any{{"tags": map[string]any{"a": 1.0}}})
	if rows[0].Bloom != nil {
		t.Fatal("an object-valued json column produced a bloom")
	}
	cols = []chunk.Column{{Name: "tags", Type: flattypes.ColumnTypeJson}}
	rows = Compute(cols, []map[string]any{{"tags": []any{1.0, 3.0}}})
	if rows[0].Bloom == nil || !MayContain(rows[0].Bloom, "1") || !MayContain(rows[0].Bloom, "3") {
		t.Fatal("an array-valued json column lost its members")
	}
}

func TestBloomNoFalseNegatives(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		size := rng.IntN(50)
		values := map[string]struct{}{}
		for range size {
			values[fmt.Sprintf("value-%d-%d", rng.IntN(1<<30), rng.IntN(1<<20))] = struct{}{}
		}
		bits := Bloom(values)
		if size == 0 {
			if bits != nil {
				t.Fatal("empty set produced a bloom")
			}
			continue
		}
		bitsWanted := size * bloomBitsPerValue
		if bitsWanted < bloomMinBits {
			bitsWanted = bloomMinBits
		}
		if len(bits) != (bitsWanted+7)/8 {
			t.Fatalf("size %d bloom is %d bytes, want %d", size, len(bits), (bitsWanted+7)/8)
		}
		for text := range values {
			if !MayContain(bits, text) {
				t.Fatalf("bloom of %d values is missing %q", size, text)
			}
		}
	}
	if len(Bloom(map[string]struct{}{"solo": {}})) != bloomMinBits/8 {
		t.Fatal("single value bloom is not the minimum size")
	}
}

func TestHashesOdd(t *testing.T) {
	for _, text := range []string{"", "a", "hello", "número", "0.1"} {
		h1, h2 := Hashes(text)
		if h1 < 0 || h2 <= 0 || h2%2 != 1 {
			t.Fatalf("Hashes(%q) = %d, %d; want non-negative h1 and odd positive h2", text, h1, h2)
		}
	}
}

func TestOutwardBounds(t *testing.T) {
	col := []chunk.Column{{Name: "n", Type: flattypes.ColumnTypeLong}}
	rows := Compute(col, []map[string]any{{"n": int64(math.MaxInt64)}})
	if len(rows) != 1 || rows[0].Min == nil || rows[0].Max == nil {
		t.Fatalf("long stats = %+v", rows)
	}
	if *rows[0].Min != 9223372036854774784 || *rows[0].Max != 9223372036854775808 {
		t.Fatalf("max int64 bounds = [%v, %v]", *rows[0].Min, *rows[0].Max)
	}
	if *rows[0].Min > float64(math.MaxInt64) || *rows[0].Max < float64(math.MaxInt64) {
		t.Fatalf("max int64 falls outside [%v, %v]", *rows[0].Min, *rows[0].Max)
	}

	ulong := []chunk.Column{{Name: "n", Type: flattypes.ColumnTypeULong}}
	rows = Compute(ulong, []map[string]any{{"n": uint64(math.MaxUint64)}})
	if *rows[0].Min > float64(uint64(math.MaxUint64)) || *rows[0].Max < float64(uint64(math.MaxUint64)) {
		t.Fatalf("max uint64 falls outside [%v, %v]", *rows[0].Min, *rows[0].Max)
	}

	small := []chunk.Column{{Name: "n", Type: flattypes.ColumnTypeInt}}
	rows = Compute(small, []map[string]any{{"n": int32(-7)}, {"n": int32(400)}})
	if *rows[0].Min != -7 || *rows[0].Max != 400 {
		t.Fatalf("small int bounds = [%v, %v]", *rows[0].Min, *rows[0].Max)
	}
}

func TestNullCounts(t *testing.T) {
	cols := []chunk.Column{
		{Name: "a", Type: flattypes.ColumnTypeString},
		{Name: "b", Type: flattypes.ColumnTypeInt},
	}
	rows := Compute(cols, []map[string]any{
		{"a": "x", "b": int32(1)},
		{"b": int32(2)},
		{"a": nil},
	})
	if rows[0].Nulls != 2 {
		t.Fatalf("string nulls = %d, want 2", rows[0].Nulls)
	}
	if rows[1].Nulls != 1 {
		t.Fatalf("int nulls = %d, want 1", rows[1].Nulls)
	}
	if rows[0].Bloom == nil || !MayContain(rows[0].Bloom, "x") {
		t.Fatal("bloom lost the only present string")
	}
}
