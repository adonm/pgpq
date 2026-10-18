package cql

import (
	"strings"
	"testing"
	"time"
)

func chunkOf(t *testing.T, text string, casts Casts) (string, []any) {
	t.Helper()
	expr, err := Parse(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	b := NewBuilder("ch", casts)
	return ChunkSQL(expr, b), b.Args()
}

func TestChunkSQLPrunesConservatively(t *testing.T) {
	cases := []struct {
		text   string
		casts  Casts
		has    []string
		absent []string
	}{
		{`id = 5`, Casts{"id": CastNumeric}, []string{`"column" = $1`, "s.min <= ", "s.max", "not exists"}, nil},
		{`name = 'x'`, Casts{"name": CastText}, []string{"bloom_has(s.bloom,"}, []string{"s.min <= "}},
		{`n < 10`, Casts{"n": CastNumeric}, []string{"s.min is null or s.min <"}, nil},
		{`n >= 10`, Casts{"n": CastNumeric}, []string{"s.max is null or s.max >="}, nil},
		{`n BETWEEN 1 AND 5`, Casts{"n": CastNumeric}, []string{"s.max is null or s.max >=", "s.min is null or s.min <="}, nil},
		{`x IS NULL`, nil, []string{"s.nulls > 0"}, nil},
		{`x IS NOT NULL`, nil, []string{"true"}, []string{"s.nulls"}},
		{`x LIKE 'a%'`, nil, []string{"true"}, []string{"bloom_has"}},
		{`NOT (x = 1)`, nil, []string{"true"}, []string{"bloom_has"}},
		{`n <> 5`, Casts{"n": CastNumeric}, []string{"true"}, []string{"s.min"}},
		{`n = 1 AND name = 'a'`, nil, []string{" and ", "bloom_has(s.bloom,"}, nil},
		{`n = 1 OR name = 'a'`, nil, []string{" or ", "bloom_has(s.bloom,"}, nil},
	}
	for _, tc := range cases {
		sql, _ := chunkOf(t, tc.text, tc.casts)
		for _, want := range tc.has {
			if !strings.Contains(sql, want) {
				t.Errorf("%s: missing %q in %s", tc.text, want, sql)
			}
		}
		for _, want := range tc.absent {
			if strings.Contains(sql, want) {
				t.Errorf("%s: unexpected %q in %s", tc.text, want, sql)
			}
		}
	}
}

func TestChunkSQLUsesStatsHashes(t *testing.T) {
	sql, args := chunkOf(t, `name = 'cat_07'`, Casts{"name": CastText})
	if !strings.Contains(sql, "bloom_has(s.bloom, $2, $3)") {
		t.Fatalf("equality bloom probe = %s %v", sql, args)
	}
	if args[0] != "name" {
		t.Fatalf("first arg = %#v, want the column name", args[0])
	}
	if _, ok := args[1].(int64); !ok {
		t.Fatalf("hash arg = %#v, want int64", args[1])
	}
}

func TestChunkSQLTimePrunesWithEpoch(t *testing.T) {
	sql, args := chunkOf(t, `c_date > '2026-01-01T00:00:00Z'`, Casts{"c_date": CastNone})
	if !strings.Contains(sql, "s.max is null or s.max >") {
		t.Fatalf("time comparison = %s", sql)
	}
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	if len(args) != 2 {
		t.Fatalf("args = %v", args)
	}
	if got, ok := args[1].(float64); !ok || got != float64(epoch) {
		t.Fatalf("time bound = %#v, want %v", args[1], float64(epoch))
	}
}
