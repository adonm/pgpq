package api

import (
	"context"
	"net/http"
	"time"
)

type catalogStats struct {
	Collections   int64     `json:"collections"`
	Features      int64     `json:"features"`
	Chunks        int64     `json:"chunks"`
	StoredBytes   int64     `json:"storedBytes"`
	DatabaseBytes int64     `json:"databaseBytes"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	out, err := s.catalogStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, out)
}

func (s *Server) catalogStats(ctx context.Context) (catalogStats, error) {
	var out catalogStats
	if s.Store == nil {
		return out, nil
	}
	err := s.Store.Pool.QueryRow(ctx, `
		select (select count(*) from collections),
		       (select coalesce(sum(feature_count), 0) from collections),
		       (select coalesce(sum(chunk_count), 0) from collections),
		       (select coalesce(max(updated_at), now()) from collections),
		       (select coalesce(sum(length(data)), 0) from chunks),
		       pg_database_size(current_database())`).
		Scan(&out.Collections, &out.Features, &out.Chunks, &out.UpdatedAt, &out.StoredBytes, &out.DatabaseBytes)
	return out, err
}
