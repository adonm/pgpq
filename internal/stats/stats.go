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
