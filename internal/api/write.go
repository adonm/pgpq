package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

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
		Flatten:    flattenJSON(r),
	})
	if errors.Is(err, ingest.ErrInvalid) || errors.Is(err, store.ErrInvalidView) {
		s.metrics.recordIngest("invalid", 0, 0)
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.metrics.recordIngest("error", 0, 0)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.metrics.recordIngest("ok", result.Features, result.Chunks)
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
	ID          string    `json:"id"`
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	License     *string   `json:"license"`
	Keywords    *[]string `json:"keywords"`
	Members     []string  `json:"members"`
	Postfgb     []string  `json:"postfgb:members"`
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
	members := doc.Members
	if len(members) == 0 {
		members = doc.Postfgb
	}
	if len(members) > 0 {
		s.putView(w, r, id, members, body, meta)
		return
	}
	if err := s.Store.PutMeta(ctx, id, body, meta); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"collection": id})
}

func (s *Server) deleteCollection(w http.ResponseWriter, r *http.Request) {
	if !store.ValidName(r.PathValue("id")) {
		http.Error(w, "invalid collection name", http.StatusUnprocessableEntity)
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

func flattenJSON(r *http.Request) bool {
	switch strings.ToLower(r.URL.Query().Get("flatten")) {
	case "off", "none", "false", "0":
		return false
	}
	return true
}

func (s *Server) putView(w http.ResponseWriter, r *http.Request, id string, names []string, doc []byte, meta store.CollectionMeta) {
	_, err := s.Store.Collection(r.Context(), id)
	created := errors.Is(err, store.ErrNotFound)
	err = s.Store.PutView(r.Context(), id, names, doc, meta)
	switch {
	case errors.Is(err, store.ErrInvalidView):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	case errors.Is(err, store.ErrConflict):
		http.Error(w, "collection exists and is not a view", http.StatusConflict)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"collection": id, "members": names})
}
