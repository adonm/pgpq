package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/store"
)

type featureQuery struct {
	bbox        *[4]float64
	tw          timeWindow
	filter      cql.Expr
	offset      int
	overview    *int
	cursor      *cursor
	fingerprint [16]byte
}

type hit struct {
	chunk *chunk.Chunk
	raw   *flattypes.Feature
	geo   *chunk.Feature
}

type sink interface {
	add(hit) error
	remaining() int
}

type startPos struct {
	chunkID int64
	ord     int
}

type walkResult struct {
	member int
	pos    startPos
	set    bool
}

var reservedParams = map[string]bool{
	"bbox": true, "limit": true, "offset": true, "f": true,
	"filter": true, "filter-lang": true, "format": true,
	"overview": true, "numberMatched": true,
	"datetime": true, "crs": true, "bbox-crs": true,
	"z": true, "x": true, "y": true, "cursor": true,
}

func parseFeatureQuery(r *http.Request, info store.CollectionInfo) (featureQuery, error) {
	var q featureQuery
	var err error
	if q.bbox, err = parseBBox(r); err != nil {
		return q, err
	}
	if q.overview, err = parseOverview(r); err != nil {
		return q, err
	}
	if q.filter, err = requestFilter(r); err != nil {
		return q, err
	}
	if q.tw, err = parseTimeWindow(r); err != nil {
		return q, err
	}
	if q.tw.set && info.Kind != "view" && (info.TimeColumn == nil || *info.TimeColumn == "") {
		return q, errors.New("collection has no time column")
	}
	q.fingerprint = requestFingerprint(r)
	if value := r.URL.Query().Get("cursor"); value != "" {
		c, err := decodeCursor(value)
		if err != nil {
			return q, err
		}
		if r.URL.Query().Has("offset") {
			return q, errors.New("cursor and offset cannot be combined")
		}
		q.cursor = &c
	}
	return q, nil
}

func checkOverview(q featureQuery) error {
	if q.overview != nil && *q.overview > 0 && (q.filter != nil || q.tw.set) {
		return errors.New("overview is only served without a filter")
	}
	return nil
}

func effectiveTier(info store.CollectionInfo, q featureQuery) int {
	if q.overview != nil && info.Kind != "view" {
		return *q.overview
	}
	return 0
}

func validateCursor(info store.CollectionInfo, q featureQuery) error {
	if q.cursor == nil {
		return nil
	}
	c := q.cursor
	if c.uid != info.UID || c.version != info.Version {
		return errors.New("cursor does not match the collection")
	}
	if c.fingerprint != q.fingerprint {
		return errors.New("cursor does not match the request")
	}
	if c.tier != effectiveTier(info, q) {
		return errors.New("cursor does not match the overview tier")
	}
	return nil
}

func parseOverview(r *http.Request) (*int, error) {
	switch value := r.URL.Query().Get("overview"); value {
	case "", "auto":
		return nil, nil
	case "0", "1", "2":
		n, _ := strconv.Atoi(value)
		return &n, nil
	default:
		return nil, errors.New("overview must be auto, 0, 1 or 2")
	}
}

func parseBBox(r *http.Request) (*[4]float64, error) {
	value := r.URL.Query().Get("bbox")
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	if len(parts) != 4 {
		return nil, errors.New("bbox must be minx,miny,maxx,maxy")
	}
	var bbox [4]float64
	for i, part := range parts {
		n, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil, errors.New("invalid bbox")
		}
		bbox[i] = n
	}
	return &bbox, nil
}

func parseLimit(r *http.Request, fallback int) (int, error) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 1000000 {
		return 0, errors.New("invalid limit")
	}
	return n, nil
}

func parseOffset(r *http.Request) (int, error) {
	value := r.URL.Query().Get("offset")
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, errors.New("invalid offset")
	}
	return n, nil
}

func parseFilters(r *http.Request) map[string]string {
	filters := map[string]string{}
	for name, values := range r.URL.Query() {
		if reservedParams[name] || len(values) == 0 {
			continue
		}
		filters[name] = values[0]
	}
	return filters
}

func requestFilter(r *http.Request) (cql.Expr, error) {
	var expr cql.Expr
	for name, value := range parseFilters(r) {
		expr = cql.And(expr, &cql.Compare{Prop: name, Op: "=", Value: typedLiteral(value)})
	}
	text := r.URL.Query().Get("filter")
	if text == "" {
		return expr, nil
	}
	parsed, err := cql.Parse(text)
	if err != nil {
		return nil, err
	}
	return cql.And(expr, parsed), nil
}

func typedLiteral(value string) any {
	switch value {
	case "true":
		return true
	case "false":
		return false
	}
	if n, ok := cql.Number(value); ok {
		return n
	}
	return value
}

