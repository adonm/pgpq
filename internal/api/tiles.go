package api

import (
	"context"
	"errors"
	"math"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/adonm/postfgb/internal/store"
	"github.com/adonm/postfgb/internal/tiles"
)

const (
	webMercatorQuadID  = "WebMercatorQuad"
	webMercatorQuadURI = "http://www.opengis.net/def/tilematrixset/OGC/1.0/WebMercatorQuad"
	tmsEPSG3857        = "http://www.opengis.net/def/crs/EPSG/0/3857"
)

type tilesetBoundingBox struct {
	LowerLeft  []float64 `json:"lowerLeft"`
	UpperRight []float64 `json:"upperRight"`
	CRS        string    `json:"crs,omitempty"`
}

type tileMatrixSetLimit struct {
	TileMatrix string `json:"tileMatrix"`
	MinTileRow int    `json:"minTileRow"`
	MaxTileRow int    `json:"maxTileRow"`
	MinTileCol int    `json:"minTileCol"`
	MaxTileCol int    `json:"maxTileCol"`
}

type tileset struct {
	Title               string               `json:"title,omitempty"`
	DataType            string               `json:"dataType"`
	CRS                 string               `json:"crs"`
	TileMatrixSetURI    string               `json:"tileMatrixSetURI,omitempty"`
	BoundingBox         *tilesetBoundingBox  `json:"boundingBox,omitempty"`
	TileMatrixSetLimits []tileMatrixSetLimit `json:"tileMatrixSetLimits,omitempty"`
	Links               []link               `json:"links"`
}

type tilesetsOutput struct {
	Body struct {
		Tilesets []tileset `json:"tilesets"`
		Links    []link    `json:"links"`
	}
}

func (s *Server) tilesets(ctx context.Context, in *collectionInput) (*tilesetsOutput, error) {
	info, err := s.Store.Collection(ctx, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("collection not found")
	}
	if err != nil {
		return nil, err
	}
	out := &tilesetsOutput{}
	set := tileset{
		Title:            info.Name,
		DataType:         "vector",
		CRS:              tmsEPSG3857,
		TileMatrixSetURI: webMercatorQuadURI,
		Links: []link{
			{
				Href:      "/collections/" + in.ID + "/tiles/" + webMercatorQuadID + "/{z}/{x}/{y}",
				Rel:       "item",
				Type:      "application/vnd.mapbox-vector-tile",
				Title:     "Tiles",
				Templated: true,
			},
			{
				Href:  "/tileMatrixSets/" + webMercatorQuadID,
				Rel:   "http://www.opengis.net/def/rel/ogc/1.0/tiling-scheme",
				Type:  "application/json",
				Title: "WebMercatorQuad tile matrix set",
			},
		},
	}
	if extent := s.collectionExtent(ctx, info); extent != nil && len(extent.Spatial.BBox) > 0 && len(extent.Spatial.BBox[0]) == 4 {
		bbox := extent.Spatial.BBox[0]
		set.BoundingBox = webMercatorBox([4]float64{bbox[0], bbox[1], bbox[2], bbox[3]})
		set.TileMatrixSetLimits = webMercatorLimits([4]float64{bbox[0], bbox[1], bbox[2], bbox[3]})
	}
	out.Body.Tilesets = []tileset{set}
	out.Body.Links = []link{{Href: "/collections/" + in.ID + "/tiles", Rel: "self", Type: "application/json"}}
	return out, nil
}

const (
	webMercatorMaxLat = 85.05112877980659
	webMercatorRadius = 20037508.342789244
)

func webMercatorMeters(lon, lat float64) (float64, float64) {
	switch {
	case lat > webMercatorMaxLat:
		lat = webMercatorMaxLat
	case lat < -webMercatorMaxLat:
		lat = -webMercatorMaxLat
	}
	x := lon * webMercatorRadius / 180
	y := math.Log(math.Tan((90+lat)*math.Pi/360)) / (math.Pi / 180) * webMercatorRadius / 180
	return x, y
}

func webMercatorBox(b [4]float64) *tilesetBoundingBox {
	minX, minY := webMercatorMeters(b[0], b[1])
	maxX, maxY := webMercatorMeters(b[2], b[3])
	return &tilesetBoundingBox{LowerLeft: []float64{minX, minY}, UpperRight: []float64{maxX, maxY}, CRS: tmsEPSG3857}
}

