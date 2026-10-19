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
	if !strings.Contains(body, "postfgb v2 · design") {
		t.Fatal("design page body missing")
	}
	if !strings.Contains(body, "zdog@1.1.3/dist/zdog.dist.min.js") {
		t.Fatal("design page does not load the pinned Zdog build")
	}
}
