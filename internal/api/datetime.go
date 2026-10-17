package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/adonm/postfgb/internal/cql"
)

type timeWindow struct {
	lo, hi time.Time
	set    bool
}

func parseTimeWindow(r *http.Request) (timeWindow, error) {
	value := r.URL.Query().Get("datetime")
	if value == "" {
		return timeWindow{}, nil
	}
	lo, hi, err := parseDatetime(value)
	if err != nil {
		return timeWindow{}, err
	}
	return timeWindow{lo: lo, hi: hi, set: true}, nil
}

func datetimeExpr(col string, tw timeWindow) cql.Expr {
	var expr cql.Expr
	if !tw.lo.IsZero() {
		expr = cql.And(expr, &cql.Compare{Prop: col, Op: ">=", Value: tw.lo})
	}
	if !tw.hi.IsZero() {
		expr = cql.And(expr, &cql.Compare{Prop: col, Op: "<=", Value: tw.hi})
	}
	return expr
}

func parseDatetime(value string) (time.Time, time.Time, error) {
	parts := strings.Split(value, "/")
	if len(parts) == 0 || len(parts) > 2 {
		return time.Time{}, time.Time{}, errors.New("invalid datetime")
	}
	if len(parts) == 1 {
		t, err := parseInstant(parts[0])
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		return t, t, nil
	}
	var lo, hi time.Time
	var err error
	if parts[0] != ".." {
		if lo, err = parseInstant(parts[0]); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if parts[1] != ".." {
		if hi, err = parseInstant(parts[1]); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if lo.IsZero() && hi.IsZero() {
		return time.Time{}, time.Time{}, errors.New("datetime interval needs an endpoint")
	}
	if !lo.IsZero() && !hi.IsZero() && hi.Before(lo) {
		return time.Time{}, time.Time{}, errors.New("datetime interval is inverted")
	}
	return lo, hi, nil
}

func parseInstant(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("invalid datetime " + value)
}
