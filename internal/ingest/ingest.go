package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/paulmach/orb"
	orbgeojson "github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/simplify"
	"github.com/wroge/wgs84/v2"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/geo"
	"github.com/adonm/postfgb/internal/store"
)

var ErrInvalid = errors.New("invalid upload")

const (
	sortWindowBytes  = 8 << 20
	targetChunkBytes = 2 << 20
	maxChunkFeatures = 50000
)

type sizer struct {
	est       int
	threshold int
	ratio     float64
}

func newSizer() *sizer {
	return &sizer{threshold: targetChunkBytes, ratio: 1}
}

func (s *sizer) add(f chunk.InputFeature) {
	s.est += featureEstimate(f)
}

func featureEstimate(f chunk.InputFeature) int {
	n := 52 + len(f.Geometry.Coordinates)
	for _, value := range f.Properties {
		n += len(value) + 3
	}
	return n
}

func (s *sizer) full(features int) bool {
	return s.est >= s.threshold || features >= maxChunkFeatures
}

func (s *sizer) reset(encoded int, features int) {
	if features > 0 && s.est > 0 {
		s.ratio = 0.6*s.ratio + 0.4*float64(encoded)/float64(s.est)
		s.threshold = int(float64(targetChunkBytes) / s.ratio)
	}
	s.est = 0
}

