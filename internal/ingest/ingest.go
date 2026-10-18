package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/simplify"
	"github.com/wroge/wgs84/v2"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/stats"
	"github.com/adonm/postfgb/internal/store"
)

var ErrInvalid = errors.New("invalid upload")

const (
	sortWindowBytes  = 8 << 20
	targetChunkBytes = 2 << 20
	maxChunkFeatures = 50000
	maxChunkBytes    = 8 << 20
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
	n := 52
	if f.Parsed != nil {
		n += f.Parsed.Estimate()
	} else {
		n += len(f.Geometry.Coordinates)
	}
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
	IDColumn   string
	TimeColumn string
	Flatten    bool
}

type Result struct {
	Features int
	Chunks   int
	Created  bool
}

type prepResult struct {
	payload  store.Payload
	rows     []stats.Row
	prepare  time.Duration
	stats    time.Duration
	compress time.Duration
	err      error
}

type prepRequest struct {
	raw  []byte
	tier int
	out  chan prepResult
}

type future struct {
	req *prepRequest
	raw []byte
}

type pipeline struct {
	ctx        context.Context
	ing        *store.Ingest
	collection string
	jobs       chan *prepRequest
	futures    []*future
	limit      int
	workers    sync.WaitGroup
	once       sync.Once
	encode     atomic.Int64
	prepared   atomic.Int64
	stats      atomic.Int64
	compress   atomic.Int64
	store      atomic.Int64
}

func newPipeline(ctx context.Context, ing *store.Ingest, collection string) *pipeline {
	workers := min(runtime.NumCPU(), 8)
	p := &pipeline{
		ctx: ctx, ing: ing, collection: collection,
		jobs:  make(chan *prepRequest, workers),
		limit: workers * 2,
	}
	p.workers.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer p.workers.Done()
			for req := range p.jobs {
				req.out <- p.prepare(req.raw, req.tier)
			}
		}()
	}
	return p
}

func (p *pipeline) submit(raw []byte, tier int) error {
	req := &prepRequest{raw: raw, tier: tier, out: make(chan prepResult, 1)}
	p.jobs <- req
	p.futures = append(p.futures, &future{req: req, raw: raw})
	if len(p.futures) >= p.limit {
		return p.drain()
	}
	return nil
}

func (p *pipeline) drain() error {
	for len(p.futures) > 0 {
		f := p.futures[0]
		p.futures = p.futures[1:]
		res := <-f.req.out
		if res.err != nil {
			return res.err
		}
		p.prepared.Add(int64(res.prepare))
		p.stats.Add(int64(res.stats))
		p.compress.Add(int64(res.compress))
		start := time.Now()
		id, err := p.ing.PutChunk(p.ctx, res.payload)
		if err != nil {
			return err
		}
		if err := p.ing.PutChunkStats(p.ctx, id, res.rows); err != nil {
			return err
		}
		p.store.Add(int64(time.Since(start)))
	}
	return nil
}

func (p *pipeline) finish() error {
	var err error
	p.once.Do(func() {
		err = p.drain()
		close(p.jobs)
		p.workers.Wait()
	})
	return err
}

func (p *pipeline) prepare(raw []byte, tier int) prepResult {
	var res prepResult
	start := time.Now()
	c, err := chunk.FromRaw(raw)
	if err != nil {
		res.err = err
		return res
	}
	env, ok := c.Envelope()
	if !ok {
		res.err = errors.New("ingest: chunk missing envelope")
		return res
	}
	columns, err := json.Marshal(columnList(c.Columns()))
	if err != nil {
		res.err = err
		return res
	}
	headerBlob, leaves, featureBytes, ok := fgb.Layout(raw, c.Header())
	if !ok {
		res.err = errors.New("ingest: chunk layout unreadable")
		return res
	}
	rows, err := chunkStats(c)
	if err != nil {
		res.err = err
		return res
	}
	res.stats = time.Since(start)
	sum := sha256.Sum256(raw)
	compressStart := time.Now()
	res.payload = store.Payload{
		Data: chunk.Compress(raw), Size: len(raw), Count: c.Count(), Tier: tier,
		MinX: env[0], MinY: env[1], MaxX: env[2], MaxY: env[3],
		Checksum:     hex.EncodeToString(sum[:]),
		Columns:      string(columns),
		FeatureBytes: featureBytes,
		HeaderBlob:   headerBlob,
		Leaves:       leaves,
	}
	res.compress = time.Since(compressStart)
	res.rows = rows
	res.prepare = time.Since(start)
	return res
}

