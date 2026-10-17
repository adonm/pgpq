package cql

import "testing"

func TestRename(t *testing.T) {
	expr, err := Parse(`a = 1 and (b like 'x%' or c is null)`)
	if err != nil {
		t.Fatal(err)
	}
	renamed := Rename(expr, func(prop string) string {
		if prop == "b" {
			return "bb"
		}
		return prop
	})
	var args []any
	renamed.SQL("props", &args)
	found := false
	for _, arg := range args {
		if arg == "bb" {
			found = true
		}
		if arg == "b" {
			t.Fatalf("unrenamed property in args: %v", args)
		}
	}
	if !found {
		t.Fatalf("renamed property missing from args: %v", args)
	}
}
