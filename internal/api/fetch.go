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
