package api

import (
	"context"
	"errors"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/adonm/postfgb/internal/store"
)

const (
	webMercatorQuadID  = "WebMercatorQuad"
	webMercatorQuadURI = "http://www.opengis.net/def/tilematrixset/OGC/1.0/WebMercatorQuad"
	tmsEPSG3857        = "http://www.opengis.net/def/crs/EPSG/0/3857"
)

type tileset struct {
	Title            string `json:"title,omitempty"`
	DataType         string `json:"dataType"`
	CRS              string `json:"crs"`
	TileMatrixSetURI string `json:"tileMatrixSetURI,omitempty"`
	Links            []link `json:"links"`
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
	out.Body.Tilesets = []tileset{{
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
	}}
	out.Body.Links = []link{{Href: "/collections/" + in.ID + "/tiles", Rel: "self", Type: "application/json"}}
	return out, nil
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
