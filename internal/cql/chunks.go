package cql

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/adonm/postfgb/internal/stats"
)

func ChunkSQL(e Expr, bind func(any) string) string {
	switch x := e.(type) {
	case *Compare:
		if x.Op == "=" {
			return equalityChunk(bind, x.Prop, x.Value)
		}
		return orderedChunk(bind, x.Prop, x.Op, x.Value)
	case *Between:
		if x.Not {
			return "true"
		}
		return "(" + orderedChunk(bind, x.Prop, ">=", x.Lo) + " and " + orderedChunk(bind, x.Prop, "<=", x.Hi) + ")"
	case *In:
		if x.Not {
			return "true"
		}
		parts := make([]string, len(x.Values))
		for i, v := range x.Values {
			parts[i] = equalityChunk(bind, x.Prop, v)
		}
		return "(" + strings.Join(parts, " or ") + ")"
	case *IsNull:
		if x.Not {
			return "true"
		}
		return chunkLeaf(bind(x.Prop), "s.nulls > 0")
	case andExpr:
		return "(" + ChunkSQL(x.left, bind) + " and " + ChunkSQL(x.right, bind) + ")"
	case orExpr:
		return "(" + ChunkSQL(x.left, bind) + " or " + ChunkSQL(x.right, bind) + ")"
	}
	return "true"
}

func equalityChunk(bind func(any) string, prop string, value any) string {
	key := bind(prop)
	var parts []string
	if texts := probeTexts(value); len(texts) > 0 {
		or := make([]string, 0, len(texts))
		for _, text := range texts {
			h1, h2 := stats.Hashes(text)
			or = append(or, "bloom_has(s.bloom, "+bind(h1)+", "+bind(h2)+")")
		}
		parts = append(parts, "(s.bloom is null or "+strings.Join(or, " or ")+")")
	}
	if number, ok := asNumber(value); ok {
		v := bind(number)
		parts = append(parts, "(s.min is null or s.max is null or (s.min <= "+v+" and "+v+" <= s.max))")
	}
	if len(parts) == 0 {
		return "true"
	}
	return chunkLeaf(key, strings.Join(parts, " and "))
}

func orderedChunk(bind func(any) string, prop, op string, value any) string {
	if op == "<>" {
		return "true"
	}
	if number, ok := numericValue(value); ok {
		return chunkLeaf(bind(prop), rangeTest(op, bind(number)))
	}
	if t, ok := value.(time.Time); ok {
		return chunkLeaf(bind(prop), rangeTest(op, bind(epochSeconds(t))))
	}
	if text, ok := value.(string); ok {
		if t, ok := ParseTime(text); ok {
			return chunkLeaf(bind(prop), rangeTest(op, bind(epochSeconds(t))))
		}
	}
	return "true"
}

func rangeTest(op, v string) string {
	switch op {
	case "<":
		return "s.min is null or s.min < " + v
	case "<=":
		return "s.min is null or s.min <= " + v
	case ">":
		return "s.max is null or s.max > " + v
	case ">=":
		return "s.max is null or s.max >= " + v
	}
	return "true"
}

func epochSeconds(t time.Time) float64 {
	return float64(t.UnixMicro()) / 1e6
}

func chunkLeaf(key, test string) string {
	return fmt.Sprintf(`(not exists (select 1 from chunk_stats s where s.collection = ch.collection and s.chunk_id = ch.id and s."column" = %[1]s) or exists (select 1 from chunk_stats s where s.collection = ch.collection and s.chunk_id = ch.id and s."column" = %[1]s and (%[2]s)))`, key, test)
}

func probeTexts(value any) []string {
	switch x := value.(type) {
	case bool:
		return []string{fmt.Sprint(x)}
	case string:
		texts := []string{x}
		if text, ok := stats.Number(x); ok && !slices.Contains(texts, text) {
			texts = append(texts, text)
		}
		return texts
	}
	if !isNumber(value) {
		return nil
	}
	texts := make([]string, 0, 2)
	if text, ok := stats.CanonicalText(value); ok {
		texts = append(texts, text)
	}
	if text := fmt.Sprint(value); !slices.Contains(texts, text) {
		texts = append(texts, text)
	}
	return texts
}

func asNumber(value any) (float64, bool) {
	if text, ok := value.(string); ok {
		n, ok := Number(text)
		if !ok {
			return 0, false
		}
		return floatValue(n)
	}
	return numericValue(value)
}

func numericValue(value any) (float64, bool) {
	if !isNumber(value) {
		return 0, false
	}
	return floatValue(value)
}