func Run(ctx context.Context, st *store.Store, body io.Reader, opts Options) (Result, error) {
	started := time.Now()
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
	if err := ing.StageFeatureBase(ctx); err != nil {
		return Result{}, err
	}
	result := Result{Created: created}
	sizer := newSizer()
	batch := make([]chunk.InputFeature, 0, 1024)
	window := make([]chunk.InputFeature, 0, 4096)
	windowBytes := 0
	var timeMin, timeMax *time.Time
	jsonDecided := false
	var flat flattening
	var stream, tiers time.Duration
	p := newPipeline(ctx, ing, opts.Collection)
	defer p.finish()
	specs := []*tierSpec{
		{tier: 1, stride: 10, tol: 0.01, sizer: newSizer(), batch: make([]chunk.InputFeature, 0, 4096)},
		{tier: 2, stride: 100, tol: 0.05, sizer: newSizer(), batch: make([]chunk.InputFeature, 0, 4096)},
	}
	flush := func() error {
		data, err := p.putChunk(opts.Collection, cols, batch, 0)
		if err != nil {
			return err
		}
		sizer.reset(len(data), len(batch))
		batch = batch[:0]
		result.Chunks++
		tierStart := time.Now()
		for _, spec := range specs {
			if err := spec.sample(ctx, p, opts.Collection, cols, data); err != nil {
				return err
			}
		}
		tiers += time.Since(tierStart)
		return nil
	}
	drain := func() error {
		sortWindow(window)
		if !jsonDecided {
			decideJSONColumns(cols, window)
			rewriteJSONValues(cols, window)
			if opts.Flatten {
				cols, flat = flattenJSONColumns(cols, window)
			}
			jsonDecided = true
		} else {
			rewriteJSONValues(cols, window)
		}
		flat.apply(window)
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
		loopStart := time.Now()
		f, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, invalid(err)
		}
		feature, err := toInput(header, f, transform)
		stream += time.Since(loopStart)
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
	tierStart := time.Now()
	for _, spec := range specs {
		if err := spec.flush(ctx, p, opts.Collection, cols); err != nil {
			return Result{}, err
		}
	}
	tiers += time.Since(tierStart)
	if err := p.finish(); err != nil {
		return Result{}, err
	}
	if err := ing.ComputeFeatureBase(ctx); err != nil {
		return Result{}, err
	}
	if err := ing.SetCatalog(ctx, idColumn, timeColumn, timeMin, timeMax); err != nil {
		return Result{}, err
	}
	commitStart := time.Now()
	if err := ing.Commit(ctx); err != nil {
		return Result{}, err
	}
	commit := time.Since(commitStart)
	slog.Info("ingest", "collection", opts.Collection, "features", result.Features, "chunks", result.Chunks,
		"stream", stream, "encode", time.Duration(p.encode.Load()), "prepare_sum", time.Duration(p.prepared.Load()),
		"stats_sum", time.Duration(p.stats.Load()), "compress_sum", time.Duration(p.compress.Load()),
		"store", time.Duration(p.store.Load()), "tiers", tiers, "commit", commit, "total", time.Since(started))
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

type flattening struct {
	parents []string
	names   map[string]bool
}

func flattenJSONColumns(cols []chunk.Column, features []chunk.InputFeature) ([]chunk.Column, flattening) {
	var parents []string
	for _, col := range cols {
		if col.Type == flattypes.ColumnTypeJson {
			parents = append(parents, col.Name)
		}
	}
	if len(parents) == 0 {
		return cols, flattening{}
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
		return cols, flattening{}
	}
	names := make(map[string]bool, len(selected))
	for _, item := range selected {
		names[item.name] = true
		cols = append(cols, chunk.Column{Name: item.name, Type: flattenType(item.kind)})
	}
	return cols, flattening{parents: parents, names: names}
}

func (f flattening) apply(features []chunk.InputFeature) {
	for i := range features {
		for _, parent := range f.parents {
			raw, ok := features[i].Properties[parent]
			if !ok {
				continue
			}
			object, ok := jsonObject(raw)
			if !ok {
				continue
			}
			for key, value := range object {
				if name := flattenName(parent, key); f.names[name] {
					features[i].Properties[name] = value
				}
			}
		}
	}
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

func (p *pipeline) putChunk(collection string, cols []chunk.Column, batch []chunk.InputFeature, tier int) ([]byte, error) {
	var last []byte
	err := encodeChunks(collection, cols, batch, maxChunkBytes, &p.encode, func(data []byte) error {
		last = data
		return p.submit(data, tier)
	})
	return last, err
}

func encodeChunks(collection string, cols []chunk.Column, batch []chunk.InputFeature, limit int, timing *atomic.Int64, emit func([]byte) error) error {
	encodeStart := time.Now()
	data, err := chunk.EncodeCols(collection, cols, batch)
	if timing != nil {
		timing.Add(int64(time.Since(encodeStart)))
	}
	if err != nil {
		return err
	}
	if len(data) > limit && len(batch) > 1 {
		mid := len(batch) / 2
		if err := encodeChunks(collection, cols, batch[:mid], limit, timing, emit); err != nil {
			return err
		}
		return encodeChunks(collection, cols, batch[mid:], limit, timing, emit)
	}
	return emit(data)
}

func chunkStats(c *chunk.Chunk) ([]stats.Row, error) {
	features, err := c.SearchAll()
	if err != nil {
		return nil, err
	}
	cols := c.Columns()
	props := make([]map[string]any, len(features))
	for i, f := range features {
		props[i] = chunk.DecodeProperties(f.PropertiesBytes(), cols)
	}
	return stats.Compute(cols, props), nil
}

func columnList(cols []chunk.Column) []columnInfo {
	list := make([]columnInfo, len(cols))
	for i, col := range cols {
		list[i] = columnInfo{Name: col.Name, Type: col.Type.String()}
	}
	return list
}

func (s *tierSpec) sample(ctx context.Context, p *pipeline, collection string, cols []chunk.Column, data []byte) error {
	c, err := chunk.Open(data)
	if err != nil {
		return err
	}
	feats, err := c.SearchAll()
	if err != nil {
		return err
	}
	for i, f := range feats {
		if i%s.stride != 0 {
			continue
		}
		feature, ok := tierFeature(c, f, s)
		if !ok {
			continue
		}
		s.batch = append(s.batch, feature)
		s.sizer.add(feature)
		if s.sizer.full(len(s.batch)) {
			if err := s.flush(ctx, p, collection, cols); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *tierSpec) flush(ctx context.Context, p *pipeline, collection string, cols []chunk.Column) error {
	if len(s.batch) == 0 {
		return nil
	}
	data, err := p.putChunk(collection, cols, s.batch, s.tier)
	if err != nil {
		return err
	}
	s.sizer.reset(len(data), len(s.batch))
	s.batch = s.batch[:0]
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
	geom := chunk.ToOrb(g.Geometry)
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
	shape, ok := chunk.FromOrb(simp)
	if !ok {
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
	return chunk.InputFeature{Parsed: shape, Properties: props}, true
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
	shape, err := chunk.ParseFlat(header, f)
	if err != nil {
		return chunk.InputFeature{}, err
	}
	if shape == nil {
		return chunk.InputFeature{}, errors.New("feature missing geometry")
	}
	if transform != nil {
		if err := shape.Transform(transform); err != nil {
			return chunk.InputFeature{}, err
		}
	}
	return chunk.InputFeature{
		Parsed:     shape,
		Properties: chunk.DecodePropertyText(f.PropertiesBytes(), chunk.HeaderColumns(header)),
	}, nil
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
