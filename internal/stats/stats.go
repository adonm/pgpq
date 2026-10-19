package stats

import (
	"encoding/json"
	"math"
	"strconv"
)

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
