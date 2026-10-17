package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/geo"
	"github.com/adonm/postfgb/internal/store"
)

const sortedCacheBudget = 32 << 20

type chunkEntry struct {
	chunk *chunk.Chunk
	feats []*flattypes.Feature
	size  int
}

type chunkCache struct {
	store      *store.Store
	collection string
	entries    map[int64]*chunkEntry
	order      []int64
	bytes      int
}

func newChunkCache(st *store.Store, collection string) *chunkCache {
	return &chunkCache{store: st, collection: collection, entries: map[int64]*chunkEntry{}}
}

func (c *chunkCache) get(ctx context.Context, id int64) (*chunkEntry, error) {
	if entry, ok := c.entries[id]; ok {
		return entry, nil
	}
	row, err := c.store.ChunkByID(ctx, c.collection, id)
	if err != nil {
		return nil, err
	}
	ch, err := chunk.Open(row.Data)
	if err != nil {
		return nil, err
	}
	feats, err := ch.SearchAll()
	if err != nil {
		return nil, err
	}
	entry := &chunkEntry{chunk: ch, feats: feats, size: len(ch.Data)}
	c.entries[id] = entry
	c.order = append(c.order, id)
	c.bytes += entry.size
	for c.bytes > sortedCacheBudget && len(c.order) > 1 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if evicted, ok := c.entries[oldest]; ok {
			c.bytes -= evicted.size
			delete(c.entries, oldest)
		}
	}
	return entry, nil
}

func (s *Server) streamSorted(ctx context.Context, sink featureSink, info store.CollectionInfo, bbox *[4]float64, query string, filter cql.Expr, order []string, offset int) {
	matches, err := s.Store.Sorted(ctx, info.Name, bbox, query, filter, order, sink.remaining(), offset)
	if err != nil {
		slog.Error("sorted query failed", "collection", info.Name, "error", err)
		return
	}
	cache := newChunkCache(s.Store, info.Name)
	for _, m := range matches {
		entry, err := cache.get(ctx, m.ChunkID)
		if err != nil {
			slog.Error("chunk load failed", "chunk", m.ChunkID, "error", err)
			continue
		}
		if m.Ord >= len(entry.feats) {
			continue
		}
		g := entry.chunk.GeoJSON(entry.feats[m.Ord])
		if !inBounds(g, bbox) {
			continue
		}
		g.ID = featureID(info, m.ChunkID, m.Ord, g.Properties)
		if !sink.add(g) {
			return
		}
	}
}

func (s *Server) collectSorted(ctx context.Context, id string, bbox *[4]float64, query string, filter cql.Expr, order []string, shapes []geo.Shape, offset, limit int) ([]*flattypes.Feature, *flattypes.Header, error) {
	matches, err := s.Store.Sorted(ctx, id, bbox, query, filter, order, limit, offset)
	if err != nil {
		return nil, nil, err
	}
	cache := newChunkCache(s.Store, id)
	var features []*flattypes.Feature
	var header *flattypes.Header
	for _, m := range matches {
		entry, err := cache.get(ctx, m.ChunkID)
		if err != nil {
			return nil, nil, err
		}
		if m.Ord >= len(entry.feats) {
			continue
		}
		if header == nil {
			header = entry.chunk.Header()
		} else if !sameColumns(header, entry.chunk.Header()) {
			return nil, nil, errors.New("collection chunks have differing schemas")
		}
		g := entry.chunk.GeoJSON(entry.feats[m.Ord])
		if !inBounds(g, bbox) {
			continue
		}
		if len(shapes) > 0 && !intersectsShapes(shapes, geo.ToOrb(g.Geometry)) {
			continue
		}
		features = append(features, entry.feats[m.Ord])
	}
	return features, header, nil
}
