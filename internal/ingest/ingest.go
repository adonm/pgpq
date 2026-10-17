package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/paulmach/orb"
	orbgeojson "github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/simplify"
	"github.com/wroge/wgs84/v2"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cover"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/store"
	"github.com/adonm/postfgb/internal/tiles"
)

var ErrInvalid = errors.New("invalid upload")

const chunkSize = 50000

type Options struct {
	Collection string
	Sortable   []string
}

type Result struct {
	Features int
	Chunks   int
	Created  bool
}

func Run(ctx context.Context, st *store.Store, body io.Reader, opts Options) (Result, error) {
	reader, err := fgb.NewReader(body)
	if err != nil {
		return Result{}, invalid(err)
	}
	header := reader.Header()
	cols := chunk.HeaderColumns(header)
	transform, err := reproject(header)
	if err != nil {
		return Result{}, invalid(err)
	}
	idColumn := detectIDColumn(cols)
	timeColumn := detectTimeColumn(cols)
	for _, name := range opts.Sortable {
		if !hasColumn(cols, name) {
			return Result{}, invalid(fmt.Errorf("sortable column %q is not in the schema", name))
		}
	}
	_, err = st.Collection(ctx, opts.Collection)
	created := errors.Is(err, store.ErrNotFound)
	ing, err := st.BeginIngest(ctx, opts.Collection)
	if err != nil {
		return Result{}, err
	}
	defer ing.Rollback(ctx)
	result := Result{Created: created}
	batch := make([]chunk.InputFeature, 0, chunkSize)
	flush := func() error {
		if err := putChunk(ctx, ing, opts.Collection, cols, batch, 0); err != nil {
			return err
		}
		result.Chunks++
		return nil
	}
	for {
		f, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, invalid(err)
		}
		feature, err := toInput(header, f, transform)
		if err != nil {
			return Result{}, invalid(err)
		}
		batch = append(batch, feature)
		result.Features++
		if len(batch) == chunkSize {
			if err := flush(); err != nil {
				return Result{}, err
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := flush(); err != nil {
			return Result{}, err
		}
	}
	if result.Features == 0 {
		return Result{}, invalid(errors.New("upload contains no features"))
	}
	if err := overviews(ctx, ing, opts.Collection, cols); err != nil {
		return Result{}, err
	}
	if err := ing.CreateSortIndexes(ctx, sortColumns(cols, opts.Sortable)); err != nil {
		return Result{}, err
	}
	if err := ing.SetCatalog(ctx, opts.Sortable, idColumn, timeColumn); err != nil {
		return Result{}, err
	}
	if err := ing.RefreshStats(ctx); err != nil {
		return Result{}, err
	}
	if err := ing.Commit(ctx); err != nil {
		return Result{}, err
	}
	return result, nil
}

func invalid(err error) error {
	return fmt.Errorf("%w: %v", ErrInvalid, err)
}

type columnInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func putChunk(ctx context.Context, ing *store.Ingest, collection string, cols []chunk.Column, batch []chunk.InputFeature, tier int) error {
	data, err := chunk.EncodeCols(collection, cols, batch)
	if err != nil {
		return err
	}
	c, err := chunk.Open(data)
	if err != nil {
		return err
	}
	env, ok := c.Envelope()
	if !ok {
		return errors.New("ingest: chunk missing envelope")
	}
	sum := sha256.Sum256(data)
	columns, err := json.Marshal(columnList(c.Columns()))
	if err != nil {
		return err
	}
	payload := store.Payload{
		Data: data, Count: c.Count(), Tier: tier,
		MinX: env[0], MinY: env[1], MaxX: env[2], MaxY: env[3],
		Cover:    cover.Chunk(env[0], env[1], env[2], env[3]),
		Checksum: hex.EncodeToString(sum[:]),
		Columns:  string(columns),
	}
	id, err := ing.PutChunk(ctx, payload)
	if err != nil {
		return err
	}
	if tier > 0 {
		return nil
	}
	docs := make([]string, len(batch))
	for i := range batch {
		docs[i] = chunk.Text(batch[i].Properties)
	}
	if err := ing.PutChunkText(ctx, id, docs); err != nil {
		return err
	}
	props := make([]string, len(batch))
	for i := range batch {
		body, err := json.Marshal(batch[i].Properties)
		if err != nil {
			return err
		}
		props[i] = string(body)
	}
	return ing.PutChunkProps(ctx, id, props)
}

func columnList(cols []chunk.Column) []columnInfo {
	list := make([]columnInfo, len(cols))
	for i, col := range cols {
		list[i] = columnInfo{Name: col.Name, Type: col.Type.String()}
	}
	return list
}

func overviews(ctx context.Context, ing *store.Ingest, collection string, cols []chunk.Column) error {
	rows, err := ing.Chunks(ctx, nil, 1000000, 0)
	if err != nil {
		return err
	}
	if err := ing.DeleteChunksAboveTier(ctx, 0); err != nil {
		return err
	}
	tiers := []struct {
		tier    int
		stride  int
		tol     float64
		minDiag float64
	}{
		{tier: 1, stride: 10, tol: 0.01, minDiag: 0.05},
		{tier: 2, stride: 100, tol: 0.05, minDiag: 0.2},
	}
	for _, row := range rows {
		c, err := chunk.Open(row.Data)
		if err != nil {
			return err
		}
		feats, err := c.SearchAll()
		if err != nil {
			return err
		}
		for _, spec := range tiers {
			batch := make([]chunk.InputFeature, 0, len(feats)/spec.stride+1)
			for i, f := range feats {
				if i%spec.stride != 0 {
					continue
				}
				g := c.GeoJSON(f)
				geom := tiles.ToOrb(g.Geometry)
				if geom == nil {
					continue
				}
				if _, ok := geom.(orb.Collection); ok {
					continue
				}
				simp := geom
				if !pointFamily(geom) {
					simp = simplify.DouglasPeucker(spec.tol).Simplify(geom)
					if simp == nil {
						continue
					}
					bound := simp.Bound()
					dx := bound.Max[0] - bound.Min[0]
					dy := bound.Max[1] - bound.Min[1]
					if dx*dx+dy*dy < spec.minDiag*spec.minDiag {
						continue
					}
				}
				body, err := json.Marshal(orbgeojson.NewGeometry(simp))
				if err != nil {
					continue
				}
				var raw chunk.RawGeometry
				if err := json.Unmarshal(body, &raw); err != nil {
					continue
				}
				props := make(map[string]json.RawMessage, len(g.Properties))
				for name, value := range g.Properties {
					encoded, err := json.Marshal(value)
					if err != nil {
						continue
					}
					props[name] = encoded
				}
				batch = append(batch, chunk.InputFeature{Geometry: raw, Properties: props})
			}
			if len(batch) == 0 {
				continue
			}
			if err := putChunk(ctx, ing, collection, cols, batch, spec.tier); err != nil {
				return err
			}
		}
	}
	return nil
}

func pointFamily(g orb.Geometry) bool {
	switch g.(type) {
	case orb.Point, orb.MultiPoint:
		return true
	default:
		return false
	}
}

func toInput(header *flattypes.Header, f *flattypes.Feature, transform wgs84.Func) (chunk.InputFeature, error) {
	g := chunk.GeoJSONFeature(header, f)
	if g.Geometry == nil {
		return chunk.InputFeature{}, errors.New("feature missing geometry")
	}
	if g.Geometry.Type == "GeometryCollection" {
		return chunk.InputFeature{}, errors.New("geometry collections are not supported")
	}
	if transform != nil {
		if err := transformGeometry(g.Geometry, transform); err != nil {
			return chunk.InputFeature{}, err
		}
	}
	raw, err := json.Marshal(g.Geometry.Coordinates)
	if err != nil {
		return chunk.InputFeature{}, err
	}
	props := make(map[string]json.RawMessage, len(g.Properties))
	for name, value := range g.Properties {
		encoded, err := json.Marshal(value)
		if err != nil {
			return chunk.InputFeature{}, err
		}
		props[name] = encoded
	}
	return chunk.InputFeature{
		Geometry:   chunk.RawGeometry{Type: g.Geometry.Type, Coordinates: raw},
		Properties: props,
	}, nil
}

func transformGeometry(g *chunk.Geometry, fn wgs84.Func) error {
	switch c := g.Coordinates.(type) {
	case []float64:
		return transformPoint(c, fn)
	case [][]float64:
		for _, p := range c {
			if err := transformPoint(p, fn); err != nil {
				return err
			}
		}
	case [][][]float64:
		for _, r := range c {
			for _, p := range r {
				if err := transformPoint(p, fn); err != nil {
					return err
				}
			}
		}
	case [][][][]float64:
		for _, poly := range c {
			for _, r := range poly {
				for _, p := range r {
					if err := transformPoint(p, fn); err != nil {
						return err
					}
				}
			}
		}
	default:
		return fmt.Errorf("unsupported coordinates %T", g.Coordinates)
	}
	return nil
}

func transformPoint(p []float64, fn wgs84.Func) error {
	if len(p) < 2 {
		return errors.New("invalid coordinate")
	}
	x, y, _, err := fn(p[0], p[1], 0)
	if err != nil {
		return err
	}
	p[0], p[1] = x, y
	return nil
}

var (
	epsgCodeString = regexp.MustCompile(`(?i)epsg:\s*:?\s*(\d{3,6})`)
	epsgWkt        = regexp.MustCompile(`(?i)AUTHORITY\s*\[\s*"EPSG"\s*,\s*"?(\d{3,6})`)
)

func reproject(header *flattypes.Header) (wgs84.Func, error) {
	code, present, err := crsCode(header)
	if err != nil {
		return nil, err
	}
	if !present || code == 4326 {
		return nil, nil
	}
	from, ok := wgs84.EPSG[code]
	if !ok {
		return nil, fmt.Errorf("unsupported CRS EPSG:%d", code)
	}
	return wgs84.Transform(from, wgs84.EPSG[4326])
}

func crsCode(header *flattypes.Header) (int, bool, error) {
	var crs flattypes.Crs
	if header.Crs(&crs) == nil {
		return 0, false, nil
	}
	if code := int(crs.Code()); code > 0 && strings.EqualFold(string(crs.Org()), "EPSG") {
		return code, true, nil
	}
	for _, value := range []string{string(crs.CodeString()), string(crs.Wkt())} {
		if value == "" {
			continue
		}
		for _, re := range []*regexp.Regexp{epsgCodeString, epsgWkt} {
			if m := re.FindStringSubmatch(value); m != nil {
				if code, err := strconv.Atoi(m[1]); err == nil && code > 0 {
					return code, true, nil
				}
			}
		}
	}
	return 0, false, errors.New("cannot determine the CRS of the upload")
}

var idNames = []string{"id", "fid", "gid", "ogc_fid"}

func detectIDColumn(cols []chunk.Column) string {
	for _, name := range idNames {
		for _, col := range cols {
			if strings.EqualFold(col.Name, name) {
				return col.Name
			}
		}
	}
	return ""
}

func detectTimeColumn(cols []chunk.Column) string {
	for _, col := range cols {
		if col.Type == flattypes.ColumnTypeDateTime {
			return col.Name
		}
	}
	return ""
}

func hasColumn(cols []chunk.Column, name string) bool {
	for _, col := range cols {
		if col.Name == name {
			return true
		}
	}
	return false
}

func sortColumns(cols []chunk.Column, sortable []string) []store.SortColumn {
	out := make([]store.SortColumn, 0, len(sortable))
	for _, name := range sortable {
		cast := ""
		for _, col := range cols {
			if col.Name != name {
				continue
			}
			switch col.Type {
			case flattypes.ColumnTypeBool:
				cast = "boolean"
			case flattypes.ColumnTypeInt, flattypes.ColumnTypeUInt, flattypes.ColumnTypeLong,
				flattypes.ColumnTypeULong, flattypes.ColumnTypeFloat, flattypes.ColumnTypeDouble:
				cast = "numeric"
			case flattypes.ColumnTypeDateTime:
				cast = "timestamptz"
			}
			break
		}
		out = append(out, store.SortColumn{Name: name, Cast: cast})
	}
	return out
}
