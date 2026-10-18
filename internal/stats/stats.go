package stats

import (
	"encoding/json"
	"hash/fnv"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
)

const (
	bloomBitsPerValue = 10
	bloomMinBits      = 64
	bloomProbes       = 7
)

type Row struct {
	Column string
	Min    *float64
	Max    *float64
	Nulls  int64
	Bloom  []byte
}

func CanonicalText(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case json.Number:
		return Number(string(x))
	case int:
		return strconv.FormatInt(int64(x), 10), true
	case int8:
		return strconv.FormatInt(int64(x), 10), true
	case int16:
		return strconv.FormatInt(int64(x), 10), true
	case int32:
		return strconv.FormatInt(int64(x), 10), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case uint:
		return strconv.FormatUint(uint64(x), 10), true
	case uint8:
		return strconv.FormatUint(uint64(x), 10), true
	case uint16:
		return strconv.FormatUint(uint64(x), 10), true
	case uint32:
		return strconv.FormatUint(uint64(x), 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	case float32:
		return decimal(float64(x), 32), true
	case float64:
		return decimal(x, 64), true
	}
	return "", false
}

func CanonicalTexts(v any) []string {
	if x, ok := v.([]any); ok {
		out := make([]string, 0, len(x))
		for _, element := range x {
			if text, ok := CanonicalText(element); ok {
				out = append(out, text)
			}
		}
		return out
	}
	if text, ok := CanonicalText(v); ok {
		return []string{text}
	}
	return nil
}

func Number(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	if !hasExponentOrPoint(text) {
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return strconv.FormatInt(n, 10), true
		}
		if u, err := strconv.ParseUint(text, 10, 64); err == nil {
			return strconv.FormatUint(u, 10), true
		}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(f) {
		return "", false
	}
	return decimal(f, 64), true
}

func Hashes(text string) (int64, int64) {
	h := fnv.New64a()
	h.Write([]byte(text))
	sum := h.Sum64()
	return int64(sum >> 32), int64(sum&0xffffffff) | 1
}

func Bloom(values map[string]struct{}) []byte {
	if len(values) == 0 {
		return nil
	}
	bits := len(values) * bloomBitsPerValue
	if bits < bloomMinBits {
		bits = bloomMinBits
	}
	out := make([]byte, (bits+7)/8)
	m := int64(len(out) * 8)
	for text := range values {
		set(out, m, text)
	}
	return out
}

func MayContain(bits []byte, text string) bool {
	m := int64(len(bits) * 8)
	if m == 0 {
		return false
	}
	h1, h2 := Hashes(text)
	for i := int64(0); i < bloomProbes; i++ {
		pos := (h1 + i*h2) % m
		if bits[pos/8]&(1<<(uint(pos)%8)) == 0 {
			return false
		}
	}
	return true
}

func Compute(cols []chunk.Column, features []map[string]any) []Row {
	rows := make([]Row, len(cols))
	for i := range cols {
		rows[i] = compute(&cols[i], features)
	}
	return rows
}

func compute(col *chunk.Column, features []map[string]any) Row {
	row := Row{Column: col.Name}
	bloomed := bloomColumn(col.Type)
	values := map[string]struct{}{}
	var lo, hi float64
	seen, invalid := false, false
	for _, props := range features {
		v, ok := props[col.Name]
		if !ok || v == nil {
			row.Nulls++
			continue
		}
		if bloomed {
			for _, text := range CanonicalTexts(v) {
				values[text] = struct{}{}
			}
			continue
		}
		if !numericColumn(col.Type) {
			continue
		}
		low, high, ok := bounds(col.Type, v)
		if !ok {
			invalid = true
			continue
		}
		if !seen {
			lo, hi, seen = low, high, true
			continue
		}
		lo = math.Min(lo, low)
		hi = math.Max(hi, high)
	}
	if bloomed {
		row.Bloom = Bloom(values)
	}
	if numericColumn(col.Type) && seen && !invalid {
		min, max := lo, hi
		row.Min, row.Max = &min, &max
	}
	return row
}

