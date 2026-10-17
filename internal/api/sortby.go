package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/adonm/postfgb/internal/store"
)

func (s *Server) sortOrder(ctx context.Context, r *http.Request, info store.CollectionInfo) ([]string, error) {
	value := r.URL.Query().Get("sortby")
	if value == "" {
		return nil, nil
	}
	if len(info.Sortable) == 0 {
		return nil, errors.New("collection has no sortable columns")
	}
	casts, err := s.Store.ColumnCasts(ctx, info.Name)
	if err != nil {
		return nil, err
	}
	var order []string
	for _, key := range strings.Split(value, ",") {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		desc := false
		switch key[0] {
		case '+':
			key = key[1:]
		case '-':
			key = key[1:]
			desc = true
		}
		if !declaredSortable(info.Sortable, key) {
			return nil, fmt.Errorf("sortby column %q is not declared sortable", key)
		}
		expr := store.SortExpr(key, casts[key])
		if expr == "" {
			return nil, fmt.Errorf("sortby column %q has an unsupported type", key)
		}
		if desc {
			expr += " desc"
		}
		order = append(order, expr)
	}
	if len(order) == 0 {
		return nil, errors.New("empty sortby")
	}
	return order, nil
}

func declaredSortable(sortable []string, name string) bool {
	for _, column := range sortable {
		if column == name {
			return true
		}
	}
	return false
}
