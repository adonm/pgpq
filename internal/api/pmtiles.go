package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/encoding/mvt"
	orbgeojson "github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/maptile"
	"github.com/paulmach/orb/simplify"
	"github.com/protomaps/go-pmtiles/pmtiles"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
)

const pmtilesMediaType = "application/vnd.pmtiles"

const maxPMTilesTiles = 4096

type tilePayload struct {
	id   uint64
	data []byte
}

func tileBytes(name string, collection *orbgeojson.FeatureCollection, tile maptile.Tile) ([]byte, bool, error) {
	layer := mvt.NewLayer(name, collection)
	layer.ProjectToTile(tile)
	layer.Clip(mvt.MapboxGLDefaultExtentBound)
	layer.Simplify(simplify.DouglasPeucker(1.0))
	layer.RemoveEmpty(1.0, 2.0)
	if len(layer.Features) == 0 {
		return nil, true, nil
	}
	data, err := mvt.MarshalGzipped(mvt.Layers{layer})
	if err != nil {
		return nil, false, err
	}
	return data, false, nil
}

func parseMaxZoom(r *http.Request, fallback int) (int, error) {
	value := r.URL.Query().Get("maxzoom")
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 || n > 8 {
		return 0, errors.New("invalid maxzoom")
	}
	return n, nil
}

func clampBounds(b [4]float64) [4]float64 {
	const maxLat = 85.05112878
	return [4]float64{
		math.Max(b[0], -180), math.Max(b[1], -maxLat),
		math.Min(b[2], 180), math.Min(b[3], maxLat),
	}
}

func tilesForBounds(b [4]float64, z uint8) (uint32, uint32, uint32, uint32) {
	nw := maptile.At(orb.Point{b[0], b[3]}, maptile.Zoom(z))
	se := maptile.At(orb.Point{b[2], b[1]}, maptile.Zoom(z))
	last := uint32(1)<<z - 1
	clamp := func(v uint32) uint32 {
		if v > last {
			return last
		}
		return v
	}
	x0, x1 := clamp(nw.X), clamp(se.X)
	if x0 > x1 {
		x0, x1 = x1, x0
	}
	y0, y1 := clamp(nw.Y), clamp(se.Y)
	if y0 > y1 {
		y0, y1 = y1, y0
	}
	return x0, x1, y0, y1
}

func (s *Server) pmtilesCachePath(ctx context.Context, id, rawQuery string) (string, string, error) {
	if s.CacheDir == "" {
		return "", "", nil
	}
	state, err := s.Store.CollectionState(ctx, id)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(id + "\x00" + state + "\x00" + rawQuery))
	key := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(s.CacheDir, 0o755); err != nil {
		return "", "", err
	}
	return filepath.Join(s.CacheDir, key+".pmtiles"), key, nil
}

func (s *Server) servePMTiles(w http.ResponseWriter, r *http.Request, content io.ReadSeeker, modTime time.Time, key, name string) {
	w.Header().Set("Content-Type", pmtilesMediaType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".pmtiles"))
	if key != "" {
		w.Header().Set("ETag", `"`+key+`"`)
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, name+".pmtiles", modTime, content)
}

func (s *Server) writePMTiles(w http.ResponseWriter, r *http.Request, id string, bbox *[4]float64, query string, filter cql.Expr, offset, limit, maxzoom int) {
	ctx := r.Context()
	path, key, err := s.pmtilesCachePath(ctx, id, r.URL.RawQuery)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if path != "" {
		if file, err := os.Open(path); err == nil {
			defer file.Close()
			if info, err := file.Stat(); err == nil {
				s.servePMTiles(w, r, file, info.ModTime(), key, id)
				return
			}
		}
	}
	features, header, err := s.collectFeatures(ctx, id, bbox, query, filter, offset, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(features) == 0 {
		http.Error(w, "no features matched", http.StatusNotFound)
		return
	}
	fgb, err := chunk.EncodeFeatures(id, header, features)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	c, err := chunk.Open(fgb)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	env, ok := c.Envelope()
	if !ok {
		http.Error(w, "chunk missing envelope", http.StatusInternalServerError)
		return
	}
	bounds := clampBounds(env)
	if bbox != nil {
		bounds = clampBounds(*bbox)
	}
	total := 0
	for z := 0; z <= maxzoom; z++ {
		x0, x1, y0, y1 := tilesForBounds(bounds, uint8(z))
		total += int(x1-x0+1) * int(y1-y0+1)
	}
	if total > maxPMTilesTiles {
		http.Error(w, "too many tiles, reduce maxzoom or add a bbox", http.StatusBadRequest)
		return
	}
	var tiles []tilePayload
	for z := 0; z <= maxzoom; z++ {
		x0, x1, y0, y1 := tilesForBounds(bounds, uint8(z))
		for x := x0; x <= x1; x++ {
			for y := y0; y <= y1; y++ {
				tile := maptile.New(x, y, maptile.Zoom(z))
				bound := tile.Bound()
				feats, err := c.Search(bound.Min[0], bound.Min[1], bound.Max[0], bound.Max[1])
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if len(feats) == 0 {
					continue
				}
				fc := orbgeojson.NewFeatureCollection()
				for _, f := range feats {
					g := c.GeoJSON(f)
					geom := toOrb(g.Geometry)
					if geom == nil {
						continue
					}
					feature := orbgeojson.NewFeature(geom)
					feature.Properties = g.Properties
					fc.Append(feature)
				}
				data, empty, err := tileBytes(id, fc, tile)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if empty {
					continue
				}
				tiles = append(tiles, tilePayload{id: pmtiles.ZxyToID(uint8(z), x, y), data: data})
			}
		}
	}
	if len(tiles) == 0 {
		http.Error(w, "no tiles generated", http.StatusNotFound)
		return
	}
	body, err := buildPMTiles(id, uint8(maxzoom), bounds, c.Columns(), tiles)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if path != "" {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, body, 0o644); err == nil {
			os.Rename(tmp, path)
		}
	}
	s.servePMTiles(w, r, bytes.NewReader(body), time.Time{}, key, id)
}

