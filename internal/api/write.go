package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/adonm/postfgb/internal/ingest"
	"github.com/adonm/postfgb/internal/store"
)

func (s *Server) ingestData(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	sortable := parseSortable(r.URL.Query().Get("sortable"))
	if len(sortable) > 4 {
		http.Error(w, "at most 4 sortable columns", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	result, err := ingest.Run(r.Context(), s.Store, r.Body, ingest.Options{
		Collection: id,
		Sortable:   sortable,
	})
	if errors.Is(err, ingest.ErrInvalid) {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"collection": id,
		"features":   result.Features,
		"chunks":     result.Chunks,
	})
}

func (s *Server) deleteCollection(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	err := s.Store.DropCollection(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "collection not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseSortable(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
