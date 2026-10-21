package geo

import (
	"github.com/parquet-go/parquet-go"

	"github.com/adonm/pgpq/internal/cql"
)

type Column struct {
	Name string
	Type cql.Type
}

type Feature struct {
	Geometry []byte
	Bounds   [4]float64
	Props    map[string]any
	Key      uint64
	ID       string
}

type Dataset struct {
	Columns  []Column
	Geometry string
	IDColumn string
	Features []Feature
}

type File struct {
	data       []byte
	file       *parquet.File
	fields     []parquet.Field
	geometry   int
	key        int
	attrs      []Column
	attrIndex  []int
	geometryAt int
}