func bounds(t flattypes.ColumnType, v any) (float64, float64, bool) {
	switch t {
	case flattypes.ColumnTypeByte:
		if x, ok := v.(int8); ok {
			return intBounds(int64(x))
		}
	case flattypes.ColumnTypeUByte:
		if x, ok := v.(uint8); ok {
			return uintBounds(uint64(x))
		}
	case flattypes.ColumnTypeShort:
		if x, ok := v.(int16); ok {
			return intBounds(int64(x))
		}
	case flattypes.ColumnTypeUShort:
		if x, ok := v.(uint16); ok {
			return uintBounds(uint64(x))
		}
	case flattypes.ColumnTypeInt:
		if x, ok := v.(int32); ok {
			return intBounds(int64(x))
		}
	case flattypes.ColumnTypeUInt:
		if x, ok := v.(uint32); ok {
			return uintBounds(uint64(x))
		}
	case flattypes.ColumnTypeLong:
		if x, ok := v.(int64); ok {
			return intBounds(x)
		}
	case flattypes.ColumnTypeULong:
		if x, ok := v.(uint64); ok {
			return uintBounds(x)
		}
	case flattypes.ColumnTypeFloat:
		if x, ok := v.(float32); ok {
			f := float64(x)
			return f, f, true
		}
	case flattypes.ColumnTypeDouble:
		if x, ok := v.(float64); ok {
			return x, x, true
		}
	case flattypes.ColumnTypeDateTime:
		if x, ok := v.(string); ok {
			return timeBounds(x)
		}
	}
	return 0, 0, false
}

func intBounds(v int64) (float64, float64, bool) {
	if v >= -(1<<53) && v <= 1<<53 {
		f := float64(v)
		return f, f, true
	}
	return roundInt(v, big.ToNegativeInf), roundInt(v, big.ToPositiveInf), true
}

func uintBounds(v uint64) (float64, float64, bool) {
	if v <= 1<<53 {
		f := float64(v)
		return f, f, true
	}
	return roundUint(v, big.ToNegativeInf), roundUint(v, big.ToPositiveInf), true
}

func roundInt(v int64, mode big.RoundingMode) float64 {
	f := new(big.Float).SetPrec(64).SetInt64(v)
	f.SetMode(mode)
	f.SetPrec(53)
	out, _ := f.Float64()
	return out
}

func roundUint(v uint64, mode big.RoundingMode) float64 {
	f := new(big.Float).SetPrec(64).SetUint64(v)
	f.SetMode(mode)
	f.SetPrec(53)
	out, _ := f.Float64()
	return out
}

var timeLayouts = []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"}

func timeBounds(text string) (float64, float64, bool) {
	for _, layout := range timeLayouts {
		parsed, err := time.Parse(layout, text)
		if err != nil {
			continue
		}
		seconds := float64(parsed.UnixMicro()) / 1e6
		return math.Nextafter(seconds, math.Inf(-1)), math.Nextafter(seconds, math.Inf(1)), true
	}
	return 0, 0, false
}

func bloomColumn(t flattypes.ColumnType) bool {
	switch t {
	case flattypes.ColumnTypeString, flattypes.ColumnTypeBool, flattypes.ColumnTypeJson:
		return true
	}
	return false
}

func numericColumn(t flattypes.ColumnType) bool {
	switch t {
	case flattypes.ColumnTypeByte, flattypes.ColumnTypeUByte,
		flattypes.ColumnTypeShort, flattypes.ColumnTypeUShort,
		flattypes.ColumnTypeInt, flattypes.ColumnTypeUInt,
		flattypes.ColumnTypeLong, flattypes.ColumnTypeULong,
		flattypes.ColumnTypeFloat, flattypes.ColumnTypeDouble,
		flattypes.ColumnTypeDateTime:
		return true
	}
	return false
}

func set(bits []byte, m int64, text string) {
	h1, h2 := Hashes(text)
	for i := int64(0); i < bloomProbes; i++ {
		pos := (h1 + i*h2) % m
		bits[pos/8] |= 1 << (uint(pos) % 8)
	}
}

func decimal(f float64, bits int) string {
	if f == 0 {
		return "0"
	}
	return strconv.FormatFloat(f, 'f', -1, bits)
}

func hasExponentOrPoint(text string) bool {
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '.', 'e', 'E':
			return true
		}
	}
	return false
}
