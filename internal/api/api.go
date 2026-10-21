package api

import (
	"net/http"

	"github.com/adonm/pgpq/internal/layer"
	"github.com/adonm/pgpq/internal/store"
)

type Server struct {
	Store  *store.Store
	layers *layer.Layer
}

type Config struct {
	Store    *store.Store
	ReadOnly bool
	Layers   *layer.Layer
}

type link struct {
	Href string `json:"href"`
	Rel  string `json:"rel"`
	Type string `json:"type,omitempty"`
}

const crs84 = "http://www.opengis.net/def/crs/OGC/1.3/CRS84"

func New(st *store.Store) http.Handler {
	return NewWithConfig(Config{Store: st})
}

func NewWithConfig(cfg Config) http.Handler {
	mux := http.NewServeMux()
	s := &Server{Store: cfg.Store}
	if cfg.Layers != nil {
		s.layers = cfg.Layers
	} else if cfg.Store != nil {
		s.layers = layer.New(cfg.Store.Pool)
	}
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	if !cfg.ReadOnly {
		mux.HandleFunc("PUT /v2/collections/{id}", s.putLayer)
		mux.HandleFunc("POST /v2/collections/{id}", s.putLayer)
	}
	mux.HandleFunc("GET /v2/collections/{id}/items", s.layerItems)
	mux.HandleFunc("GET /v2/collections/{id}/items/{featureID}", s.layerItem)
	mux.HandleFunc("GET /design", s.design)
	return corsMiddleware(compressionMiddleware(s.observe(rejectUnsupportedQuery(mux))))
}

func rejectUnsupportedQuery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		for _, name := range []string{"sortby", "q"} {
			if query.Has(name) {
				http.Error(w, name+" is not supported", http.StatusBadRequest)
				return
			}
		}
		for _, name := range []string{"crs", "bbox-crs"} {
			if value := query.Get(name); value != "" && value != crs84 {
				http.Error(w, name+" must be "+crs84, http.StatusBadRequest)
				return
			}
		}
		if value := query.Get("filter-lang"); value != "" && value != "cql2-text" {
			http.Error(w, "filter-lang must be cql2-text", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
