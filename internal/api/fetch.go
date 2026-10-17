package api

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/store"
)

const chunkFetchWorkers = 8

func (s *Server) fetchRows(ctx context.Context, collection string, ids []int64) []store.ChunkRow {
	rows := make([]store.ChunkRow, len(ids))
	if len(ids) == 0 {
		return rows
	}
	sem := make(chan struct{}, chunkFetchWorkers)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id int64) {
			defer wg.Done()
			defer func() { <-sem }()
			row, err := s.Store.ChunkByID(ctx, collection, id)
			if err != nil {
				slog.Error("chunk load failed", "chunk", id, "error", err)
				return
			}
			rows[i] = row
		}(i, id)
	}
	wg.Wait()
	return rows
}

func (s *Server) chunkBatches(ctx context.Context, collection string, ids []int64) [][]int64 {
	sizes, err := s.Store.ChunkSizes(ctx, collection, ids)
	if err != nil {
		slog.Error("chunk size query failed", "collection", collection, "error", err)
	}
	return splitBatches(ids, sizes, s.MaxChunks, s.MaxBytes)
}

func splitBatches(ids []int64, sizes map[int64]int, maxCount, maxBytes int) [][]int64 {
	var batches [][]int64
	for start := 0; start < len(ids); {
		end := start
		bytes := 0
		for end < len(ids) && end-start < maxCount {
			size := sizes[ids[end]]
			if end > start && bytes+size > maxBytes {
				break
			}
			bytes += size
			end++
		}
		batches = append(batches, ids[start:end])
		start = end
	}
	return batches
}

func budgetMetas(metas []store.ChunkMeta, maxBytes int) ([]store.ChunkMeta, bool) {
	total := 0
	for i, meta := range metas {
		if i > 0 && total+meta.SizeBytes > maxBytes {
			return metas[:i], true
		}
		total += meta.SizeBytes
	}
	return metas, false
}

func geojsonAll(c *chunk.Chunk, features []*flattypes.Feature) []*chunk.Feature {
	out := make([]*chunk.Feature, len(features))
	workers := runtime.NumCPU()
	if workers > len(features) {
		workers = len(features)
	}
	if workers <= 1 {
		for i, f := range features {
			out[i] = c.GeoJSON(f)
		}
		return out
	}
	per := (len(features) + workers - 1) / workers
	var wg sync.WaitGroup
	for lo := 0; lo < len(features); lo += per {
		hi := min(lo+per, len(features))
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				out[i] = c.GeoJSON(features[i])
			}
		}(lo, hi)
	}
	wg.Wait()
	return out
}

func (s *Server) openChunk(ctx context.Context, collection string, version, id int64, row store.ChunkRow) (*chunk.Chunk, error) {
	key := fmt.Sprintf("%s|%d|%d", collection, id, version)
	if entry, ok := s.chunks.get(key); ok {
		return chunk.FromRaw(entry.data)
	}
	c, err := chunk.Open(row.Data)
	if err != nil {
		return nil, err
	}
	featuresAt, _, err := fgb.Regions(c.Data, c.Header())
	if err != nil {
		return nil, err
	}
	s.chunks.put(key, decompressedChunk{data: c.Data, featuresAt: featuresAt})
	return c, nil
}
