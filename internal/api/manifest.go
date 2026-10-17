package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/adonm/postfgb/internal/store"
)

type manifestChunk struct {
	ID           int64           `json:"id"`
	BBox         []float64       `json:"bbox"`
	FeatureCount int             `json:"featureCount"`
	SizeBytes    int             `json:"sizeBytes"`
	Checksum     string          `json:"checksum,omitempty"`
	Columns      json.RawMessage `json:"columns,omitempty"`
	Href         string          `json:"href"`
}

type manifestOutput struct {
	Body struct {
		Collection   string          `json:"collection"`
		ChunkCount   int             `json:"chunkCount"`
		FeatureCount int64           `json:"featureCount"`
		Extent       []float64       `json:"extent,omitempty"`
		Chunks       []manifestChunk `json:"chunks"`
	}
}

type manifestInput struct {
	ID            string `path:"id"`
	Authorization string `header:"Authorization"`
}

func (s *Server) manifest(ctx context.Context, in *manifestInput) (*manifestOutput, error) {
	if !s.adminOK(in.Authorization) {
		return nil, huma.Error401Unauthorized("unauthorized")
	}
	info, err := s.Store.Collection(ctx, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("collection not found")
	}
	if err != nil {
		return nil, err
	}
	metas, err := s.Store.ChunkMetas(ctx, in.ID, nil, 1000000, 0)
	if err != nil {
		return nil, err
	}
	out := &manifestOutput{}
	out.Body.Collection = info.Name
	out.Body.ChunkCount = len(metas)
	out.Body.FeatureCount = info.FeatureCount
	if info.MinX != nil && info.MinY != nil && info.MaxX != nil && info.MaxY != nil {
		out.Body.Extent = []float64{*info.MinX, *info.MinY, *info.MaxX, *info.MaxY}
	}
	for _, meta := range metas {
		chunk := manifestChunk{
			ID:           meta.ID,
			BBox:         []float64{meta.MinX, meta.MinY, meta.MaxX, meta.MaxY},
			FeatureCount: meta.FeatureCount,
			SizeBytes:    meta.SizeBytes,
			Href:         "/collections/" + in.ID + "/chunks/" + strconv.FormatInt(meta.ID, 10),
		}
		if meta.Checksum != nil {
			chunk.Checksum = *meta.Checksum
		}
		if len(meta.Columns) > 0 {
			chunk.Columns = json.RawMessage(meta.Columns)
		}
		out.Body.Chunks = append(out.Body.Chunks, chunk)
	}
	return out, nil
}
