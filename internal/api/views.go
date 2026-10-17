package api

import (
	"context"
	"encoding/json"
	"log/slog"

	orbgeojson "github.com/paulmach/orb/geojson"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/cql"
	"github.com/adonm/postfgb/internal/store"
)

const maxViewMembers = 16

type viewMember struct {
	Collection string            `json:"collection"`
	Filter     string            `json:"filter,omitempty"`
	Columns    map[string]string `json:"columns,omitempty"`
}

func parseMembers(info store.CollectionInfo) ([]viewMember, error) {
	if info.Kind != "view" || len(info.Members) == 0 {
		return nil, nil
	}
	var members []viewMember
	if err := json.Unmarshal(info.Members, &members); err != nil {
		return nil, err
	}
	return members, nil
}

func (m *viewMember) renameFn() func(string) string {
	if len(m.Columns) == 0 {
		return func(prop string) string { return prop }
	}
	reverse := make(map[string]string, len(m.Columns))
	for src, dst := range m.Columns {
		reverse[dst] = src
	}
	return func(prop string) string {
		if src, ok := reverse[prop]; ok {
			return src
		}
		return prop
	}
}

func (m *viewMember) transformProps(props map[string]any) map[string]any {
	if len(m.Columns) == 0 {
		return props
	}
	out := make(map[string]any, len(m.Columns))
	for src, dst := range m.Columns {
		if value, ok := props[src]; ok {
			out[dst] = value
		}
	}
	return out
}

type memberWriter struct {
	base   featureSink
	member viewMember
}

func (w *memberWriter) add(g *chunk.Feature) bool {
	g.Properties = w.member.transformProps(g.Properties)
	return w.base.add(g)
}

func (w *memberWriter) full() bool {
	return w.base.full()
}

func (w *memberWriter) remaining() int {
	return w.base.remaining()
}

func (s *Server) memberFilter(member viewMember) (cql.Expr, error) {
	if member.Filter == "" {
		return nil, nil
	}
	return cql.Parse(member.Filter)
}

type unionMember struct {
	info   store.CollectionInfo
	filter cql.Expr
	member viewMember
}

func (s *Server) viewMembers(ctx context.Context, info store.CollectionInfo) ([]unionMember, error) {
	members, err := parseMembers(info)
	if err != nil {
		return nil, err
	}
	out := make([]unionMember, 0, len(members))
	for _, member := range members {
		mi, err := s.Store.Collection(ctx, member.Collection)
		if err != nil {
			slog.Error("view member missing", "collection", info.Name, "member", member.Collection, "error", err)
			continue
		}
		memberFilter, err := s.memberFilter(member)
		if err != nil {
			slog.Error("view member filter invalid", "collection", info.Name, "member", member.Collection, "error", err)
			continue
		}
		out = append(out, unionMember{info: mi, filter: memberFilter, member: member})
	}
	return out, nil
}

func (s *Server) streamUnion(ctx context.Context, members []unionMember, sinkFor func(store.CollectionInfo, viewMember) featureSink, bbox *[4]float64, query string, filter cql.Expr, offset int, overview *int, tw timeWindow) {
	remaining := offset
	for _, m := range members {
		sink := sinkFor(m.info, m.member)
		if sink.full() {
			return
		}
		effective := cql.And(m.filter, cql.Rename(filter, m.member.renameFn()))
		if tw.set {
			if m.info.TimeColumn == nil || *m.info.TimeColumn == "" {
				continue
			}
			effective = cql.And(effective, datetimeExpr(*m.info.TimeColumn, tw))
		}
		if effective == nil && query == "" {
			tier := 0
			if overview != nil {
				tier = *overview
			}
			remaining -= s.streamBBox(ctx, sink, m.info, bbox, remaining, tier)
		} else {
			skip := 0
			if remaining > 0 {
				count, err := s.Store.CountMatches(ctx, m.info.Name, bbox, query, effective)
				if err != nil {
					slog.Error("union member count failed", "collection", m.info.Name, "error", err)
					continue
				}
				skip = min(remaining, int(count))
			}
			remaining -= skip
			s.streamMatches(ctx, sink, m.info, bbox, query, effective, skip)
		}
		if remaining < 0 {
			remaining = 0
		}
	}
}

func (s *Server) viewTile(ctx context.Context, info store.CollectionInfo, bbox [4]float64, query string, filter cql.Expr, tier int, tw timeWindow) (*orbgeojson.FeatureCollection, error) {
	members, err := parseMembers(info)
	if err != nil {
		return nil, err
	}
	fc := orbgeojson.NewFeatureCollection()
	for _, member := range members {
		mi, err := s.Store.Collection(ctx, member.Collection)
		if err != nil {
			continue
		}
		memberFilter, err := s.memberFilter(member)
		if err != nil {
			continue
		}
		effective := cql.And(memberFilter, cql.Rename(filter, member.renameFn()))
		if tw.set {
			if mi.TimeColumn == nil || *mi.TimeColumn == "" {
				continue
			}
			effective = cql.And(effective, datetimeExpr(*mi.TimeColumn, tw))
		}
		var source *orbgeojson.FeatureCollection
		if effective == nil && query == "" {
			rows, err := s.Store.Chunks(ctx, mi.Name, &bbox, s.MaxChunks, tier)
			if err == nil && len(rows) == 0 && tier > 0 {
				rows, err = s.Store.Chunks(ctx, mi.Name, &bbox, s.MaxChunks, 0)
			}
			if err != nil {
				continue
			}
			source = orbgeojson.NewFeatureCollection()
			for _, row := range rows {
				c, err := chunk.Open(row.Data)
				if err != nil {
					continue
				}
				raw, err := c.Search(bbox[0], bbox[1], bbox[2], bbox[3])
				if err != nil {
					continue
				}
				appendFeatures(source, c, raw, mi, row.ID)
			}
		} else {
			source, err = s.tileMatches(ctx, mi, bbox, query, effective)
			if err != nil {
				continue
			}
		}
		for _, feature := range source.Features {
			feature.Properties = member.transformProps(feature.Properties)
			fc.Append(feature)
		}
	}
	return fc, nil
}
