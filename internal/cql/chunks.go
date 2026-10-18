package cql

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/adonm/postfgb/internal/stats"
)

func ChunkSQL(e Expr, b *Builder) string {
	switch x := e.(type) {
	case *Compare:
		value := coerce(x.Value, b.Casts[x.Prop])
		if x.Op == "=" {
			return equalityChunk(b, x.Prop, value)
		}
		return orderedChunk(b, x.Prop, x.Op, value)
	case *Between:
		if x.Not {
			return "true"
		}
		lo := coerce(x.Lo, b.Casts[x.Prop])
		hi := coerce(x.Hi, b.Casts[x.Prop])
		return "(" + orderedChunk(b, x.Prop, ">=", lo) + " and " + orderedChunk(b, x.Prop, "<=", hi) + ")"
	case *In:
		if x.Not {
			return "true"
		}
		parts := make([]string, len(x.Values))
		for i, v := range x.Values {
			parts[i] = equalityChunk(b, x.Prop, coerce(v, b.Casts[x.Prop]))
		}
		return "(" + strings.Join(parts, " or ") + ")"
	case *IsNull:
		if x.Not {
			return "true"
		}
		return chunkLeaf(b.Bind(x.Prop), "s.nulls > 0")
	case andExpr:
		return "(" + ChunkSQL(x.left, b) + " and " + ChunkSQL(x.right, b) + ")"
	case orExpr:
		return "(" + ChunkSQL(x.left, b) + " or " + ChunkSQL(x.right, b) + ")"
	}
	return "true"
}

func equalityChunk(b *Builder, prop string, value any) string {
	key := b.Bind(prop)
	var parts []string
	if texts := probeTexts(value); len(texts) > 0 {
		or := make([]string, 0, len(texts))
		for _, text := range texts {
			h1, h2 := stats.Hashes(text)
			or = append(or, "bloom_has(s.bloom, "+b.Bind(h1)+", "+b.Bind(h2)+")")
		}
		parts = append(parts, "(s.bloom is null or "+strings.Join(or, " or ")+")")
	}
	if number, ok := asNumber(value); ok {
		v := b.Bind(number)
		parts = append(parts, "(s.min is null or s.max is null or (s.min <= "+v+" and "+v+" <= s.max))")
	}
	if len(parts) == 0 {
		return "true"
	}
	return chunkLeaf(key, strings.Join(parts, " and "))
}

func orderedChunk(b *Builder, prop, op string, value any) string {
	if op == "<>" {
		return "true"
	}
	if number, ok := asNumber(value); ok {
		return chunkLeaf(b.Bind(prop), rangeTest(op, b.Bind(number)))
	}
	if t, ok := value.(time.Time); ok {
		return chunkLeaf(b.Bind(prop), rangeTest(op, b.Bind(epochSeconds(t))))
	}
	if text, ok := value.(string); ok {
		if t, ok := ParseTime(text); ok {
			return chunkLeaf(b.Bind(prop), rangeTest(op, b.Bind(epochSeconds(t))))
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
		return []string{x}
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
	if !isNumber(value) {
		return 0, false
	}
	return floatValue(value)
}
