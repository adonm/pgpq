package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/adonm/pgpq/internal/dataset"
	"github.com/adonm/pgpq/internal/geo"
	"github.com/adonm/pgpq/internal/store"
)

type Server struct {
	Store    *store.Store
	datasets *dataset.Manager
}

type Config struct {
	Store    *store.Store
	ReadOnly bool
	Datasets *dataset.Manager
}

func New(st *store.Store) http.Handler {
	return NewWithConfig(Config{Store: st})
}

func NewWithConfig(cfg Config) http.Handler {
	mux := http.NewServeMux()
	s := &Server{Store: cfg.Store, datasets: cfg.Datasets}
	if s.datasets == nil && cfg.Store != nil {
		s.datasets = dataset.New(cfg.Store.Pool)
	}
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /v2/collections", s.listCollections)
	mux.HandleFunc("GET /v2/collections/{file}", s.getCollection)
	if !cfg.ReadOnly {
		mux.HandleFunc("PUT /v2/collections/{id}", s.writeCollection)
		mux.HandleFunc("POST /v2/collections/{id}", s.writeCollection)
	}
	mux.HandleFunc("GET /design", s.design)
	return corsMiddleware(compressionMiddleware(s.observe(mux)))
}

func (s *Server) writeCollection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("id")
	if !store.ValidName(name) {
		http.Error(w, "invalid collection name", http.StatusUnprocessableEntity)
		return
	}
	ds, err := geo.ReadDataset(r.Body, geo.Options{IDColumn: r.URL.Query().Get("id_column")})
	if err != nil {
		writeError(w, err)
		return
	}
	var result dataset.Result
	if r.Method == http.MethodPut {
		result, err = s.datasets.Put(r.Context(), name, ds)
	} else {
		result, err = s.datasets.Upsert(r.Context(), name, ds)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) getCollection(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("file")
	name := strings.TrimSuffix(value, ".parquet")
	if name == value {
		s.collectionMeta(w, r, value)
		return
	}
	if !store.ValidName(name) {
		http.Error(w, "invalid collection name", http.StatusUnprocessableEntity)
		return
	}
	version, err := requestedVersion(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var v *dataset.VirtualFile
	if version > 0 {
		v, err = s.datasets.OpenVersion(r.Context(), name, version)
	} else {
		v, err = s.datasets.Open(r.Context(), name)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apache.parquet")
	w.Header().Set("ETag", `"`+v.ETag()+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name+".parquet", time.Time{}, io.NewSectionReader(v, 0, v.Size()))
}

func (s *Server) collectionMeta(w http.ResponseWriter, r *http.Request, name string) {
	if !store.ValidName(name) {
		http.Error(w, "invalid collection name", http.StatusUnprocessableEntity)
		return
	}
	c, err := s.datasets.Collection(r.Context(), name)
	if err != nil {
		writeError(w, err)
		return
	}
	v, err := s.datasets.Open(r.Context(), name)
	if err != nil {
		writeError(w, err)
		return
	}
	out := map[string]any{
		"name":     c.Name,
		"version":  c.Version,
		"idColumn": c.IDColumn,
		"geometry": c.Geometry,
		"columns":  c.Columns,
		"rows":     c.Rows,
		"bytes":    c.Bytes,
		"etag":     v.ETag(),
		"url":      "/v2/collections/" + name + ".parquet",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) listCollections(w http.ResponseWriter, r *http.Request) {
	collections, err := s.datasets.List(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]map[string]any, len(collections))
	for i, c := range collections {
		out[i] = map[string]any{
			"name":     c.Name,
			"version":  c.Version,
			"idColumn": c.IDColumn,
			"geometry": c.Geometry,
			"rows":     c.Rows,
			"bytes":    c.Bytes,
			"url":      "/v2/collections/" + c.Name + ".parquet",
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"collections": out})
}

func requestedVersion(r *http.Request) (int64, error) {
	value := r.URL.Query().Get("version")
	if value == "" {
		return 0, nil
	}
	version, err := strconv.ParseInt(value, 10, 64)
	if err != nil || version < 1 {
		return 0, errors.New("version must be a positive integer")
	}
	return version, nil
}

func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, dataset.ErrInvalid):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, dataset.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "collection not found", http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
