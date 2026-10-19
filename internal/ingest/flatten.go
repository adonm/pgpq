package ingest

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
)

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
