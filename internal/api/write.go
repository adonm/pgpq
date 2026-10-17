package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/ingest"
	"github.com/adonm/postfgb/internal/store"
)

func (s *Server) putCollection(w http.ResponseWriter, r *http.Request) {
	if !store.ValidName(r.PathValue("id")) {
		http.Error(w, "invalid collection name", http.StatusUnprocessableEntity)
		return
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		s.putCollectionDoc(w, r)
		return
	}
	s.putCollectionData(w, r)
}

func (s *Server) putCollectionData(w http.ResponseWriter, r *http.Request) {
	sortable := parseList(r.URL.Query().Get("sortable"))
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
		s.metrics.add("postfgb_ingest_total", metricLabels("result", "invalid"), 1)
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.metrics.add("postfgb_ingest_total", metricLabels("result", "error"), 1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.metrics.add("postfgb_ingest_total", metricLabels("result", "ok"), 1)
	s.metrics.add("postfgb_ingest_features_total", "", int64(result.Features))
	s.metrics.add("postfgb_ingest_chunks_total", "", int64(result.Chunks))
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

type collectionDoc struct {
	ID          string       `json:"id"`
	Title       *string      `json:"title"`
	Description *string      `json:"description"`
	License     *string      `json:"license"`
	Keywords    *[]string    `json:"keywords"`
	Members     []viewMember `json:"members"`
}

func (s *Server) putCollectionDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid document", http.StatusBadRequest)
		return
	}
	var doc collectionDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		http.Error(w, "invalid document", http.StatusBadRequest)
		return
	}
	if doc.ID != "" && doc.ID != id {
		http.Error(w, "document id does not match the path", http.StatusUnprocessableEntity)
		return
	}
	meta := store.CollectionMeta{Title: doc.Title, Description: doc.Description, License: doc.License, Keywords: doc.Keywords}
	if doc.Members != nil {
		s.putViewDoc(w, r, id, doc.Members, meta)
		return
	}
	if err := s.Store.PutMeta(ctx, id, body, meta); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"collection": id})
}

func (s *Server) putViewDoc(w http.ResponseWriter, r *http.Request, id string, members []viewMember, meta store.CollectionMeta) {
	ctx := r.Context()
	if len(members) == 0 || len(members) > maxViewMembers {
		http.Error(w, "view needs 1 to 16 members", http.StatusUnprocessableEntity)
		return
	}
	seen := map[string]bool{}
	for _, member := range members {
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
	payload, err := json.Marshal(members)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.Store.PutView(ctx, id, payload, meta); errors.Is(err, store.ErrConflict) {
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
	json.NewEncoder(w).Encode(map[string]any{"collection": id, "members": len(members)})
}

func (s *Server) deleteCollection(w http.ResponseWriter, r *http.Request) {
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

func parseList(value string) []string {
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
