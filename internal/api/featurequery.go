package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		return f
	}
	return value
}

func (s *Server) targets(ctx context.Context, info store.CollectionInfo) []store.CollectionInfo {
	if info.Kind != "view" {
		return []store.CollectionInfo{info}
	}
	names, err := s.Store.ViewMemberNames(ctx, info)
	if err != nil {
		slog.Error("view members unreadable", "view", info.Name, "error", err)
		return nil
	}
	targets := make([]store.CollectionInfo, 0, len(names))
	for _, name := range names {
		member, err := s.Store.Collection(ctx, name)
		if err != nil {
			slog.Error("view member missing", "view", info.Name, "member", name, "error", err)
			continue
		}
		targets = append(targets, member)
	}
	return targets
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
	if q.overview != nil {
		tier = *q.overview
	}
	offset := q.offset
	for _, target := range s.targets(ctx, info) {
		if out.remaining() == 0 {
			return nil
		}
		filter, ok := targetFilter(target, q)
		if !ok {
			continue
		}
		if filter == nil {
			skipped, err := s.walkChunks(ctx, target, q, tier, offset, out)
			if err != nil {
				return err
			}
			offset -= skipped
			continue
		}
		skip := 0
		if offset > 0 {
			count, err := s.Store.CountMatches(ctx, target.Name, q.bbox, filter)
			if err != nil {
				return err
			}
			skip = min(offset, int(count))
		}
		offset -= skip
		if err := s.walkMatches(ctx, target, q, filter, skip, out); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) walkChunks(ctx context.Context, target store.CollectionInfo, q featureQuery, tier, offset int, out sink) (int, error) {
	skipped, after := 0, int64(0)
	for out.remaining() > 0 {
		metas, err := s.Store.ChunkMetas(ctx, target.Name, q.bbox, s.MaxChunks, tier, after)
		if err != nil {
			return skipped, err
		}
		if len(metas) == 0 && tier > 0 && after == 0 {
			tier = 0
			continue
		}
		if len(metas) == 0 {
			return skipped, nil
		}
		metas, trimmed := budgetMetas(metas, s.MaxBytes)
		var fetch []store.ChunkMeta
		var ids []int64
		for _, meta := range metas {
			if q.bbox == nil && skipped+meta.FeatureCount <= offset {
				skipped += meta.FeatureCount
				continue
			}
			fetch = append(fetch, meta)
			ids = append(ids, meta.ID)
		}
		rows := s.fetchRows(ctx, target.Name, ids)
		for j, meta := range fetch {
			if out.remaining() == 0 {
				return skipped, nil
			}
			c, features, err := s.openFeatures(ctx, target, meta.ID, rows[j])
			if err != nil {
				slog.Error("chunk read failed", "chunk", meta.ID, "error", err)
				continue
			}
			from := min(len(features), max(offset-skipped, 0))
			skipped += from
			if err := emit(target, q.bbox, meta.ID, c, features, func(i int) bool { return i >= from }, out); err != nil {
				return skipped, err
			}
		}
		if len(metas) < s.MaxChunks && !trimmed {
			return skipped, nil
		}
		after = metas[len(metas)-1].ID
	}
	return skipped, nil
}

func (s *Server) walkMatches(ctx context.Context, target store.CollectionInfo, q featureQuery, filter cql.Expr, skip int, out sink) error {
	matches, err := s.Store.Matches(ctx, target.Name, q.bbox, filter, out.remaining(), skip)
	if err != nil {
		return err
	}
	picked := map[int64]map[int]bool{}
	var ids []int64
	for _, m := range matches {
		set := picked[m.ChunkID]
		if set == nil {
			set = map[int]bool{}
			picked[m.ChunkID] = set
			ids = append(ids, m.ChunkID)
		}
		set[m.Ord] = true
	}
	for _, batch := range s.chunkBatches(ctx, target.Name, ids) {
		rows := s.fetchRows(ctx, target.Name, batch)
		for j, row := range rows {
			if row.Data == nil {
				continue
			}
			set := picked[batch[j]]
			c, features, err := s.openFeatures(ctx, target, batch[j], row)
			if err != nil {
				slog.Error("chunk read failed", "chunk", batch[j], "error", err)
				continue
			}
			if err := emit(target, q.bbox, batch[j], c, features, func(i int) bool { return set[i] }, out); err != nil {
				return err
			}
			if out.remaining() == 0 {
				return nil
			}
		}
	}
	return nil
}

func emit(target store.CollectionInfo, bbox *[4]float64, chunkID int64, c *chunk.Chunk, features []*flattypes.Feature, keep func(int) bool, out sink) error {
	var ords []int
	for i := range features {
		if keep(i) {
			ords = append(ords, i)
		}
	}
	for start := 0; start < len(ords); start += decodeWindow {
		window := ords[start:min(start+decodeWindow, len(ords))]
		geos := decodeAt(c, features, window)
		for j, ord := range window {
			if out.remaining() == 0 {
				return nil
			}
			g := geos[j]
			if !inBounds(g, bbox) {
				continue
			}
			g.ID = featureID(target, chunkID, ord, g.Properties)
			if err := out.add(hit{chunk: c, raw: features[ord], geo: g}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) countMatches(ctx context.Context, info store.CollectionInfo, q featureQuery) (int64, bool, error) {
	var total int64
	exact := true
	for _, target := range s.targets(ctx, info) {
		filter, ok := targetFilter(target, q)
		switch {
		case !ok:
			exact = false
		case filter != nil:
			count, err := s.Store.CountMatches(ctx, target.Name, q.bbox, filter)
			if err != nil {
				return 0, false, err
			}
			total += count
		case q.bbox == nil:
			total += target.FeatureCount
		default:
			exact = false
		}
	}
	return total, exact, nil
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
