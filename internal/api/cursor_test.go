package api

import "testing"

func TestCursorRoundTrip(t *testing.T) {
	in := cursor{
		uid:         1234567890123,
		version:     42,
		tier:        2,
		member:      3,
		chunkID:     9876543210,
		ord:         1234567,
		fingerprint: [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	}
	out, err := decodeCursor(encodeCursor(in))
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for _, value := range []string{"", "not-base64!!", "AAAA", encodeCursor(cursor{uid: 1}) + "x"} {
		if _, err := decodeCursor(value); err == nil {
			t.Fatalf("decodeCursor(%q) accepted an invalid cursor", value)
		}
	}
}
