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
	body := rec.Body.String()
	if !strings.Contains(body, "pgpq v2") || !strings.Contains(body, "GeoParquet 2.0") {
		t.Fatal("design page body missing")
	}
	if !strings.Contains(body, "bootstrap@6.0.0-alpha.1") {
		t.Fatal("design page is not Bootstrap 6")
	}
	if strings.Contains(body, "<script") {
		t.Fatal("design page must not require a script/build step")
	}
}
