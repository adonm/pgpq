package api

import (
	_ "embed"
	"net/http"
)

//go:embed design.html
var designHTML []byte

func (s *Server) design(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(designHTML)
}