func (s *Server) targets(ctx context.Context, info store.CollectionInfo) ([]store.CollectionInfo, error) {
	if info.Kind != "view" {
		return []store.CollectionInfo{info}, nil
	}
	names, err := s.Store.ViewMemberNames(ctx, info)
	if err != nil {
		return nil, fmt.Errorf("view %s members: %w", info.Name, err)
	}
	targets := make([]store.CollectionInfo, 0, len(names))
	for _, name := range names {
		member, err := s.Store.Collection(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("view %s member %s: %w", info.Name, name, err)
		}
		targets = append(targets, member)
	}
	return targets, nil
}

func targetFilter(target store.CollectionInfo, q featureQuery) (cql.Expr, bool) {
	if !q.tw.set {
		return q.filter, true
	}
	if target.TimeColumn == nil || *target.TimeColumn == "" {
		return nil, false
	}
	return cql.And(q.filter, datetimeExpr(*target.TimeColumn, q.tw)), true
}

func (s *Server) walkFeatures(ctx context.Context, info store.CollectionInfo, q featureQuery, out sink) (walkResult, error) {
	var res walkResult
	tier := effectiveTier(info, q)
	offset := q.offset
	targets, err := s.targets(ctx, info)
	if err != nil {
		return res, err
	}
	resume := q.cursor
	if resume != nil && (resume.member < 0 || resume.member >= len(targets)) {
		return res, errors.New("cursor does not match the collection")
	}
	for idx, target := range targets {
		if out.remaining() == 0 {
			return res, nil
		}
		if resume != nil && idx < resume.member {
			continue
		}
		filter, ok := targetFilter(target, q)
		if !ok {
			continue
		}
		before := out.remaining()
		remaining, start := resumePos(resume, idx, offset)
		if filter == nil {
			if resume == nil && start == nil && q.bbox == nil && offset > 0 {
				if tier == 0 && int64(offset) >= target.FeatureCount {
					offset -= int(target.FeatureCount)
					continue
				}
				meta, found, err := s.Store.ChunkAtOffset(ctx, target.Name, tier, offset)
				if err != nil {
					return res, err
				}
				if found {
					start = &startPos{chunkID: meta.ID, ord: offset - int(meta.FeatureBase)}
					remaining = 0
				}
			}
			rest, err := s.walkChunks(ctx, target, q, tier, remaining, start, &res.pos, out)
			if err != nil {
				return res, err
			}
			offset = rest
		} else {
			rest, err := s.walkCandidates(ctx, target, q, filter, remaining, start, &res.pos, out)
			if err != nil {
				return res, err
			}
			offset = rest
		}
		if out.remaining() != before {
			res.member = idx
			res.set = true
		}
	}
	return res, nil
}

func resumePos(resume *cursor, idx, offset int) (int, *startPos) {
	if resume == nil {
		return offset, nil
	}
	if idx == resume.member {
		return 0, &startPos{chunkID: resume.chunkID, ord: resume.ord + 1}
	}
	return 0, nil
}

func (s *Server) walkChunks(ctx context.Context, target store.CollectionInfo, q featureQuery, tier, offset int, start *startPos, last *startPos, out sink) (int, error) {
	after := int64(0)
	if start != nil {
		after = start.chunkID - 1
	}
	skip := start
	if q.bbox == nil {
		skip = nil
	}
	listedChunks, listedFeatures := 0, int64(0)
	for out.remaining() > 0 {
		metas, err := s.Store.ChunkMetas(ctx, target.Name, q.bbox, s.MaxChunks, tier, after)
		if err != nil {
			return offset, err
		}
		if len(metas) == 0 && tier > 0 && after == 0 {
			tier = 0
			continue
		}
		if len(metas) == 0 {
			return offset, checkListing(target, q, tier, out, listedChunks, listedFeatures)
		}
		window, trimmed := budgetMetas(metas, s.MaxBytes)
		for _, meta := range window {
			listedChunks++
			listedFeatures += int64(meta.FeatureCount)
		}
		var read []store.ChunkMeta
		var starts []int
		switch {
		case q.bbox != nil && offset > 0:
			read, starts, offset = skipInsideBBox(window, q.bbox, offset)
		case q.bbox != nil:
			read, starts = window, make([]int, len(window))
		case start != nil:
			read, starts = splitFrom(window, start)
			start = nil
		default:
			read, starts, offset = skipChunks(window, offset)
		}
		ids := make([]int64, len(read))
		for j, meta := range read {
			ids[j] = meta.ID
		}
		rows, errs := s.fetchRows(ctx, target.Name, ids)
		chunks, features, errs := s.decodeChunks(ctx, target, read, rows, errs)
		for j, meta := range read {
			if out.remaining() == 0 {
				return offset, nil
			}
			if errs[j] != nil {
				return offset, fmt.Errorf("chunk %d: %w", meta.ID, errs[j])
			}
			if offset, err = emit(target, q.bbox, meta.ID, chunks[j], features[j], ordinals(starts[j], len(features[j])), skip, offset, last, out); err != nil {
				return offset, err
			}
		}
		if len(window) < s.MaxChunks && !trimmed {
			return offset, checkListing(target, q, tier, out, listedChunks, listedFeatures)
		}
		after = window[len(window)-1].ID
	}
	return offset, nil
}

