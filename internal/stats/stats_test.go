package stats

import (
	"encoding/json"
	"testing"
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
	if got, ok := CanonicalText(json.Number("9223372036854775807")); !ok || got != "9223372036854775807" {
		t.Fatalf("int64 max = %q, %v", got, ok)
	}
	if got, ok := CanonicalText(json.Number("-9223372036854775808")); !ok || got != "-9223372036854775808" {
		t.Fatalf("int64 min = %q, %v", got, ok)
	}
	if got, ok := CanonicalText(json.Number("2.50")); !ok || got != "2.5" {
		t.Fatalf("decimal = %q, %v", got, ok)
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
