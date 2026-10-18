package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/adonm/postfgb/internal/store"
)

const stacVersion = "1.0.0"

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	return scheme + "://" + r.Host
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(body)
}

func (s *Server) stacCollections(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	infos, err := s.Store.Collections(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	base := baseURL(r)
	out := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		out = append(out, s.stacCollectionBody(ctx, info, base))
	}
	writeJSON(w, map[string]any{
		"collections": out,
		"links": []map[string]any{
			{"rel": "self", "href": base + "/stac/collections", "type": "application/json"},
			{"rel": "root", "href": base + "/stac", "type": "application/json"},
		},
	})
}

func (s *Server) stacCollection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	info, err := s.Store.Collection(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "collection not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.stacCollectionBody(ctx, info, baseURL(r)))
}

func (s *Server) stacCollectionBody(ctx context.Context, info store.CollectionInfo, base string) map[string]any {
	out := map[string]any{}
	if len(info.Stac) > 0 {
		var stored map[string]any
		if json.Unmarshal(info.Stac, &stored) == nil {
			out = stored
		}
	}
	out["type"] = "Collection"
	out["stac_version"] = stacVersion
	out["id"] = info.Name
	title := info.Name
	if info.Title != nil && *info.Title != "" {
		title = *info.Title
	}
	out["title"] = title
	if info.Description != nil && *info.Description != "" {
		out["description"] = *info.Description
	} else if _, ok := out["description"]; !ok {
		out["description"] = title
	}
	if info.License != nil && *info.License != "" {
		out["license"] = *info.License
	} else if _, ok := out["license"]; !ok {
		out["license"] = "proprietary"
	}
	if len(info.Keywords) > 0 {
		out["keywords"] = info.Keywords
	}
	spatial := [][]float64{{-180, -90, 180, 90}}
	interval := [][]any{{nil, nil}}
	if ext := s.collectionExtent(ctx, info); ext != nil {
		if len(ext.Spatial.BBox) > 0 {
			spatial = ext.Spatial.BBox
		}
		if ext.Temporal != nil && len(ext.Temporal.Interval) == 1 {
			pair := ext.Temporal.Interval[0]
			interval = [][]any{{pair[0], pair[1]}}
		}
	}
	out["extent"] = map[string]any{
		"spatial":  map[string]any{"bbox": spatial},
		"temporal": map[string]any{"interval": interval},
	}
	out["links"] = []map[string]any{
		{"rel": "self", "href": base + "/stac/collections/" + info.Name, "type": "application/json"},
		{"rel": "root", "href": base + "/stac", "type": "application/json"},
		{"rel": "parent", "href": base + "/stac/collections", "type": "application/json"},
		{"rel": "items", "href": base + "/stac/collections/" + info.Name + "/items", "type": "application/geo+json"},
	}
	return out
}
