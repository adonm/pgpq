package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/store"
)

func (s *Server) feature(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	info, err := s.Store.Collection(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "collection not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	feature, ok, err := s.lookupFeature(ctx, info, r.PathValue("featureID"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "feature not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/geo+json")
	json.NewEncoder(w).Encode(feature)
}

func (s *Server) lookupFeature(ctx context.Context, info store.CollectionInfo, fid string) (*chunk.Feature, bool, error) {
	if info.IDColumn != nil {
		values := []any{typedLiteral(fid)}
		if _, isString := values[0].(string); !isString {
			values = append(values, fid)
		}
		for _, value := range values {
			feature, ok, err := s.featureByProperty(ctx, info, *info.IDColumn, value)
			if err != nil || ok {
				return feature, ok, err
			}
		}
	}
	chunkID, ord, ok := parseSyntheticID(fid)
	if !ok {
		return nil, false, nil
	}
	row, err := s.Store.ChunkByID(ctx, info.Name, chunkID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	c, err := chunk.Open(row.Data)
	if err != nil {
		return nil, false, err
	}
	features, err := c.SearchAll()
	if err != nil {
		return nil, false, err
	}
	if ord >= len(features) {
		return nil, false, nil
	}
	feature := c.GeoJSON(features[ord])
	feature.ID = fid
	return feature, true, nil
}

func (s *Server) featureByProperty(ctx context.Context, info store.CollectionInfo, column string, value any) (*chunk.Feature, bool, error) {
	matches, err := s.Store.Matches(ctx, info.Name, nil, "", &cql.Compare{Prop: column, Op: "=", Value: value}, 1, 0)
	if err != nil {
		return nil, false, err
	}
	if len(matches) == 0 {
		return nil, false, nil
	}
	match := matches[0]
	row, err := s.Store.ChunkByID(ctx, info.Name, match.ChunkID)
	if err != nil {
		return nil, false, err
	}
	c, err := chunk.Open(row.Data)
	if err != nil {
		return nil, false, err
	}
	features, err := c.SearchAll()
	if err != nil {
		return nil, false, err
	}
	if match.Ord >= len(features) {
		return nil, false, nil
	}
	feature := c.GeoJSON(features[match.Ord])
	feature.ID = featureID(info, match.ChunkID, match.Ord, feature.Properties)
	return feature, true, nil
}

func parseSyntheticID(value string) (int64, int, bool) {
	i := strings.LastIndexByte(value, '-')
	if i <= 0 || i == len(value)-1 {
		return 0, 0, false
	}
	chunkID, err := strconv.ParseInt(value[:i], 10, 64)
	if err != nil || chunkID <= 0 {
		return 0, 0, false
	}
	ord, err := strconv.Atoi(value[i+1:])
	if err != nil || ord < 0 {
		return 0, 0, false
	}
	return chunkID, ord, true
}
