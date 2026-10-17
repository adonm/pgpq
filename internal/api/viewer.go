package api

import (
	_ "embed"
	"net/http"
)

//go:embed viewer.html
var viewerPage []byte

func (s *Server) viewer(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(viewerPage)
}
