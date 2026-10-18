package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"sort"
	"strings"
)

const cursorVersion = 1

type cursor struct {
	uid         int64
	version     int64
	tier        int
	member      int
	chunkID     int64
	ord         int
	fingerprint [16]byte
}

func encodeCursor(c cursor) string {
	raw := make([]byte, 0, 1+8+8+2+2+8+4+16)
	raw = append(raw, cursorVersion)
	raw = binary.BigEndian.AppendUint64(raw, uint64(c.uid))
	raw = binary.BigEndian.AppendUint64(raw, uint64(c.version))
	raw = binary.BigEndian.AppendUint16(raw, uint16(c.tier))
	raw = binary.BigEndian.AppendUint16(raw, uint16(c.member))
	raw = binary.BigEndian.AppendUint64(raw, uint64(c.chunkID))
	raw = binary.BigEndian.AppendUint32(raw, uint32(c.ord))
	raw = append(raw, c.fingerprint[:]...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(value string) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != 1+8+8+2+2+8+4+16 {
		return cursor{}, errors.New("invalid cursor")
	}
	if raw[0] != cursorVersion {
		return cursor{}, errors.New("invalid cursor")
	}
	var c cursor
	c.uid = int64(binary.BigEndian.Uint64(raw[1:]))
	c.version = int64(binary.BigEndian.Uint64(raw[9:]))
	c.tier = int(binary.BigEndian.Uint16(raw[17:]))
	c.member = int(binary.BigEndian.Uint16(raw[19:]))
	c.chunkID = int64(binary.BigEndian.Uint64(raw[21:]))
	c.ord = int(int32(binary.BigEndian.Uint32(raw[29:])))
	copy(c.fingerprint[:], raw[33:])
	return c, nil
}

func requestFingerprint(r *http.Request) [16]byte {
	query := r.URL.Query()
	parts := make([]string, 0, 8)
	for _, name := range []string{"bbox", "datetime", "filter", "filter-lang"} {
		if value := query.Get(name); value != "" {
			parts = append(parts, name+"="+value)
		}
	}
	props := parseFilters(r)
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		parts = append(parts, name+"="+props[name])
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	var fingerprint [16]byte
	copy(fingerprint[:], sum[:])
	return fingerprint
}
