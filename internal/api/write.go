package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/adonm/postfgb/internal/cql"
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
		IDColumn:   r.URL.Query().Get("id_column"),
		TimeColumn: r.URL.Query().Get("time_column"),
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

func (s *Server) putView(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r.Header.Get("Authorization")) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	var req struct {
		Members []viewMember `json:"members"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid view definition", http.StatusBadRequest)
		return
	}
	if len(req.Members) == 0 || len(req.Members) > maxViewMembers {
		http.Error(w, "view needs 1 to 16 members", http.StatusUnprocessableEntity)
		return
	}
	seen := map[string]bool{}
	for _, member := range req.Members {
		if member.Collection == "" || seen[member.Collection] {
			http.Error(w, "duplicate or empty member", http.StatusUnprocessableEntity)
			return
		}
		seen[member.Collection] = true
		mi, err := s.Store.Collection(ctx, member.Collection)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown member collection "+member.Collection, http.StatusUnprocessableEntity)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if mi.Kind != "native" {
			http.Error(w, "member must be a native collection", http.StatusUnprocessableEntity)
			return
		}
		if member.Filter != "" {
			if _, err := cql.Parse(member.Filter); err != nil {
				http.Error(w, "member filter: "+err.Error(), http.StatusUnprocessableEntity)
				return
			}
		}
		for src, dst := range member.Columns {
			if src == "" || dst == "" {
				http.Error(w, "empty column mapping", http.StatusUnprocessableEntity)
				return
			}
		}
	}
	_, err := s.Store.Collection(ctx, id)
	created := errors.Is(err, store.ErrNotFound)
	if err != nil && !created {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	payload, err := json.Marshal(req.Members)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.Store.PutView(ctx, id, payload); errors.Is(err, store.ErrConflict) {
		http.Error(w, "collection exists and is not a view", http.StatusConflict)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"collection": id, "members": len(req.Members)})
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
