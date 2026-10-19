package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDesignPageIsServedAsHTML(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).design(rec, httptest.NewRequest(http.MethodGet, "/design", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "postfgb v2: the design on one page") {
		t.Fatal("design page body missing")
	}
}
