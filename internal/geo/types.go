package geo

type Type uint8

const (
	String Type = iota
	JSON
	Bool
	Int
	Double
)

type Column struct {
	Name string
	Type Type
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