type Options struct {
	Collection string
	Sortable   []string
	IDColumn   string
	TimeColumn string
	Flatten    bool
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
	idColumn := opts.IDColumn
	if idColumn == "" {
		idColumn = detectIDColumn(cols)
	} else if !hasColumn(cols, idColumn) {
		return Result{}, invalid(fmt.Errorf("id column %q is not in the schema", idColumn))
	}
	timeColumn := opts.TimeColumn
	if timeColumn == "" {
		timeColumn = detectTimeColumn(cols)
	} else if !hasColumn(cols, timeColumn) {
		return Result{}, invalid(fmt.Errorf("time column %q is not in the schema", timeColumn))
	}
	_, err = st.Collection(ctx, opts.Collection)
	created := errors.Is(err, store.ErrNotFound)
	ing, err := st.BeginIngest(ctx, opts.Collection)
	if err != nil {
		return Result{}, err
	}
	defer ing.Rollback(ctx)
	result := Result{Created: created}
	sizer := newSizer()
	batch := make([]chunk.InputFeature, 0, 1024)
	window := make([]chunk.InputFeature, 0, 4096)
	windowBytes := 0
	var timeMin, timeMax *time.Time
	jsonDecided := false
	flush := func() error {
		if !jsonDecided {
			decideJSONColumns(cols, batch)
			rewriteJSONValues(cols, batch)
			if opts.Flatten {
				cols = flattenJSONColumns(cols, batch)
			}
			for _, name := range opts.Sortable {
				if !hasColumn(cols, name) {
					return invalid(fmt.Errorf("sortable column %q is not in the schema", name))
				}
			}
			jsonDecided = true
		} else {
			rewriteJSONValues(cols, batch)
		}
		encoded, err := putChunk(ctx, ing, opts.Collection, cols, batch, 0)
		if err != nil {
			return err
		}
		sizer.reset(encoded, len(batch))
		batch = batch[:0]
		result.Chunks++
		return nil
	}
	drain := func() error {
		sortWindow(window)
		for i := range window {
			batch = append(batch, window[i])
			sizer.add(window[i])
			if sizer.full(len(batch)) {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		window = window[:0]
		windowBytes = 0
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
		window = append(window, feature)
		windowBytes += featureEstimate(feature)
		result.Features++
		if timeColumn != "" {
			if t, ok := featureTime(feature.Properties[timeColumn]); ok {
				if timeMin == nil || t.Before(*timeMin) {
					timeMin = &t
				}
				if timeMax == nil || t.After(*timeMax) {
					timeMax = &t
				}
			}
		}
		if windowBytes >= sortWindowBytes {
			if err := drain(); err != nil {
				return Result{}, err
			}
		}
	}
	if err := drain(); err != nil {
		return Result{}, err
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
	if err := ing.SetCatalog(ctx, opts.Sortable, idColumn, timeColumn, timeMin, timeMax); err != nil {
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

const minJSONSample = 8

func decideJSONColumns(cols []chunk.Column, features []chunk.InputFeature) {
	jsonish := map[string]int{}
	seen := map[string]int{}
	for i := range features {
		for j := range cols {
			col := &cols[j]
			if col.Type != flattypes.ColumnTypeString {
				continue
			}
			raw, ok := features[i].Properties[col.Name]
			if !ok || len(raw) == 0 || string(raw) == "null" {
				continue
			}
			seen[col.Name]++
			if _, ok := jsonText(raw); ok {
				jsonish[col.Name]++
			}
		}
	}
	for i := range cols {
		col := &cols[i]
		if col.Type != flattypes.ColumnTypeString {
			continue
		}
		if seen[col.Name] >= minJSONSample && jsonish[col.Name]*2 >= seen[col.Name] {
			col.Type = flattypes.ColumnTypeJson
		}
	}
}

func rewriteJSONValues(cols []chunk.Column, features []chunk.InputFeature) {
	for i := range cols {
		if cols[i].Type != flattypes.ColumnTypeJson {
			continue
		}
		for j := range features {
			raw, ok := features[j].Properties[cols[i].Name]
			if !ok {
				continue
			}
			if inner, ok := jsonText(raw); ok {
				features[j].Properties[cols[i].Name] = inner
			}
		}
	}
}

const (
	minFlattenFeatures = 2
	maxFlattenKeys     = 24
	maxFlattenColumns  = 64
)

type flattenStat struct {
	count int
	kind  string
}

func flattenJSONColumns(cols []chunk.Column, features []chunk.InputFeature) []chunk.Column {
	var parents []string
	for _, col := range cols {
		if col.Type == flattypes.ColumnTypeJson {
			parents = append(parents, col.Name)
		}
	}
	if len(parents) == 0 {
		return cols
	}
	stats := map[string]map[string]*flattenStat{}
	seen := map[string]bool{}
	for _, col := range cols {
		seen[col.Name] = true
	}
	for i := range features {
		for _, parent := range parents {
			raw, ok := features[i].Properties[parent]
			if !ok {
				continue
			}
			object, ok := jsonObject(raw)
			if !ok {
				continue
			}
			for key, value := range object {
				kind, ok := scalarKind(value)
				if !ok {
					continue
				}
				name := flattenName(parent, key)
				if name == "" || seen[name] {
					continue
				}
				if stats[parent] == nil {
					stats[parent] = map[string]*flattenStat{}
				}
				stat := stats[parent][name]
				if stat == nil {
					stat = &flattenStat{kind: kind}
					stats[parent][name] = stat
				}
				stat.count++
				stat.kind = mergeKind(stat.kind, kind)
			}
		}
	}
	type candidate struct {
		parent string
		name   string
		kind   string
		count  int
	}
	var selected []candidate
	for parent, byName := range stats {
		names := make([]string, 0, len(byName))
		for name := range byName {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool {
			if byName[names[i]].count != byName[names[j]].count {
				return byName[names[i]].count > byName[names[j]].count
			}
			return names[i] < names[j]
		})
		kept := 0
		for _, name := range names {
			stat := byName[name]
			if stat.count < minFlattenFeatures || kept == maxFlattenKeys {
				continue
			}
			selected = append(selected, candidate{parent: parent, name: name, kind: stat.kind, count: stat.count})
			kept++
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].count != selected[j].count {
			return selected[i].count > selected[j].count
		}
		return selected[i].name < selected[j].name
	})
	if len(selected) > maxFlattenColumns {
		selected = selected[:maxFlattenColumns]
	}
	if len(selected) == 0 {
		return cols
	}
	kinds := map[string]flattypes.ColumnType{}
	for _, item := range selected {
		kinds[item.name] = flattenType(item.kind)
		cols = append(cols, chunk.Column{Name: item.name, Type: kinds[item.name]})
	}
	for i := range features {
		for _, parent := range parents {
			raw, ok := features[i].Properties[parent]
			if !ok {
				continue
			}
			object, ok := jsonObject(raw)
			if !ok {
				continue
			}
			for key, value := range object {
				name := flattenName(parent, key)
				if _, ok := kinds[name]; !ok {
					continue
				}
				features[i].Properties[name] = value
			}
		}
	}
	return cols
}

func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil, false
	}
	return object, true
}

func scalarKind(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	switch raw[0] {
	case '"':
		return "string", true
	case 't', 'f':
		if string(raw) == "true" || string(raw) == "false" {
			return "bool", true
		}
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var number json.Number
		if json.Unmarshal(raw, &number) != nil {
			return "", false
		}
		if strings.ContainsAny(string(number), ".eE") {
			return "number", true
		}
		return "integer", true
	}
	return "", false
}

func mergeKind(a, b string) string {
	if a == b {
		return a
	}
	if a == "array" || b == "array" {
		return "array"
	}
	if (a == "integer" && b == "number") || (a == "number" && b == "integer") {
		return "number"
	}
	return "string"
}

func flattenType(kind string) flattypes.ColumnType {
	switch kind {
	case "array":
		return flattypes.ColumnTypeJson
	case "integer":
		return flattypes.ColumnTypeLong
	case "number":
		return flattypes.ColumnTypeDouble
	case "bool":
		return flattypes.ColumnTypeBool
	}
	return flattypes.ColumnTypeString
}

func flattenName(parent, key string) string {
	var b strings.Builder
	b.WriteString(parent)
	b.WriteByte('_')
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func jsonText(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) < 3 || raw[0] != '"' {
		return nil, false
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return nil, false
	}
	trimmed := json.RawMessage(strings.TrimSpace(text))
	if len(trimmed) < 2 || (trimmed[0] != '[' && trimmed[0] != '{') {
		return nil, false
	}
	if !json.Valid(trimmed) {
		return nil, false
	}
	return trimmed, true
}

type columnInfo struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func putChunk(ctx context.Context, ing *store.Ingest, collection string, cols []chunk.Column, batch []chunk.InputFeature, tier int) (int, error) {
	data, err := chunk.EncodeCols(collection, cols, batch)
	if err != nil {
		return 0, err
	}
	c, err := chunk.Open(data)
	if err != nil {
		return 0, err
	}
	env, ok := c.Envelope()
	if !ok {
		return 0, errors.New("ingest: chunk missing envelope")
	}
	sum := sha256.Sum256(data)
	columns, err := json.Marshal(columnList(c.Columns()))
	if err != nil {
		return 0, err
	}
	headerBlob, leaves, featureBytes, ok := fgb.Layout(data, c.Header())
	if !ok {
		return 0, errors.New("ingest: chunk layout unreadable")
	}
	payload := store.Payload{
		Data: chunk.Compress(data), Size: len(data), Count: c.Count(), Tier: tier,
		MinX: env[0], MinY: env[1], MaxX: env[2], MaxY: env[3],
		Checksum:     hex.EncodeToString(sum[:]),
		Columns:      string(columns),
		FeatureBytes: featureBytes,
		HeaderBlob:   headerBlob,
		Leaves:       leaves,
	}
	id, err := ing.PutChunk(ctx, payload)
	if err != nil {
		return 0, err
	}
	if tier > 0 {
		return len(data), nil
	}
	props := make([]string, len(batch))
	for i := range batch {
		body, err := json.Marshal(batch[i].Properties)
		if err != nil {
			return 0, err
		}
		props[i] = string(body)
	}
	if err := ing.PutChunkProps(ctx, id, props); err != nil {
		return 0, err
	}
	return len(data), nil
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
	specs := []*tierSpec{
		{tier: 1, stride: 10, tol: 0.01, sizer: newSizer(), batch: make([]chunk.InputFeature, 0, 4096)},
		{tier: 2, stride: 100, tol: 0.05, sizer: newSizer(), batch: make([]chunk.InputFeature, 0, 4096)},
	}
	flush := func(spec *tierSpec) error {
		if len(spec.batch) == 0 {
			return nil
		}
		encoded, err := putChunk(ctx, ing, collection, cols, spec.batch, spec.tier)
		if err != nil {
			return err
		}
		spec.sizer.reset(encoded, len(spec.batch))
		spec.batch = spec.batch[:0]
		return nil
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
		for _, spec := range specs {
			for i, f := range feats {
				if i%spec.stride != 0 {
					continue
				}
				feature, ok := tierFeature(c, f, spec)
				if !ok {
					continue
				}
				spec.batch = append(spec.batch, feature)
				spec.sizer.add(feature)
				if spec.sizer.full(len(spec.batch)) {
					if err := flush(spec); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, spec := range specs {
		if err := flush(spec); err != nil {
			return err
		}
	}
	return nil
}

type tierSpec struct {
	tier   int
	stride int
	tol    float64
	batch  []chunk.InputFeature
	sizer  *sizer
}

func tierFeature(c *chunk.Chunk, f *flattypes.Feature, spec *tierSpec) (chunk.InputFeature, bool) {
	g := c.GeoJSON(f)
	geom := geo.ToOrb(g.Geometry)
	if geom == nil {
		return chunk.InputFeature{}, false
	}
	if _, ok := geom.(orb.Collection); ok {
		return chunk.InputFeature{}, false
	}
	simp := geom
	if !pointFamily(geom) {
		bound := geom.Bound()
		if math.Hypot(bound.Max[0]-bound.Min[0], bound.Max[1]-bound.Min[1]) > spec.tol*4 {
			simp = simplify.DouglasPeucker(spec.tol).Simplify(geom)
			if simp == nil {
				return chunk.InputFeature{}, false
			}
		}
	}
	body, err := json.Marshal(orbgeojson.NewGeometry(simp))
	if err != nil {
		return chunk.InputFeature{}, false
	}
	var raw chunk.RawGeometry
	if err := json.Unmarshal(body, &raw); err != nil {
		return chunk.InputFeature{}, false
	}
	props := make(map[string]json.RawMessage, len(g.Properties))
	for name, value := range g.Properties {
		encoded, err := json.Marshal(value)
		if err != nil {
			continue
		}
		props[name] = encoded
	}
	return chunk.InputFeature{Geometry: raw, Properties: props}, true
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
		if err := TransformGeometry(g.Geometry, transform); err != nil {
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

func TransformGeometry(g *chunk.Geometry, fn wgs84.Func) error {
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

func featureTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
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
		cast := cql.CastNone
		for _, col := range cols {
			if col.Name == name {
				cast = store.CastOf(col.Type.String())
				break
			}
		}
		out = append(out, store.SortColumn{Name: name, Cast: cast})
	}
	return out
}
