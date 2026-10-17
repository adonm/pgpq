package api

import (
	"context"
	"log/slog"
	"sync"

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