func checkListing(target store.CollectionInfo, q featureQuery, tier int, out sink, chunks int, features int64) error {
	if out.remaining() == 0 || q.bbox != nil || tier != 0 || (int64(chunks) == target.ChunkCount && features == target.FeatureCount) {
		return nil
	}
	return fmt.Errorf("collection %s: its chunks list %d features in %d chunks, the catalog has %d in %d",
		target.Name, features, chunks, target.FeatureCount, target.ChunkCount)
}

func splitFrom(metas []store.ChunkMeta, start *startPos) ([]store.ChunkMeta, []int) {
	var read []store.ChunkMeta
	var starts []int
	for _, meta := range metas {
		s := 0
		if start != nil {
			if meta.ID < start.chunkID {
				continue
			}
			if meta.ID == start.chunkID {
				s = min(start.ord, meta.FeatureCount)
			}
			start = nil
		}
		if s < meta.FeatureCount {
			read = append(read, meta)
			starts = append(starts, s)
		}
	}
	return read, starts
}

func insideBox(meta store.ChunkMeta, bbox *[4]float64) bool {
	return meta.MinX >= bbox[0] && meta.MaxX <= bbox[2] && meta.MinY >= bbox[1] && meta.MaxY <= bbox[3]
}

func skipInsideBBox(metas []store.ChunkMeta, bbox *[4]float64, offset int) ([]store.ChunkMeta, []int, int) {
	var read []store.ChunkMeta
	var starts []int
	for i, meta := range metas {
		if offset > 0 && insideBox(meta, bbox) {
			skip := min(offset, meta.FeatureCount)
			offset -= skip
			if skip == meta.FeatureCount {
				continue
			}
			read = append(read, meta)
			starts = append(starts, skip)
			continue
		}
		read = append(read, metas[i:]...)
		starts = append(starts, make([]int, len(metas)-i)...)
		return read, starts, offset
	}
	return read, starts, offset
}

func skipChunks(metas []store.ChunkMeta, offset int) ([]store.ChunkMeta, []int, int) {
	var read []store.ChunkMeta
	var starts []int
	for _, meta := range metas {
		start := min(offset, meta.FeatureCount)
		offset -= start
		if start < meta.FeatureCount {
			read = append(read, meta)
			starts = append(starts, start)
		}
	}
	return read, starts, offset
}

func ordinals(start, count int) []int {
	ords := make([]int, 0, max(count-start, 0))
	for i := min(start, count); i < count; i++ {
		ords = append(ords, i)
	}
	return ords
}

func (s *Server) walkCandidates(ctx context.Context, target store.CollectionInfo, q featureQuery, filter cql.Expr, offset int, start *startPos, last *startPos, out sink) (int, error) {
	from := int64(0)
	if start != nil {
		from = start.chunkID
	}
	ids, err := s.Store.CandidateChunks(ctx, target, q.bbox, filter, from)
	if err != nil {
		return offset, err
	}
	sidecar, err := s.Store.AttrReady(ctx, target.Name)
	if err != nil {
		return offset, err
	}
	for _, batch := range s.chunkBatches(ctx, target.Name, ids) {
		var precomputed map[int64][]int
		if sidecar {
			precomputed, sidecar, err = s.attrFilter(ctx, target, batch, filter)
			if err != nil {
				return offset, err
			}
		}
		read := batch
		if precomputed != nil {
			read = make([]int64, 0, len(batch))
			for _, id := range batch {
				if len(precomputed[id]) > 0 {
					read = append(read, id)
				}
			}
			if len(read) == 0 {
				start = nil
				continue
			}
		}
		rows, errs := s.fetchRows(ctx, target.Name, read)
		metas := make([]store.ChunkMeta, len(read))
		for j, id := range read {
			metas[j] = store.ChunkMeta{ID: id}
		}
		chunks, features, errs := s.decodeChunks(ctx, target, metas, rows, errs)
		for j, id := range read {
			if out.remaining() == 0 {
				return offset, nil
			}
			if errs[j] != nil {
				return offset, fmt.Errorf("chunk %d: %w", id, errs[j])
			}
			ords := precomputed[id]
			if precomputed == nil {
				ords = matchingOrds(chunks[j], features[j], filter)
			}
			if offset, err = emit(target, q.bbox, id, chunks[j], features[j], ords, start, offset, last, out); err != nil {
				return offset, err
			}
		}
		start = nil
	}
	return offset, nil
}

