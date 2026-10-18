package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/geo"
	"github.com/adonm/postfgb/internal/store"
)

type featureQuery struct {
	bbox     *[4]float64
	tw       timeWindow
	filter   cql.Expr
	shapes   []geo.Shape
	offset   int
	limit    int
	overview *int
	empty    bool
}

func (s *Server) parseFeatureQuery(r *http.Request, info store.CollectionInfo, limit, offset int) (featureQuery, error) {
	q := featureQuery{limit: limit, offset: offset}
	bbox, err := parseBBox(r)
	if err != nil {
		return q, err
	}
	overview, err := parseOverview(r)
	if err != nil {
		return q, err
	}
	filter, err := requestFilter(r)
	if err != nil {
		return q, err
	}
	tw, err := parseTimeWindow(r)
	if err != nil {
		return q, err
	}
	if tw.set && info.Kind != "view" {
		if info.TimeColumn == nil || *info.TimeColumn == "" {
			return q, errors.New("collection has no time column")
		}
		filter = cql.And(filter, datetimeExpr(*info.TimeColumn, tw))
		tw = timeWindow{}
	}
	extracted, spatials, err := cql.ExtractSpatial(filter)
	if err != nil {
		return q, err
	}
	q.shapes = spatialShapes(spatials)
	bbox, ok := mergeBBox(bbox, q.shapes)
	if !ok {
		q.empty = true
		return q, nil
	}
	q.bbox = bbox
	q.tw = tw
	q.filter = extracted
	q.overview = overview
	return q, nil
}

func (s *Server) streamCollection(ctx context.Context, sink featureSink, info store.CollectionInfo, q featureQuery) {
	if q.empty {
		return
	}
	s.streamCollections(ctx, s.streamTargets(ctx, info), func() featureSink { return sink }, q)
}

func (s *Server) streamTargets(ctx context.Context, info store.CollectionInfo) []store.CollectionInfo {
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

func (s *Server) streamCollections(ctx context.Context, infos []store.CollectionInfo, sinkFor func() featureSink, q featureQuery) {
	remaining := q.offset
	for _, info := range infos {
		sink := sinkFor()
		if sink.full() {
			return
		}
		filter := q.filter
		if q.tw.set {
			if info.TimeColumn == nil || *info.TimeColumn == "" {
				continue
			}
			filter = cql.And(filter, datetimeExpr(*info.TimeColumn, q.tw))
		}
		if filter == nil {
			tier := 0
			if q.overview != nil && info.Kind != "view" {
				tier = *q.overview
			}
			remaining -= s.streamBBox(ctx, sink, info, q.bbox, remaining, tier)
			continue
		}
		skip := 0
		if remaining > 0 {
			count, err := s.Store.CountMatches(ctx, info.Name, q.bbox, filter)
			if err != nil {
				continue
			}
			skip = min(remaining, int(count))
		}
		remaining -= skip
		s.streamMatches(ctx, sink, info, q.bbox, filter, skip)
	}
}