func buildPMTiles(name string, maxZoom uint8, bounds [4]float64, columns []chunk.Column, tiles []tilePayload) ([]byte, error) {
	sort.Slice(tiles, func(i, j int) bool { return tiles[i].id < tiles[j].id })
	var data bytes.Buffer
	entries := make([]pmtiles.EntryV3, 0, len(tiles))
	for _, t := range tiles {
		entries = append(entries, pmtiles.EntryV3{
			TileID:    t.id,
			Offset:    uint64(data.Len()),
			Length:    uint32(len(t.data)),
			RunLength: 1,
		})
		data.Write(t.data)
	}
	root, leaves, _ := pmtiles.BuildDirectories(entries, 16384, pmtiles.Gzip)
	fields := map[string]string{}
	for _, col := range columns {
		fields[col.Name] = pmtilesFieldType(col.Type)
	}
	metadata := map[string]any{
		"name":   name,
		"format": "pbf",
		"type":   "overlay",
		"vector_layers": []map[string]any{{
			"id":     name,
			"fields": fields,
		}},
		"minzoom": 0,
		"maxzoom": maxZoom,
		"bounds":  fmt.Sprintf("%g,%g,%g,%g", bounds[0], bounds[1], bounds[2], bounds[3]),
	}
	meta, err := pmtiles.SerializeMetadata(metadata, pmtiles.Gzip)
	if err != nil {
		return nil, err
	}
	header := pmtiles.HeaderV3{
		SpecVersion:         3,
		RootOffset:          127,
		RootLength:          uint64(len(root)),
		MetadataOffset:      127 + uint64(len(root)),
		MetadataLength:      uint64(len(meta)),
		LeafDirectoryOffset: 127 + uint64(len(root)) + uint64(len(meta)),
		LeafDirectoryLength: uint64(len(leaves)),
		TileDataOffset:      127 + uint64(len(root)) + uint64(len(meta)) + uint64(len(leaves)),
		TileDataLength:      uint64(data.Len()),
		AddressedTilesCount: uint64(len(tiles)),
		TileEntriesCount:    uint64(len(tiles)),
		TileContentsCount:   uint64(len(tiles)),
		Clustered:           true,
		InternalCompression: pmtiles.Gzip,
		TileCompression:     pmtiles.Gzip,
		TileType:            pmtiles.Mvt,
		MinZoom:             0,
		MaxZoom:             maxZoom,
		MinLonE7:            toE7(bounds[0]),
		MinLatE7:            toE7(bounds[1]),
		MaxLonE7:            toE7(bounds[2]),
		MaxLatE7:            toE7(bounds[3]),
		CenterZoom:          maxZoom,
		CenterLonE7:         toE7((bounds[0] + bounds[2]) / 2),
		CenterLatE7:         toE7((bounds[1] + bounds[3]) / 2),
	}
	var out bytes.Buffer
	out.Write(pmtiles.SerializeHeader(header))
	out.Write(root)
	out.Write(meta)
	out.Write(leaves)
	out.Write(data.Bytes())
	return out.Bytes(), nil
}

func toE7(v float64) int32 {
	return int32(math.Round(v * 1e7))
}

func pmtilesFieldType(t flattypes.ColumnType) string {
	switch t {
	case flattypes.ColumnTypeBool:
		return "Boolean"
	case flattypes.ColumnTypeByte, flattypes.ColumnTypeUByte,
		flattypes.ColumnTypeShort, flattypes.ColumnTypeUShort,
		flattypes.ColumnTypeInt, flattypes.ColumnTypeUInt,
		flattypes.ColumnTypeLong, flattypes.ColumnTypeULong,
		flattypes.ColumnTypeFloat, flattypes.ColumnTypeDouble:
		return "Number"
	default:
		return "String"
	}
}
