package cql

type Type uint8

const (
	Bool Type = iota
	Int
	Double
	String
	JSON
)
