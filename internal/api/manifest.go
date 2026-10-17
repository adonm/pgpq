package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/adonm/postfgb/internal/store"
)

type manifestChunk struct {
	Collection   string          `json:"collection,omitempty"`
	ID           int64           `json:"id"`
	BBox         []float64       `json:"bbox"`
	FeatureCount int             `json:"featureCount"`
	SizeBytes    int             `json:"sizeBytes"`
	Checksum     string          `json:"checksum,omitempty"`
	Columns      json.RawMessage `json:"columns,omitempty"`
	Href         string          `json:"href"`
}

type manifestOutput struct {
	ETag string `header:"ETag"`
	Body struct {
		Collection   string          `json:"collection"`
		ChunkCount   int             `json:"chunkCount"`
		FeatureCount int64           `json:"featureCount"`
		Extent       []float64       `json:"extent,omitempty"`
		Chunks       []manifestChunk `json:"chunks"`
	}
}

type manifestInput struct {
	ID          string `path:"id"`
	IfNoneMatch string `header:"If-None-Match"`
}

func (s *Server) manifest(ctx context.Context, in *manifestInput) (*manifestOutput, error) {
	info, err := s.Store.Collection(ctx, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("collection not found")
	}
	if info.Kind == "view" {
		return nil, huma.Error400BadRequest("a view has no chunk manifest: read /items.fgb or its members")
	}
	if err != nil {
		return nil, err
	}
	etag := `"` + strconv.FormatInt(info.Version, 10) + `"`
	if in.IfNoneMatch == etag {
		return nil, huma.ErrorWithHeaders(huma.Status304NotModified(), http.Header{"ETag": {etag}})
	}
	out := &manifestOutput{ETag: etag}
	out.Body.Collection = info.Name
	metas, err := s.Store.ChunkMetas(ctx, in.ID, nil, 1000000, 0, 0)
	if err != nil {
		return nil, err
	}
	out.Body.ChunkCount = len(metas)
	out.Body.FeatureCount = info.FeatureCount
	if info.MinX != nil && info.MinY != nil && info.MaxX != nil && info.MaxY != nil {
		out.Body.Extent = []float64{*info.MinX, *info.MinY, *info.MaxX, *info.MaxY}
	}
	for _, meta := range metas {
		out.Body.Chunks = append(out.Body.Chunks, manifestChunkOf(in.ID, meta))
	}
	return out, nil
}

func manifestChunkOf(collection string, meta store.ChunkMeta) manifestChunk {
	chunk := manifestChunk{
		Collection:   collection,
		ID:           meta.ID,
		BBox:         []float64{meta.MinX, meta.MinY, meta.MaxX, meta.MaxY},
		FeatureCount: meta.FeatureCount,
		SizeBytes:    meta.SizeBytes,
		Href:         "/collections/" + collection + "/chunks/" + strconv.FormatInt(meta.ID, 10),
	}
	if meta.Checksum != nil {
		chunk.Checksum = *meta.Checksum
	}
	if len(meta.Columns) > 0 {
		chunk.Columns = json.RawMessage(meta.Columns)
	}
	return chunk
}
