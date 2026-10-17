package api

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/adonm/postfgb/internal/store"
)

type queryablesOutput struct {
	Body struct {
		Schema     string                   `json:"$schema,omitempty"`
		Type       string                   `json:"type"`
		Title      string                   `json:"title"`
		Properties map[string]queryableType `json:"properties"`
	}
}

type queryableType struct {
	Type   string `json:"type"`
	Format string `json:"format,omitempty"`
}

func (s *Server) queryables(ctx context.Context, in *collectionInput) (*queryablesOutput, error) {
	info, err := s.Store.Collection(ctx, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("collection not found")
	}
	if err != nil {
		return nil, err
	}
	out := &queryablesOutput{}
	out.Body.Schema = "https://json-schema.org/draft/2020-12/schema"
	out.Body.Type = "object"
	out.Body.Title = info.Name
	out.Body.Properties = map[string]queryableType{}
	metas, err := s.Store.ChunkMetas(ctx, in.ID, nil, 1, 0, 0)
	if err != nil {
		return nil, err
	}
	if len(metas) > 0 && len(metas[0].Columns) > 0 {
		var cols []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(metas[0].Columns, &cols); err == nil {
			for _, col := range cols {
				out.Body.Properties[col.Name] = queryableTypeFor(col.Type)
			}
		}
	}
	return out, nil
}

func queryableTypeFor(columnType string) queryableType {
	switch columnType {
	case "Bool":
		return queryableType{Type: "boolean"}
	case "Byte", "UByte", "Short", "UShort", "Int", "UInt", "Long", "ULong":
		return queryableType{Type: "integer"}
	case "Float", "Double":
		return queryableType{Type: "number"}
	case "DateTime":
		return queryableType{Type: "string", Format: "date-time"}
	case "Json":
		return queryableType{Type: "object"}
	default:
		return queryableType{Type: "string"}
	}
}