func webMercatorLimits(b [4]float64) []tileMatrixSetLimit {
	limits := make([]tileMatrixSetLimit, 0, 25)
	for z := 0; z <= 24; z++ {
		x0, x1, y0, y1 := tiles.BoundsFor(b, uint8(z))
		limits = append(limits, tileMatrixSetLimit{
			TileMatrix: strconv.Itoa(z),
			MinTileRow: int(y0), MaxTileRow: int(y1),
			MinTileCol: int(x0), MaxTileCol: int(x1),
		})
	}
	return limits
}

type tileMatrixSetSummary struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	URI   string `json:"uri,omitempty"`
	CRS   string `json:"crs,omitempty"`
	Links []link `json:"links"`
}

type tileMatrixSetsOutput struct {
	Body struct {
		TileMatrixSets []tileMatrixSetSummary `json:"tileMatrixSets"`
		Links          []link                 `json:"links"`
	}
}

func (s *Server) tileMatrixSets(context.Context, *struct{}) (*tileMatrixSetsOutput, error) {
	out := &tileMatrixSetsOutput{}
	out.Body.TileMatrixSets = []tileMatrixSetSummary{{
		ID:    webMercatorQuadID,
		Title: "Google Maps Compatible for the World",
		URI:   webMercatorQuadURI,
		CRS:   tmsEPSG3857,
		Links: []link{{Href: "/tileMatrixSets/" + webMercatorQuadID, Rel: "item", Type: "application/json"}},
	}}
	out.Body.Links = []link{{Href: "/tileMatrixSets", Rel: "self", Type: "application/json"}}
	return out, nil
}

type tileMatrixSetInput struct {
	TileMatrixSetID string `path:"tileMatrixSetId"`
}

type tileMatrix struct {
	ID               string    `json:"id"`
	ScaleDenominator float64   `json:"scaleDenominator"`
	CellSize         float64   `json:"cellSize"`
	PointOfOrigin    []float64 `json:"pointOfOrigin"`
	TileWidth        int       `json:"tileWidth"`
	TileHeight       int       `json:"tileHeight"`
	MatrixWidth      int       `json:"matrixWidth"`
	MatrixHeight     int       `json:"matrixHeight"`
}

type tileMatrixSetDefinition struct {
	ID                string       `json:"id"`
	Title             string       `json:"title"`
	URI               string       `json:"uri"`
	CRS               string       `json:"crs"`
	OrderedAxes       []string     `json:"orderedAxes"`
	WellKnownScaleSet string       `json:"wellKnownScaleSet,omitempty"`
	TileMatrices      []tileMatrix `json:"tileMatrices"`
}

type tileMatrixSetOutput struct {
	Body tileMatrixSetDefinition
}

func (s *Server) tileMatrixSet(_ context.Context, in *tileMatrixSetInput) (*tileMatrixSetOutput, error) {
	if in.TileMatrixSetID != webMercatorQuadID {
		return nil, huma.Error404NotFound("tile matrix set not found")
	}
	return &tileMatrixSetOutput{Body: webMercatorQuad()}, nil
}

func webMercatorQuad() tileMatrixSetDefinition {
	def := tileMatrixSetDefinition{
		ID:                webMercatorQuadID,
		Title:             "Google Maps Compatible for the World",
		URI:               webMercatorQuadURI,
		CRS:               tmsEPSG3857,
		OrderedAxes:       []string{"E", "N"},
		WellKnownScaleSet: "http://www.opengis.net/def/wkss/OGC/1.0/GoogleMapsCompatible",
	}
	for z := 0; z <= 24; z++ {
		dim := 1 << z
		def.TileMatrices = append(def.TileMatrices, tileMatrix{
			ID:               strconv.Itoa(z),
			ScaleDenominator: 559082264.0287178 / float64(dim),
			CellSize:         156543.03392804097 / float64(dim),
			PointOfOrigin:    []float64{-20037508.342789244, 20037508.342789244},
			TileWidth:        256,
			TileHeight:       256,
			MatrixWidth:      dim,
			MatrixHeight:     dim,
		})
	}
	return def
}
