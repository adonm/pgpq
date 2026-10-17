package api

import (
	"context"
	"encoding/json"
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
	casts, err := s.columnCasts(ctx, info)
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

func (s *Server) columnCasts(ctx context.Context, info store.CollectionInfo) (map[string]string, error) {
	casts := map[string]string{}
	metas, err := s.Store.ChunkMetas(ctx, info.Name, nil, 1, 0, 0)
	if err != nil {
		return casts, err
	}
	if len(metas) == 0 || len(metas[0].Columns) == 0 {
		return casts, nil
	}
	var cols []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(metas[0].Columns, &cols); err != nil {
		return casts, nil
	}
	for _, col := range cols {
		casts[col.Name] = sortCast(col.Type)
	}
	return casts, nil
}

func sortCast(columnType string) string {
	switch columnType {
	case "Bool":
		return "boolean"
	case "Byte", "UByte", "Short", "UShort", "Int", "UInt", "Long", "ULong", "Float", "Double":
		return "numeric"
	default:
		return ""
	}
}
