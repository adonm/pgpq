package cover

import (
	"regexp"
	"strconv"
	"testing"
)

var rangeRe = regexp.MustCompile(`\[(-?\d+),(-?\d+)\]`)

func TestChunkFormat(t *testing.T) {
	literal := Chunk(-1, -1, 1, 1)
	if !regexp.MustCompile(`^\{(\[-?\d+,-?\d+\],?)+\}$`).MatchString(literal) {
		t.Fatalf("bad literal: %s", literal)
	}
}

func TestQueryInsideChunk(t *testing.T) {
	chunk := ranges(t, Chunk(-1, -1, 1, 1))
	query := ranges(t, Query(-0.1, -0.1, 0.1, 0.1))
	if len(query) == 0 {
		t.Fatal("empty query cover")
	}
	for _, q := range query {
		if !contains(chunk, q) {
			t.Fatalf("query range %v not contained in chunk cover %v", q, chunk)
		}
	}
}

func TestWorldChunkContainsRegion(t *testing.T) {
	chunk := ranges(t, Chunk(-180, -85, 180, 85))
	query := ranges(t, Query(-10, 45, 10, 55))
	if len(query) == 0 {
		t.Fatal("empty query cover")
	}
	for _, q := range query {
		if !contains(chunk, q) {
			t.Fatalf("query range %v not contained in world chunk cover", q)
		}
	}
}

func TestDisjointCovers(t *testing.T) {
	chunk := ranges(t, Chunk(-1, -1, 1, 1))
	far := ranges(t, Query(100, 50, 101, 51))
	for _, a := range chunk {
		for _, b := range far {
			if a.lo <= b.hi && b.lo <= a.hi {
				t.Fatalf("unexpected overlap %v %v", a, b)
			}
		}
	}
}

func contains(outer []interval, inner interval) bool {
	for _, o := range outer {
		if o.lo <= inner.lo && inner.hi <= o.hi {
			return true
		}
	}
	return false
}

func ranges(t *testing.T, literal string) []interval {
	t.Helper()
	var out []interval
	for _, m := range rangeRe.FindAllStringSubmatch(literal, -1) {
		lo, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		hi, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, interval{lo, hi})
	}
	return out
}
