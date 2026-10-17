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
	if tw.set && info.Kind != "view" {
		if info.TimeColumn == nil || *info.TimeColumn == "" {
			return q, errors.New("collection has no time column")
		}
		filter = cql.And(filter, datetimeExpr(*info.TimeColumn, tw))
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

func (s *Server) streamFeatureQuery(ctx context.Context, sink featureSink, info store.CollectionInfo, q featureQuery) {
	if q.empty {
		return
	}
	switch {
	case q.order != nil:
		s.streamSorted(ctx, sink, info, q.bbox, q.query, q.filter, q.order, q.offset)
	case q.query != "" || q.filter != nil:
		s.streamMatches(ctx, sink, info, q.bbox, q.query, q.filter, q.offset)
	default:
		tier := tierForBounds(q.bbox)
		if q.overview != nil {
			tier = *q.overview
		}
		if len(q.shapes) > 0 {
			tier = 0
		}
		s.streamBBox(ctx, sink, info, q.bbox, q.offset, tier)
	}
}

func (s *Server) streamCollection(ctx context.Context, sinkFor func(store.CollectionInfo, viewMember) featureSink, info store.CollectionInfo, q featureQuery) {
	if q.empty {
		return
	}
	if info.Kind == "view" {
		members, err := s.viewMembers(ctx, info)
		if err != nil {
			return
		}
		s.streamUnion(ctx, members, sinkFor, q.bbox, q.query, q.filter, q.offset, q.overview, q.tw)
		return
	}
	s.streamFeatureQuery(ctx, sinkFor(info, viewMember{}), info, q)
}
