package layer

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

type Cursor struct {
	UID      int64
	Snapshot int64
	Level    int
	Key      uint64
	ID       string
}

func (c Cursor) String() string {
	text := strconv.FormatInt(c.UID, 10) + "." + strconv.FormatInt(c.Snapshot, 10) + "." + strconv.Itoa(c.Level) + "." + strconv.FormatUint(c.Key, 10) + "." + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(text))
}

func DecodeCursor(value string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return Cursor{}, ErrBadCursor
	}
	parts := strings.SplitN(string(raw), ".", 5)
	if len(parts) != 5 || parts[4] == "" {
		return Cursor{}, ErrBadCursor
	}
	uid, err1 := strconv.ParseInt(parts[0], 10, 64)
	snapshot, err2 := strconv.ParseInt(parts[1], 10, 64)
	level, err3 := strconv.Atoi(parts[2])
	key, err4 := strconv.ParseUint(parts[3], 10, 64)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return Cursor{}, ErrBadCursor
	}
	return Cursor{UID: uid, Snapshot: snapshot, Level: level, Key: key, ID: parts[4]}, nil
}