func (s *Server) countCandidates(ctx context.Context, target store.CollectionInfo, filter cql.Expr) (int64, error) {
	ids, err := s.Store.CandidateChunks(ctx, target, nil, filter, 0)
	if err != nil {
		return 0, err
	}
	sidecar, err := s.Store.AttrReady(ctx, target.Name)
	if err != nil {
		return 0, err
	}
	if sidecar {
		precomputed, ok, err := s.attrFilter(ctx, target, ids, filter)
		if err != nil {
			return 0, err
		}
		if ok {
			var total int64
			for _, id := range ids {
				total += int64(len(precomputed[id]))
			}
			return total, nil
		}
	}
	var total int64
	for _, batch := range s.chunkBatches(ctx, target.Name, ids) {
		rows, errs := s.fetchRows(ctx, target.Name, batch)
		metas := make([]store.ChunkMeta, len(batch))
		for j, id := range batch {
			metas[j] = store.ChunkMeta{ID: id}
		}
		chunks, features, errs := s.decodeChunks(ctx, target, metas, rows, errs)
		for j := range batch {
			if errs[j] != nil {
				return 0, fmt.Errorf("chunk %d: %w", batch[j], errs[j])
			}
			total += int64(len(matchingOrds(chunks[j], features[j], filter)))
		}
	}
	return total, nil
}

func matchingOrds(c *chunk.Chunk, features []*flattypes.Feature, filter cql.Expr) []int {
	cols := c.Columns()
	columns := chunk.ColumnTypes(cols)
	ords := make([]int, 0, len(features))
	for i, f := range features {
		if cql.Match(filter, chunk.DecodeProperties(f.PropertiesBytes(), cols), columns) {
			ords = append(ords, i)
		}
	}
	return ords
}

func emit(target store.CollectionInfo, bbox *[4]float64, chunkID int64, c *chunk.Chunk, features []*flattypes.Feature, ords []int, skip *startPos, offset int, last *startPos, out sink) (int, error) {
	for start := 0; start < len(ords) && out.remaining() > 0; {
		end := min(start+max(out.remaining(), minDecodeWindow), len(ords))
		window := ords[start:end]
		geos := decodeAt(c, features, window)
		for j, ord := range window {
			if out.remaining() == 0 {
				break
			}
			g := geos[j]
			if !inBounds(g, bbox) {
				continue
			}
			if skip != nil && !atOrAfter(skip, chunkID, ord) {
				continue
			}
			if offset > 0 {
				offset--
				continue
			}
			g.ID = featureID(target, chunkID, ord, g.Properties)
			if err := out.add(hit{chunk: c, raw: features[ord], geo: g}); err != nil {
				return offset, err
			}
			*last = startPos{chunkID: chunkID, ord: ord}
		}
		start = end
	}
	return offset, nil
}

func atOrAfter(p *startPos, chunkID int64, ord int) bool {
	if chunkID != p.chunkID {
		return chunkID > p.chunkID
	}
	return ord >= p.ord
}

func (s *Server) countMatches(ctx context.Context, info store.CollectionInfo, q featureQuery) (int64, bool, error) {
	if q.bbox != nil {
		return 0, false, nil
	}
	var total int64
	targets, err := s.targets(ctx, info)
	if err != nil {
		return 0, false, err
	}
	for _, target := range targets {
		filter, ok := targetFilter(target, q)
		if !ok {
			return 0, false, nil
		}
		if filter == nil {
			total += target.FeatureCount
			continue
		}
		count, err := s.countTarget(ctx, target, filter)
		if err != nil {
			return 0, false, err
		}
		total += count
	}
	return total, true, nil
}

func (s *Server) countTarget(ctx context.Context, target store.CollectionInfo, filter cql.Expr) (int64, error) {
	return s.countCandidates(ctx, target, filter)
}

func featureID(info store.CollectionInfo, chunkID int64, ord int, props map[string]any) any {
	if info.IDColumn != nil {
		if value, ok := props[*info.IDColumn]; ok && value != nil {
			return value
		}
	}
	return fmt.Sprintf("%d-%d", chunkID, ord)
}

func inBounds(f *chunk.Feature, bbox *[4]float64) bool {
	if bbox == nil {
		return true
	}
	geom := chunk.ToOrb(f.Geometry)
	if geom == nil {
		return false
	}
	bound := geom.Bound()
	return bound.Max[0] >= bbox[0] && bound.Min[0] <= bbox[2] &&
		bound.Max[1] >= bbox[1] && bound.Min[1] <= bbox[3]
}
