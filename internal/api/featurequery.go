package api

import (
	"context"
	"errors"
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
	query    string
	order    []string
	offset   int
	limit    int
	overview *int
	empty    bool
}

func (s *Server) parseFeatureQuery(ctx context.Context, r *http.Request, info store.CollectionInfo, limit, offset int) (featureQuery, error) {
	q := featureQuery{limit: limit, offset: offset}
	bbox, err := parseBBox(r)
	if err != nil {
		return q, err
	}
	bbox, err = s.inputBBox(r, bbox)
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
	if tw.set {
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
	order, err := s.sortOrder(ctx, r, info)
	if err != nil {
		return q, err
	}
	q.bbox = bbox
	q.tw = tw
	q.filter = extracted
	q.query = r.URL.Query().Get("q")
	q.order = order
	q.overview = overview
	return q, nil
}

func (s *Server) streamCollection(ctx context.Context, sink featureSink, info store.CollectionInfo, q featureQuery) {
	if q.empty {
		return
	}
	s.streamCollections(ctx, []store.CollectionInfo{info}, func() featureSink { return sink }, q)
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
		if filter == nil && q.query == "" && q.order == nil {
			tier := tierForBounds(q.bbox)
			if q.overview != nil {
				tier = *q.overview
			}
			if len(q.shapes) > 0 {
				tier = 0
			}
			remaining -= s.streamBBox(ctx, sink, info, q.bbox, remaining, tier)
			continue
		}
		if q.order != nil {
			s.streamSorted(ctx, sink, info, q.bbox, q.query, filter, q.order, remaining)
			remaining = 0
			continue
		}
		skip := 0
		if remaining > 0 {
			count, err := s.Store.CountMatches(ctx, info.Name, q.bbox, q.query, filter)
			if err != nil {
				continue
			}
			skip = min(remaining, int(count))
		}
		remaining -= skip
		s.streamMatches(ctx, sink, info, q.bbox, q.query, filter, skip)
	}
}
