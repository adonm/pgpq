package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type pageDoc struct {
	Features []struct {
		ID any `json:"id"`
	} `json:"features"`
	Links []struct {
		Rel  string `json:"rel"`
		Href string `json:"href"`
	} `json:"links"`
}

func walkPage(t *testing.T, h http.Handler, path string) ([]string, string) {
	t.Helper()
	w := call(h, "GET", path, "", nil)
	if w.Code != 200 {
		t.Fatalf("items %s = %d: %s", path, w.Code, w.Body.String())
	}
	var doc pageDoc
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(doc.Features))
	for _, f := range doc.Features {
		ids = append(ids, fmt.Sprint(f.ID))
	}
	next := ""
	for _, link := range doc.Links {
		if link.Rel == "next" {
			next = link.Href
		}
	}
	return ids, next
}

func walkCursors(t *testing.T, h http.Handler, path string) []string {
	t.Helper()
	var ids []string
	next := path
	for pages := 0; next != ""; pages++ {
		if pages > 10000 {
			t.Fatalf("cursor walk did not terminate at %s", path)
		}
		page, link := walkPage(t, h, next)
		ids = append(ids, page...)
		if link != "" && !strings.Contains(link, "cursor=") {
			t.Fatalf("next link carries no cursor: %s", link)
		}
		next = link
	}
	return ids
}

func walkOffsets(t *testing.T, h http.Handler, path string, limit int) []string {
	t.Helper()
	var ids []string
	base, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; ; offset += limit {
		query := base.Query()
		query.Set("limit", strconv.Itoa(limit))
		query.Set("offset", strconv.Itoa(offset))
		base.RawQuery = query.Encode()
		page, _ := walkPage(t, h, base.String())
		ids = append(ids, page...)
		if len(page) < limit {
			return ids
		}
	}
}

func TestPostgresCursorWalk(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	name := "itest_cursor"
	st.DropCollection(ctx, name)
	t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	h := New(st)
	if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", fixtureFGB(t, 40)); w.Code != 201 {
		t.Fatalf("ingest = %d: %s", w.Code, w.Body.String())
	}

	path := "/collections/" + name + "/items"
	offsets := walkOffsets(t, h, path, 7)
	cursors := walkCursors(t, h, path+"?limit=7")
	if len(offsets) != 40 || !slices.Equal(offsets, cursors) {
		t.Fatalf("cursor walk %d features != offset walk %d features", len(cursors), len(offsets))
	}

	filtered := path + "?limit=5&filter=" + url.QueryEscape("id >= 7")
	offsets = walkOffsets(t, h, filtered, 5)
	cursors = walkCursors(t, h, filtered)
	if len(offsets) == 0 || !slices.Equal(offsets, cursors) {
		t.Fatalf("filtered cursor walk %v != offset walk %v", cursors, offsets)
	}

	box := url.QueryEscape("7.39,43.71,7.42,43.74")
	boxed := path + "?limit=6&bbox=" + box
	offsets = walkOffsets(t, h, boxed, 6)
	cursors = walkCursors(t, h, boxed)
	if len(offsets) == 0 || !slices.Equal(offsets, cursors) {
		t.Fatalf("bbox cursor walk %v != offset walk %v", cursors, offsets)
	}
}

func TestPostgresCursorDeepResume(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	name := "itest_cursor_deep"
	st.DropCollection(ctx, name)
	t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	h := New(st)
	if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", sizedFixtureFGB(t, 3000, 200)); w.Code != 201 {
		t.Fatalf("ingest = %d: %s", w.Code, w.Body.String())
	}
	info, err := st.Collection(ctx, name)
	if err != nil {
		t.Fatal(err)
	}

	want := walkOffsets(t, h, "/collections/"+name+"/items", 1000)[1000:2000]
	_, link := walkPage(t, h, "/collections/"+name+"/items?limit=1000")
	if link == "" {
		t.Fatal("first page has no next link")
	}
	got, _ := walkPage(t, h, link)
	if !slices.Equal(got, want) {
		t.Fatalf("cursor resume returned %d ids, want the offset page of %d", len(got), len(want))
	}

	base, err := url.Parse("/collections/" + name + "/items")
	if err != nil {
		t.Fatal(err)
	}
	resume, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	cursor := resume.Query().Get("cursor")
	if cursor == "" {
		t.Fatalf("no cursor in %s", link)
	}
	c, err := decodeCursor(cursor)
	if err != nil {
		t.Fatal(err)
	}
	c.uid = info.UID + 1
	query := base.Query()
	query.Set("limit", "1000")
	query.Set("cursor", encodeCursor(c))
	base.RawQuery = query.Encode()
	if w := call(h, "GET", base.String(), "", nil); w.Code != 400 {
		t.Fatalf("stale cursor = %d, want 400: %s", w.Code, w.Body.String())
	}

	c, _ = decodeCursor(cursor)
	query = base.Query()
	query.Set("limit", "1000")
	query.Set("filter", "id > 10")
	query.Set("cursor", cursor)
	base.RawQuery = query.Encode()
	if w := call(h, "GET", base.String(), "", nil); w.Code != 400 {
		t.Fatalf("cursor with a different query = %d, want 400: %s", w.Code, w.Body.String())
	}

	query = base.Query()
	query.Set("limit", "1000")
	query.Set("offset", "10")
	query.Set("cursor", cursor)
	base.RawQuery = query.Encode()
	if w := call(h, "GET", base.String(), "", nil); w.Code != 400 {
		t.Fatalf("cursor with an offset = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestPostgresCursorViewAndOverview(t *testing.T) {
	st := integrationStore(t)
	ctx := context.Background()
	view := "itest_cursor_view"
	members := []string{"itest_cursor_a", "itest_cursor_b"}
	for _, name := range append(members, view) {
		st.DropCollection(ctx, name)
		t.Cleanup(func() { st.DropCollection(context.Background(), name) })
	}
	h := New(st)
	for _, name := range members {
		if w := call(h, "PUT", "/collections/"+name, "application/flatgeobuf", fixtureFGB(t, 40)); w.Code != 201 {
			t.Fatalf("ingest %s = %d: %s", name, w.Code, w.Body.String())
		}
	}
	if w := call(h, "PUT", "/collections/"+view, "application/json", []byte(`{"members":["itest_cursor_a","itest_cursor_b"]}`)); w.Code != 201 {
		t.Fatalf("view = %d: %s", w.Code, w.Body.String())
	}

	path := "/collections/" + view + "/items"
	offsets := walkOffsets(t, h, path, 7)
	cursors := walkCursors(t, h, path+"?limit=7")
	if len(offsets) != 80 || !slices.Equal(offsets, cursors) {
		t.Fatalf("view cursor walk %d features != offset walk %d features", len(cursors), len(offsets))
	}

	filtered := path + "?limit=6&filter=" + url.QueryEscape("id >= 8")
	offsets = walkOffsets(t, h, filtered, 6)
	cursors = walkCursors(t, h, filtered)
	if len(offsets) == 0 || !slices.Equal(offsets, cursors) {
		t.Fatalf("filtered view cursor walk %v != offset walk %v", cursors, offsets)
	}

	if w := call(h, "GET", path+"?limit=1&filter=id%3E0&overview=1", "", nil); w.Code != 400 {
		t.Fatalf("filtered overview = %d, want 400: %s", w.Code, w.Body.String())
	}
	if w := call(h, "GET", path+"?limit=1&overview=1", "", nil); w.Code != 200 {
		t.Fatalf("unfiltered overview = %d, want 200: %s", w.Code, w.Body.String())
	}
}
