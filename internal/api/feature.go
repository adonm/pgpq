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
	if info.Kind == "view" {
		for _, member := range s.targets(ctx, info) {
			if feature, ok, err := s.lookupFeature(ctx, member, fid); err != nil || ok {
				return feature, ok, err
			}
		}
		return nil, false, nil
	}
	if info.IDColumn != nil {
		values := []any{typedLiteral(fid)}
		if _, isString := values[0].(string); !isString {
			values = append(values, fid)
		}
		for _, value := range values {
			if feature, ok, err := s.featureByProperty(ctx, info, *info.IDColumn, value); err != nil || ok {
				return feature, ok, err
			}
		}
	}
	chunkID, ord, ok := parseSyntheticID(fid)
	if !ok {
		return nil, false, nil
	}
	feature, ok, err := s.chunkFeature(ctx, info, chunkID, ord)
	if ok {
		feature.ID = fid
	}
	return feature, ok, err
}

func (s *Server) featureByProperty(ctx context.Context, info store.CollectionInfo, column string, value any) (*chunk.Feature, bool, error) {
	match, ok, err := s.Store.FindFeature(ctx, info.Name, &cql.Compare{Prop: column, Op: "=", Value: value})
	if err != nil || !ok {
		return nil, false, err
	}
	feature, ok, err := s.chunkFeature(ctx, info, match.ChunkID, match.Ord)
	if ok {
		feature.ID = featureID(info, match.ChunkID, match.Ord, feature.Properties)
	}
	return feature, ok, err
}

func (s *Server) chunkFeature(ctx context.Context, info store.CollectionInfo, chunkID int64, ord int) (*chunk.Feature, bool, error) {
	row, err := s.Store.ChunkByID(ctx, info.Name, chunkID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil || row.Tier > 0 {
		return nil, false, err
	}
	c, features, err := s.openFeatures(ctx, info, chunkID, row)
	if err != nil || ord >= len(features) {
		return nil, false, err
	}
	return c.GeoJSON(features[ord]), true, nil
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
