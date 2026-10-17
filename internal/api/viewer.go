package api

import (
	"context"
	"encoding/json"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/store"
)

const (
	rustgisWeb           = "https://web.rustgis.app/"
	projectCameraWorld   = 40075016.685578488 / 512
	projectOverviewScale = 1000000.0
	projectDetailScale   = 140000.0
)

var viewerPage = template.Must(template.New("viewer").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<title>postfgb · RustGIS</title>
<style>
*{box-sizing:border-box}
html,body{margin:0;height:100%;background:#0b0b0d;color:#e5e5e5;font:12px/1.4 ui-sans-serif,system-ui,sans-serif}
#app{display:flex;flex-direction:column;height:100%}
#bar{display:flex;align-items:center;gap:12px;flex-wrap:wrap;padding:7px 12px;background:#18181b;border-bottom:1px solid #27272a;color:#a1a1aa}
#bar .brand{font:600 13px ui-sans-serif,system-ui,sans-serif;color:#e5e5e5;letter-spacing:.2px}
#bar .host{font:11px ui-monospace,monospace;color:#71717a}
#bar .divider{width:1px;height:16px;background:#3f3f46;flex:none}
#bar .stats{font:11px ui-monospace,monospace;color:#a1a1aa;display:flex;align-items:center;gap:10px;flex-wrap:wrap}
#bar .stats b{color:#e5e5e5;font-weight:600}
#bar .spacer{flex:1 1 auto}
#bar a{color:#38bdf8;text-decoration:none;font:11px ui-monospace,monospace;padding:2px 6px;border-radius:4px}
#bar a:hover{background:#27272a;text-decoration:none}
iframe{border:0;width:100%;flex:1;background:#0b0b0d}
</style>
</head>
<body>
<div id="app">
  <div id="bar">
    <span class="brand">postfgb</span>
    <span class="divider"></span>
    <span class="host">{{.Host}}</span>
    <span class="divider"></span>
    <span class="stats" id="stats">loading stats…</span>
    <span class="spacer"></span>
    <a href="{{.Project}}" target="_blank" rel="noopener">project JSON</a>
    <span class="divider"></span>
    <a href="{{.Open}}" target="_blank" rel="noopener">open in RustGIS ↗</a>
  </div>
  <iframe src="{{.Embed}}" title="RustGIS" allow="fullscreen; clipboard-write" referrerpolicy="strict-origin-when-cross-origin"></iframe>
</div>
<script>
const bytes = (n) => {
  if (!n && n !== 0) return "?";
  const units = ["B", "kB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)) + " " + units[i];
};
fetch("/stats").then((r) => r.json()).then((s) => {
  const when = new Date(s.updatedAt);
  document.getElementById("stats").innerHTML = [
    "<b>" + s.collections + "</b> collections",
    "<b>" + s.features.toLocaleString() + "</b> features",
    "<b>" + s.chunks.toLocaleString() + "</b> chunks",
    "<b>" + bytes(s.storedBytes) + "</b> chunk payload",
    "<b>" + bytes(s.databaseBytes) + "</b> database",
    "updated <b>" + when.toLocaleString() + "</b>",
  ].join(" · ");
}).catch(() => { document.getElementById("stats").textContent = ""; });
</script>
</body>
</html>
`))

func (s *Server) viewer(w http.ResponseWriter, r *http.Request) {
	base := baseURL(r)
	project := base + "/viewer.rgis"
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	viewerPage.Execute(w, struct {
		Host    string
		Project string
		Open    string
		Embed   string
	}{
		Host:    r.Host,
		Project: project,
		Open:    rustgisWeb + "?project=" + url.QueryEscape(project),
		Embed:   rustgisWeb + "?embed&project=" + url.QueryEscape(project),
	})
}

func (s *Server) viewerProject(w http.ResponseWriter, r *http.Request) {
	infos, err := s.Store.Collections(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	base := baseURL(r)
	layers := []map[string]any{}
	children := []map[string]any{}
	next := 1
	for _, info := range infos {
		title := info.Name
		if info.Title != nil && *info.Title != "" {
			title = *info.Title
		}
		geometry := s.collectionGeometryType(r, info)
		if geometry == "" {
			continue
		}
		children = append(children, map[string]any{
			"layer":      next,
			"collection": info.Name,
			"source_url": base + "/collections/" + url.PathEscape(info.Name) + "/items",
		})
		for _, layer := range s.projectLayers(r.Context(), base, info, title, geometry, &next) {
			layers = append(layers, layer)
		}
	}
	centerX, centerY, res := projectCameraWorld, 0.0, projectCameraWorld
	if minX, minY, maxX, maxY, ok := catalogExtent(infos); ok {
		centerX = (minX + maxX) / 2 * 20037508.342789244 / 180
		centerY = math.Log(math.Tan((90+math.Min(math.Max((minY+maxY)/2, -85.05), 85.05))*math.Pi/360)) / (math.Pi / 180) * 20037508.342789244 / 180
		spanX := (maxX - minX) * 20037508.342789244 / 180
		spanY := (maxY - minY) * 20037508.342789244 / 180 * 1.4
		res = math.Max(spanX, spanY) / 512
		if res <= 0 || math.IsInf(res, 0) || math.IsNaN(res) {
			res = projectCameraWorld
		}
	}
	groups := []map[string]any{}
	if len(children) > 0 {
		groups = append(groups, map[string]any{
			"id":       1,
			"name":     "postfgb",
			"url":      base + "/collections",
			"kind":     "ogcapi",
			"expanded": false,
			"children": children,
		})
	}
	project := map[string]any{
		"format":     1,
		"name":       "postfgb",
		"active_map": 0,
		"maps": []map[string]any{{
			"name":           "postfgb",
			"layers":         layers,
			"service_groups": groups,
			"basemap":        "Paper",
			"projection":     "WebMercator",
			"next_layer_id":  next,
			"camera": map[string]any{
				"center": map[string]float64{"x": centerX, "y": centerY},
				"res":    res,
			},
		}},
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `inline; filename="postfgb.rgis"`)
	json.NewEncoder(w).Encode(project)
}

func catalogExtent(infos []store.CollectionInfo) (float64, float64, float64, float64, bool) {
	minX, minY, maxX, maxY := 0.0, 0.0, 0.0, 0.0
	seen := false
	for _, info := range infos {
		if info.MinX == nil || info.MinY == nil || info.MaxX == nil || info.MaxY == nil {
			continue
		}
		if !seen {
			minX, minY, maxX, maxY = *info.MinX, *info.MinY, *info.MaxX, *info.MaxY
			seen = true
			continue
		}
		minX, minY = math.Min(minX, *info.MinX), math.Min(minY, *info.MinY)
		maxX, maxY = math.Max(maxX, *info.MaxX), math.Max(maxY, *info.MaxY)
	}
	return minX, minY, maxX, maxY, seen
}

func (s *Server) collectionGeometryType(r *http.Request, info store.CollectionInfo) string {
	collection := info.Name
	if info.Kind == "view" {
		names, err := s.Store.ViewMemberNames(r.Context(), info)
		if err != nil || len(names) == 0 {
			return ""
		}
		collection = names[0]
	}
	metas, err := s.Store.ChunkMetas(r.Context(), collection, nil, 1, 0, 0)
	if err != nil || len(metas) == 0 {
		return ""
	}
	row, err := s.Store.ChunkByID(r.Context(), collection, metas[0].ID)
	if err != nil {
		return ""
	}
	c, err := chunk.Open(row.Data)
	if err != nil {
		return ""
	}
	switch c.Header().GeometryType() {
	case flattypes.GeometryTypePoint, flattypes.GeometryTypeMultiPoint:
		return "Point"
	case flattypes.GeometryTypeLineString, flattypes.GeometryTypeMultiLineString:
		return "Polyline"
	case flattypes.GeometryTypePolygon, flattypes.GeometryTypeMultiPolygon:
		return "Polygon"
	}
	features, err := c.SearchAll()
	if err != nil {
		return ""
	}
	for _, f := range features {
		switch g := c.GeoJSON(f).Geometry; {
		case g == nil:
			continue
		case strings.HasPrefix(g.Type, "Point"):
			return "Point"
		case strings.HasPrefix(g.Type, "Line"):
			return "Polyline"
		default:
			return "Polygon"
		}
	}
	return ""
}

type projectLayerSpec struct {
	label string
	query string
	min   any
	max   any
}

func (s *Server) projectLayers(ctx context.Context, base string, info store.CollectionInfo, title, geometry string, next *int) []map[string]any {
	specs := []projectLayerSpec{{min: projectDetailScale}}
	if s.hasTier(ctx, info.Name, 1) {
		specs = append([]projectLayerSpec{{label: " · 10%", query: "?overview=1", min: projectOverviewScale, max: projectDetailScale}}, specs...)
	}
	if s.hasTier(ctx, info.Name, 2) {
		specs = append([]projectLayerSpec{{label: " · 1%", query: "?overview=2", max: projectOverviewScale}}, specs...)
	}
	layers := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
		layer := map[string]any{
			"id":      *next,
			"name":    title + spec.label,
			"visible": false,
			"dataset": map[string]any{
				"name":          title + spec.label,
				"geometry_type": geometry,
				"fields":        []any{},
				"features":      []any{},
			},
			"renderer": map[string]any{"Single": map[string]any{
				"fill": []int{80, 160, 240, 120}, "stroke": []int{60, 120, 180, 255},
				"stroke_width": 0.7, "size": 3.0, "marker": "Circle",
			}},
			"labels": map[string]any{
				"enabled": false, "field": "name", "size": 10.0,
				"color": []int{43, 43, 43, 255}, "halo": true, "bold": false,
				"min_scale": nil, "max_scale": nil, "closest": nil, "farthest": nil,
				"placement": "Auto",
			},
			"transparency": 0.0,
			"expanded":     true,
			"selectable":   true,
			"min_scale":    spec.min,
			"max_scale":    spec.max,
			"source": map[string]any{
				"path": base + "/collections/" + url.PathEscape(info.Name) + "/items.fgb" + spec.query,
			},
		}
		layers = append(layers, layer)
		*next++
	}
	return layers
}

func (s *Server) hasTier(ctx context.Context, collection string, tier int) bool {
	metas, err := s.Store.ChunkMetas(ctx, collection, nil, 1, tier, 0)
	return err == nil && len(metas) > 0
}
