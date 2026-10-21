package layer

import (
	"encoding/json"

	"github.com/adonm/pgpq/internal/cql"
	"github.com/adonm/pgpq/internal/geo"
)

type zoneColumn struct {
	Name  string   `json:"name"`
	Nulls int      `json:"nulls"`
	Min   *float64 `json:"min,omitempty"`
	Max   *float64 `json:"max,omitempty"`
}

func encodeZone(cols []geo.Column, features []geo.Feature) (string, error) {
	zone := make([]zoneColumn, len(cols))
	for i, col := range cols {
		zone[i].Name = col.Name
		for _, feature := range features {
			value, ok := feature.Props[col.Name]
			if !ok || value == nil {
				zone[i].Nulls++
				continue
			}
			if col.Type != cql.Int && col.Type != cql.Double {
				continue
			}
			var f float64
			switch n := value.(type) {
			case int64:
				f = float64(n)
			case float64:
				f = n
			default:
				continue
			}
			if zone[i].Min == nil || f < *zone[i].Min {
				v := f
				zone[i].Min = &v
			}
			if zone[i].Max == nil || f > *zone[i].Max {
				v := f
				zone[i].Max = &v
			}
		}
	}
	out, err := json.Marshal(zone)
	return string(out), err
}

func decodeZone(text string) (map[string]zoneColumn, error) {
	if text == "" {
		return nil, nil
	}
	var list []zoneColumn
	if err := json.Unmarshal([]byte(text), &list); err != nil {
		return nil, err
	}
	zone := make(map[string]zoneColumn, len(list))
	for _, column := range list {
		zone[column.Name] = column
	}
	return zone, nil
}

func mayMatch(filter cql.Expr, zone map[string]zoneColumn) bool {
	return cql.MayMatch(filter, func(leaf cql.Expr) bool { return zoneLeaf(leaf, zone) })
}

func zoneLeaf(leaf cql.Expr, zone map[string]zoneColumn) bool {
	switch x := leaf.(type) {
	case *cql.Compare:
		z, ok := zone[x.Prop]
		v, numeric := numberOf(x.Value)
		if !ok || !numeric || z.Min == nil || z.Max == nil {
			return true
		}
		switch x.Op {
		case "=":
			return *z.Min <= v && v <= *z.Max
		case "<", "<=":
			return *z.Min <= v
		case ">", ">=":
			return *z.Max >= v
		}
	case *cql.Between:
		z, ok := zone[x.Prop]
		lo, okLo := numberOf(x.Lo)
		hi, okHi := numberOf(x.Hi)
		if x.Not || !ok || !okLo || !okHi || z.Min == nil || z.Max == nil {
			return true
		}
		return *z.Min <= hi && *z.Max >= lo
	case *cql.In:
		if x.Not {
			return true
		}
		for _, value := range x.Values {
			if zoneLeaf(&cql.Compare{Prop: x.Prop, Op: "=", Value: value}, zone) {
				return true
			}
		}
		return false
	case *cql.IsNull:
		z, ok := zone[x.Prop]
		return x.Not || !ok || z.Nulls > 0
	}
	return true
}

func numberOf(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}
