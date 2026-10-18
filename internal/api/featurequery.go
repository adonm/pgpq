package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/store"
)

type featureQuery struct {
	bbox     *[4]float64
	tw       timeWindow
	filter   cql.Expr
	offset   int
	overview *int
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

var reservedParams = map[string]bool{
	"bbox": true, "limit": true, "offset": true, "f": true,
	"filter": true, "filter-lang": true, "format": true,
	"overview": true, "numberMatched": true,
	"datetime": true, "crs": true, "bbox-crs": true,
	"z": true, "x": true, "y": true,
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
	return q, nil
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

func (s *Server) walkFeatures(ctx context.Context, info store.CollectionInfo, q featureQuery, out sink) error {
	tier := 0
	if q.overview != nil && info.Kind != "view" {
		tier = *q.overview
	}
	offset := q.offset
	targets, err := s.targets(ctx, info)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if out.remaining() == 0 {
			return nil
		}
		filter, ok := targetFilter(target, q)
		if !ok {
			continue
		}
		if filter == nil {
			remaining, err := s.walkChunks(ctx, target, q, tier, offset, out)
			if err != nil {
				return err
			}
			offset = remaining
			continue
		}
		remaining, err := s.walkMatches(ctx, target, q, filter, offset, out)
		if err != nil {
			return err
		}
		offset = remaining
	}
	return nil
}

func (s *Server) walkChunks(ctx context.Context, target store.CollectionInfo, q featureQuery, tier, offset int, out sink) (int, error) {
	after := int64(0)
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
		read, starts := window, make([]int, len(window))
		if q.bbox == nil {
			read, starts, offset = skipChunks(window, offset)
		}
		ids := make([]int64, len(read))
		for j, meta := range read {
			ids[j] = meta.ID
		}
		rows, errs := s.fetchRows(ctx, target.Name, ids)
		for j, meta := range read {
			if out.remaining() == 0 {
				return offset, nil
			}
			if errs[j] != nil {
				return offset, fmt.Errorf("chunk %d: %w", meta.ID, errs[j])
			}
			c, features, err := s.openFeatures(ctx, target, meta.ID, rows[j])
			if err != nil {
				return offset, fmt.Errorf("chunk %d: %w", meta.ID, err)
			}
			if offset, err = emit(target, q.bbox, meta.ID, c, features, ordinals(starts[j], len(features)), offset, out); err != nil {
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

func (s *Server) walkMatches(ctx context.Context, target store.CollectionInfo, q featureQuery, filter cql.Expr, offset int, out sink) (int, error) {
	if store.StatsReady(target) {
		return s.walkCandidates(ctx, target, q, filter, offset, out)
	}
	pos := 0
	if q.bbox == nil && offset > 0 {
		count, err := s.Store.CountMatches(ctx, target, filter)
		if err != nil {
			return offset, err
		}
		pos = min(offset, int(count))
		offset -= pos
	}
	for limit := max(offset+out.remaining(), minMatchWindow); out.remaining() > 0; limit *= 2 {
		matches, err := s.Store.Matches(ctx, target, q.bbox, filter, limit, pos)
		if err != nil {
			return offset, err
		}
		if offset, err = s.emitMatches(ctx, target, q.bbox, matches, offset, out); err != nil {
			return offset, err
		}
		pos += len(matches)
		if len(matches) < limit {
			break
		}
	}
	return offset, nil
}

func (s *Server) walkCandidates(ctx context.Context, target store.CollectionInfo, q featureQuery, filter cql.Expr, offset int, out sink) (int, error) {
	casts, err := s.Store.ColumnCasts(ctx, target)
	if err != nil {
		return offset, err
	}
	ids, err := s.Store.CandidateChunks(ctx, target, q.bbox, filter)
	if err != nil {
		return offset, err
	}
	for _, batch := range s.chunkBatches(ctx, target.Name, ids) {
		rows, errs := s.fetchRows(ctx, target.Name, batch)
		for j, id := range batch {
			if out.remaining() == 0 {
				return offset, nil
			}
			if errs[j] != nil {
				return offset, fmt.Errorf("chunk %d: %w", id, errs[j])
			}
			c, features, err := s.openFeatures(ctx, target, id, rows[j])
			if err != nil {
				return offset, fmt.Errorf("chunk %d: %w", id, err)
			}
			if offset, err = emit(target, q.bbox, id, c, features, matchingOrds(c, features, filter, casts), offset, out); err != nil {
				return offset, err
			}
		}
	}
	return offset, nil
}

func (s *Server) countCandidates(ctx context.Context, target store.CollectionInfo, filter cql.Expr) (int64, error) {
	casts, err := s.Store.ColumnCasts(ctx, target)
	if err != nil {
		return 0, err
	}
	ids, err := s.Store.CandidateChunks(ctx, target, nil, filter)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, batch := range s.chunkBatches(ctx, target.Name, ids) {
		rows, errs := s.fetchRows(ctx, target.Name, batch)
		for j, id := range batch {
			if errs[j] != nil {
				return 0, fmt.Errorf("chunk %d: %w", id, errs[j])
			}
			c, features, err := s.openFeatures(ctx, target, id, rows[j])
			if err != nil {
				return 0, fmt.Errorf("chunk %d: %w", id, err)
			}
			total += int64(len(matchingOrds(c, features, filter, casts)))
		}
	}
	return total, nil
}

func matchingOrds(c *chunk.Chunk, features []*flattypes.Feature, filter cql.Expr, casts cql.Casts) []int {
	cols := c.Columns()
	ords := make([]int, 0, len(features))
	for i, f := range features {
		if cql.Match(filter, chunk.DecodeProperties(f.PropertiesBytes(), cols), casts) {
			ords = append(ords, i)
		}
	}
	return ords
}

func (s *Server) emitMatches(ctx context.Context, target store.CollectionInfo, bbox *[4]float64, matches []store.FeatureMatch, offset int, out sink) (int, error) {
	var ids []int64
	picked := map[int64][]int{}
	for _, m := range matches {
		if _, seen := picked[m.ChunkID]; !seen {
			ids = append(ids, m.ChunkID)
		}
		picked[m.ChunkID] = append(picked[m.ChunkID], m.Ord)
	}
	for _, batch := range s.chunkBatches(ctx, target.Name, ids) {
		rows, errs := s.fetchRows(ctx, target.Name, batch)
		for j, row := range rows {
			if errs[j] != nil {
				return offset, fmt.Errorf("chunk %d: %w", batch[j], errs[j])
			}
			c, features, err := s.openFeatures(ctx, target, batch[j], row)
			if err != nil {
				return offset, fmt.Errorf("chunk %d: %w", batch[j], err)
			}
			ords := picked[batch[j]]
			n, _ := slices.BinarySearch(ords, len(features))
			if offset, err = emit(target, bbox, batch[j], c, features, ords[:n], offset, out); err != nil {
				return offset, err
			}
			if out.remaining() == 0 {
				return offset, nil
			}
		}
	}
	return offset, nil
}

func emit(target store.CollectionInfo, bbox *[4]float64, chunkID int64, c *chunk.Chunk, features []*flattypes.Feature, ords []int, offset int, out sink) (int, error) {
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
			if offset > 0 {
				offset--
				continue
			}
			g.ID = featureID(target, chunkID, ord, g.Properties)
			if err := out.add(hit{chunk: c, raw: features[ord], geo: g}); err != nil {
				return offset, err
			}
		}
		start = end
	}
	return offset, nil
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
	if store.StatsReady(target) {
		return s.countCandidates(ctx, target, filter)
	}
	return s.Store.CountMatches(ctx, target, filter)
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
