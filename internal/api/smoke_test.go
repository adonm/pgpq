package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/adonm/postfgb/internal/chunk"
)

var smokeMagic = []byte{0x66, 0x67, 0x62, 0x03, 0x66, 0x67, 0x62, 0x00}

type smokeLink struct {
	Href string `json:"href"`
	Rel  string `json:"rel"`
	Type string `json:"type"`
}

type smokeCollectionDoc struct {
	ID       string       `json:"id"`
	Kind     string       `json:"kind"`
	ItemType string       `json:"itemType"`
	CRS      []string     `json:"crs"`
	Extent   *smokeExtent `json:"extent"`
	Links    []smokeLink  `json:"links"`
}

type smokeExtent struct {
	Spatial struct {
		BBox [][]float64 `json:"bbox"`
	} `json:"spatial"`
}

type smokeFeature struct {
	Type       string          `json:"type"`
	ID         json.RawMessage `json:"id"`
	Geometry   json.RawMessage `json:"geometry"`
	Properties map[string]any  `json:"properties"`
}

type smokePage struct {
	Type           string         `json:"type"`
	NumberReturned int            `json:"numberReturned"`
	NumberMatched  *int64         `json:"numberMatched"`
	Features       []smokeFeature `json:"features"`
	Links          []smokeLink    `json:"links"`
}

type smokeLayer struct {
	Name     string   `json:"name"`
	Visible  *bool    `json:"visible"`
	MinScale *float64 `json:"min_scale"`
	MaxScale *float64 `json:"max_scale"`
	Source   struct {
		Path string `json:"path"`
	} `json:"source"`
}

type smokeProject struct {
	Format int `json:"format"`
	Maps   []struct {
		Layers []smokeLayer `json:"layers"`
	} `json:"maps"`
}

type smokeBudget struct {
	seconds float64
	assumes string
}

type smoke struct {
	t         *testing.T
	base      string
	strict    bool
	client    *http.Client
	redirects *http.Client
}

func TestSmoke(t *testing.T) {
	base := strings.TrimRight(os.Getenv("POSTFGB_SMOKE_URL"), "/")
	if base == "" {
		t.Skip("POSTFGB_SMOKE_URL is not set")
	}
	s := &smoke{
		t:         t,
		base:      base,
		strict:    os.Getenv("POSTFGB_SMOKE_STRICT") == "true",
		client:    &http.Client{Timeout: 300 * time.Second},
		redirects: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	s.run()
}

func (s *smoke) run() {
	s.landing()
	collections := s.collections()
	var native, view *smokeCollectionDoc
	for i := range collections {
		c := collections[i]
		if c.Kind == "view" && view == nil {
			view = &c
		}
		if c.Kind != "view" && native == nil {
			native = &c
		}
	}
	if native == nil {
		s.t.Fatal("no native collection to test")
	}
	s.t.Logf("smoke collection %s (%s)", native.ID, native.Kind)
	raw := native.Extent.Spatial.BBox[0]
	box := [4]float64{raw[0], raw[1], raw[2], raw[3]}

	page := s.items(native.ID)
	s.paging(native.ID)
	s.bbox(native.ID, box, s.unfiltered(native.ID))
	property, value := s.filter(native.ID)
	if view != nil {
		s.viewReads(view.ID)
	}
	s.flatgeobuf(native.ID)
	s.viewer(native.ID)
	s.speeds(native.ID, box, page, property, value)
	s.gdal(native.ID)
	s.rustgis(native.ID, box)
}

func (s *smoke) do(method, path string, header map[string]string) (int, http.Header, []byte, time.Duration) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.base+path, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	for name, value := range header {
		req.Header.Set(name, value)
	}
	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	took := time.Since(start)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp.StatusCode, resp.Header, body, took
}

func (s *smoke) get(path string) (http.Header, []byte) {
	s.t.Helper()
	status, header, body, _ := s.do("GET", path, nil)
	if status != http.StatusOK {
		s.t.Fatalf("GET %s = %d: %s", path, status, body)
	}
	return header, body
}

func (s *smoke) page(path string) smokePage {
	s.t.Helper()
	_, body := s.get(path)
	var page smokePage
	if err := json.Unmarshal(body, &page); err != nil {
		s.t.Fatalf("GET %s: %v", path, err)
	}
	return page
}

func (s *smoke) landing() {
	header, body := s.get("/")
	if !strings.Contains(header.Get("Content-Type"), "json") {
		s.t.Fatalf("landing content type = %q", header.Get("Content-Type"))
	}
	var landing struct {
		Title       string      `json:"title"`
		Description string      `json:"description"`
		Links       []smokeLink `json:"links"`
	}
	if err := json.Unmarshal(body, &landing); err != nil {
		s.t.Fatal(err)
	}
	if landing.Title == "" {
		s.t.Fatal("landing has no title")
	}
	rels := map[string]string{}
	for _, l := range landing.Links {
		rels[l.Rel] = l.Href
	}
	for _, rel := range []string{"conformance", "data"} {
		if rels[rel] == "" {
			s.t.Fatalf("landing has no %s relation: %v", rel, rels)
		}
	}
	for _, rel := range []string{"service-desc", "service-doc"} {
		if href := rels[rel]; href != "" {
			if status, _, _, _ := s.do("GET", href, nil); status != http.StatusOK {
				s.t.Fatalf("landing %s link %s = %d", rel, href, status)
			}
		}
	}
	status, _, confBody, _ := s.do("GET", rels["conformance"], nil)
	if status != http.StatusOK {
		s.t.Fatalf("conformance = %d: %s", status, confBody)
	}
	var conf struct {
		ConformsTo []string `json:"conformsTo"`
	}
	if err := json.Unmarshal(confBody, &conf); err != nil || len(conf.ConformsTo) == 0 {
		s.t.Fatalf("conformance classes = %s (%v)", confBody, err)
	}
	s.t.Logf("smoke conformance classes: %d", len(conf.ConformsTo))
}

func (s *smoke) collections() []smokeCollectionDoc {
	s.t.Helper()
	_, body := s.get("/collections")
	var doc struct {
		Collections []smokeCollectionDoc `json:"collections"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		s.t.Fatal(err)
	}
	if len(doc.Collections) == 0 {
		s.t.Fatal("no collections listed")
	}
	for _, c := range doc.Collections {
		if c.ID == "" || c.ItemType != "feature" || len(c.CRS) == 0 {
			s.t.Fatalf("collection %+v lacks required fields", c)
		}
		if c.Extent == nil || len(c.Extent.Spatial.BBox) == 0 || len(c.Extent.Spatial.BBox[0]) != 4 {
			s.t.Fatalf("collection %s has no spatial extent", c.ID)
		}
	}
	first := doc.Collections[0]
	_, one := s.get("/collections/" + url.PathEscape(first.ID))
	var single smokeCollectionDoc
	if err := json.Unmarshal(one, &single); err != nil {
		s.t.Fatal(err)
	}
	if single.ID != first.ID || single.ItemType != "feature" || len(single.CRS) == 0 || single.Extent == nil || len(single.Links) == 0 {
		s.t.Fatalf("collection document = %+v", single)
	}
	s.t.Logf("smoke collections: %d listed, %s detail ok", len(doc.Collections), single.ID)
	return doc.Collections
}

func (s *smoke) unfiltered(id string) int64 {
	s.t.Helper()
	page := s.page("/collections/" + url.PathEscape(id) + "/items?limit=1&numberMatched=true")
	if page.NumberMatched == nil {
		s.t.Fatalf("numberMatched missing for %s", id)
	}
	return *page.NumberMatched
}

func (s *smoke) items(id string) smokePage {
	s.t.Helper()
	header, body := s.get("/collections/" + url.PathEscape(id) + "/items?numberMatched=true")
	if !strings.Contains(header.Get("Content-Type"), "geo+json") {
		s.t.Fatalf("items content type = %q", header.Get("Content-Type"))
	}
	var page smokePage
	if err := json.Unmarshal(body, &page); err != nil {
		s.t.Fatal(err)
	}
	if page.Type != "FeatureCollection" {
		s.t.Fatalf("items type = %q", page.Type)
	}
	if page.NumberReturned != len(page.Features) {
		s.t.Fatalf("numberReturned %d but %d features", page.NumberReturned, len(page.Features))
	}
	if len(page.Features) == 0 {
		s.t.Fatalf("%s default page is empty", id)
	}
	for _, f := range page.Features {
		s.saneFeature(id, f)
	}
	if page.NumberMatched != nil && *page.NumberMatched > int64(page.NumberReturned) && !smokeHasNext(page) {
		s.t.Fatalf("%s has %d of %d features but no next link", id, page.NumberReturned, *page.NumberMatched)
	}
	matched := int64(-1)
	if page.NumberMatched != nil {
		matched = *page.NumberMatched
	}
	s.t.Logf("smoke default page %s: %d features (%d matched)", id, page.NumberReturned, matched)
	return page
}

func smokeHasNext(page smokePage) bool {
	for _, l := range page.Links {
		if l.Rel == "next" && l.Href != "" {
			return true
		}
	}
	return false
}

func smokeNext(page smokePage) string {
	for _, l := range page.Links {
		if l.Rel == "next" {
			return l.Href
		}
	}
	return ""
}

func (s *smoke) saneFeature(id string, f smokeFeature) {
	s.t.Helper()
	if smokeFeatureID(f) == "" {
		s.t.Fatalf("%s returned a feature with no id", id)
	}
	if len(f.Geometry) == 0 || string(f.Geometry) == "null" {
		s.t.Fatalf("%s feature %s has no geometry", id, smokeFeatureID(f))
	}
	if f.Properties == nil {
		s.t.Fatalf("%s feature %s has no properties", id, smokeFeatureID(f))
	}
}

func smokeFeatureID(f smokeFeature) string {
	var text string
	if err := json.Unmarshal(f.ID, &text); err == nil {
		return text
	}
	return string(f.ID)
}

func (s *smoke) paging(id string) {
	s.t.Helper()
	first := s.page("/collections/" + url.PathEscape(id) + "/items?limit=5")
	next := smokeNext(first)
	if next == "" {
		s.t.Fatalf("%s has no next link on a 5-feature page", id)
	}
	second := s.page(next)
	if first.Type != "FeatureCollection" || second.Type != "FeatureCollection" {
		s.t.Fatalf("paged types = %s, %s", first.Type, second.Type)
	}
	if first.NumberReturned != 5 || first.NumberReturned != len(first.Features) {
		s.t.Fatalf("first page numberReturned = %d of %d", first.NumberReturned, len(first.Features))
	}
	if second.NumberReturned != len(second.Features) || second.NumberReturned == 0 {
		s.t.Fatalf("second page numberReturned = %d of %d", second.NumberReturned, len(second.Features))
	}
	seen := map[string]bool{}
	for _, f := range append(append([]smokeFeature{}, first.Features...), second.Features...) {
		s.saneFeature(id, f)
		if seen[smokeFeatureID(f)] {
			s.t.Fatalf("%s paging repeated feature id %s", id, smokeFeatureID(f))
		}
		seen[smokeFeatureID(f)] = true
	}
	s.t.Logf("smoke paging %s: two 5-feature pages, %d distinct ids", id, len(seen))
}

func (s *smoke) bbox(id string, box [4]float64, total int64) {
	s.t.Helper()
	query := url.Values{"bbox": {smokeBBox(box)}, "limit": {"100000"}}
	page := s.page("/collections/" + url.PathEscape(id) + "/items?" + query.Encode())
	if page.Type != "FeatureCollection" || page.NumberReturned != len(page.Features) {
		s.t.Fatalf("bbox page = %+v", page)
	}
	if page.NumberReturned == 0 || int64(page.NumberReturned) > total {
		s.t.Fatalf("bbox returned %d of %d features", page.NumberReturned, total)
	}
	const tolerance = 1e-6
	for _, f := range page.Features {
		env, ok := smokeEnvelope(f.Geometry)
		if !ok {
			s.t.Fatalf("%s bbox feature %s has no usable geometry", id, smokeFeatureID(f))
		}
		if env[0] < box[0]-tolerance || env[1] < box[1]-tolerance || env[2] > box[2]+tolerance || env[3] > box[3]+tolerance {
			s.t.Fatalf("%s feature %s envelope %v outside %v", id, smokeFeatureID(f), env, box)
		}
	}
	s.t.Logf("smoke bbox %s: %d of %d features inside [%g %g %g %g]", id, page.NumberReturned, total, box[0], box[1], box[2], box[3])
}

func (s *smoke) filter(id string) (string, string) {
	s.t.Helper()
	seed := s.page("/collections/" + url.PathEscape(id) + "/items?limit=25")
	property, value := smokePickString(seed.Features)
	if property == "" {
		s.t.Log("smoke filter: no string property in the sample, skipped")
		return "", ""
	}
	query := url.Values{property: {value}, "limit": {"25"}}
	page := s.page("/collections/" + url.PathEscape(id) + "/items?" + query.Encode())
	if page.Type != "FeatureCollection" || page.NumberReturned == 0 || page.NumberReturned != len(page.Features) {
		s.t.Fatalf("filter %s=%q returned %+v", property, value, page)
	}
	for _, f := range page.Features {
		if got := fmt.Sprint(f.Properties[property]); got != value {
			s.t.Fatalf("filter %s=%q returned %s=%q", property, value, property, got)
		}
	}
	s.t.Logf("smoke filter %s=%q: %d features matched", property, value, page.NumberReturned)
	return property, value
}

func smokePickString(features []smokeFeature) (string, string) {
	for _, f := range features {
		for name, raw := range f.Properties {
			value, ok := raw.(string)
			if ok && value != "" {
				return name, value
			}
		}
	}
	return "", ""
}

func (s *smoke) viewReads(id string) {
	s.t.Helper()
	page := s.page("/collections/" + url.PathEscape(id) + "/items?limit=5")
	if page.Type != "FeatureCollection" || page.NumberReturned != len(page.Features) || len(page.Features) == 0 {
		s.t.Fatalf("view items = %+v", page)
	}
	status, header, body, _ := s.do("GET", "/collections/"+url.PathEscape(id)+"/items.fgb", map[string]string{"Range": "bytes=0-999"})
	if status != http.StatusPartialContent || header.Get("Content-Range") == "" || !bytes.HasPrefix(body, smokeMagic) {
		s.t.Fatalf("view range = %d, Content-Range %q, %d bytes", status, header.Get("Content-Range"), len(body))
	}
	s.t.Logf("smoke view %s: items and %d-byte items.fgb range ok", id, len(body))
}

func (s *smoke) flatgeobuf(id string) {
	s.t.Helper()
	status, header, body, _ := s.do("GET", "/collections/"+url.PathEscape(id)+"/items?f=flatgeobuf&limit=10", nil)
	if status != http.StatusOK || !strings.Contains(header.Get("Content-Type"), "flatgeobuf") {
		s.t.Fatalf("export = %d, type %q", status, header.Get("Content-Type"))
	}
	if !bytes.HasPrefix(body, smokeMagic) {
		s.t.Fatalf("export does not start with the FlatGeobuf magic: %x", body[:min(8, len(body))])
	}
	file, err := chunk.Open(body)
	if err != nil {
		s.t.Fatalf("export does not read as a file: %v", err)
	}
	features, err := file.SearchAll()
	if err != nil || len(features) == 0 {
		s.t.Fatalf("export search = %d features (%v)", len(features), err)
	}
	status, header, ranged, _ := s.do("GET", "/collections/"+url.PathEscape(id)+"/items.fgb", map[string]string{"Range": "bytes=0-999"})
	if status != http.StatusPartialContent {
		s.t.Fatalf("items.fgb range = %d", status)
	}
	if header.Get("Accept-Ranges") != "bytes" {
		s.t.Fatalf("items.fgb Accept-Ranges = %q", header.Get("Accept-Ranges"))
	}
	if header.Get("Content-Range") == "" || len(ranged) != 1000 || !bytes.HasPrefix(ranged, smokeMagic) {
		s.t.Fatalf("items.fgb range: Content-Range %q, %d bytes, starts %x", header.Get("Content-Range"), len(ranged), ranged[:min(8, len(ranged))])
	}
	s.t.Logf("smoke flatgeobuf %s: %d exported features, range %s", id, len(features), header.Get("Content-Range"))
}

func (s *smoke) viewer(id string) {
	s.t.Helper()
	_, body := s.get("/viewer.rgis")
	var project smokeProject
	if err := json.Unmarshal(body, &project); err != nil {
		s.t.Fatal(err)
	}
	if project.Format != 1 || len(project.Maps) == 0 || len(project.Maps[0].Layers) == 0 {
		s.t.Fatalf("viewer.rgis = %+v", project)
	}
	detail, coarse := 0, 0
	for _, layer := range project.Maps[0].Layers {
		path := layer.Source.Path
		if !strings.Contains(path, "/collections/") || !strings.Contains(path, "/items.fgb") {
			s.t.Fatalf("viewer layer %s source %q is not an items.fgb path", layer.Name, path)
		}
		if layer.Visible == nil || *layer.Visible {
			s.t.Fatalf("viewer layer %s visible = %v, want false", layer.Name, layer.Visible)
		}
		switch {
		case strings.Contains(path, "overview=2"):
			if layer.MaxScale == nil || *layer.MaxScale != projectOverviewScale || layer.MinScale != nil {
				s.t.Fatalf("viewer layer %s zoom = %v/%v", layer.Name, layer.MinScale, layer.MaxScale)
			}
		case strings.Contains(path, "overview=1"):
			if layer.MinScale == nil || *layer.MinScale != projectOverviewScale || layer.MaxScale == nil || *layer.MaxScale != projectDetailScale {
				s.t.Fatalf("viewer layer %s zoom = %v/%v", layer.Name, layer.MinScale, layer.MaxScale)
			}
			coarse++
		default:
			if layer.MinScale == nil || *layer.MinScale != projectDetailScale || layer.MaxScale != nil {
				s.t.Fatalf("viewer layer %s zoom = %v/%v", layer.Name, layer.MinScale, layer.MaxScale)
			}
			detail++
		}
	}
	if detail == 0 {
		s.t.Fatal("viewer.rgis has no full-detail items.fgb layer")
	}
	req, err := http.NewRequest("GET", s.base+"/viewer", nil)
	if err != nil {
		s.t.Fatal(err)
	}
	resp, err := s.redirects.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	location := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || !strings.Contains(location, rustgisWeb) || !strings.Contains(location, "project=") {
		s.t.Fatalf("/viewer = %d, Location %q", resp.StatusCode, location)
	}
	s.t.Logf("smoke viewer: %d detail and %d overview layers, /viewer -> %s", detail, coarse, location)
}

func (s *smoke) speeds(id string, box [4]float64, page smokePage, property, value string) {
	s.t.Helper()
	measure := func(name, path string, header map[string]string, budget smokeBudget) {
		status, _, body, took := s.do("GET", path, header)
		if status != http.StatusOK && status != http.StatusPartialContent {
			s.t.Fatalf("%s = %d", name, status)
		}
		s.t.Logf("smoke %-18s %9d bytes in %7.3fs (budget %.3fs assumes %s)", name, len(body), took.Seconds(), budget.seconds, budget.assumes)
		if s.strict && took.Seconds() > budget.seconds {
			s.t.Errorf("%s took %.3fs, budget %.3fs", name, took.Seconds(), budget.seconds)
		}
	}
	items := "/collections/" + url.PathEscape(id) + "/items"
	measure("default-100k-page", items, nil, smokeBudget{0.8, "a warmed local worker serving ~8000 features of GeoJSON over loopback"})
	measure("five-feature-page", items+"?limit=5", nil, smokeBudget{0.2, "one chunk fetch and one small JSON write over loopback"})
	measure("bbox-page", items+"?"+url.Values{"bbox": {smokeBBox(box)}, "limit": {"100000"}}.Encode(), nil, smokeBudget{0.8, "the collection R-tree pruned by one box on a warmed local worker"})
	measure("single-feature", items+"/"+url.PathEscape(smokeFeatureID(page.Features[0])), nil, smokeBudget{0.2, "one chunk fetch and one feature decode over loopback"})
	measure("items-fgb-1mb-range", "/collections/"+url.PathEscape(id)+"/items.fgb", map[string]string{"Range": "bytes=0-1048575"}, smokeBudget{0.2, "one virtual segment and a 1 MB slice from the local worker"})
	if property != "" {
		measure("filtered-page", items+"?"+url.Values{property: {value}, "limit": {"100"}}.Encode(), nil, smokeBudget{0.3, "one jsonb containment match over a warmed local worker"})
	}
	measure("viewer-rgis", "/viewer.rgis", nil, smokeBudget{0.4, "a project built from the catalog on one warmed local worker"})
}

func (s *smoke) gdal(id string) {
	s.t.Helper()
	if os.Getenv("POSTFGB_SMOKE_GDAL") != "true" {
		s.t.Log("smoke GDAL skipped: POSTFGB_SMOKE_GDAL is not set")
		return
	}
	const image = "ghcr.io/osgeo/gdal:ubuntu-full-latest"
	if !smokeHasDockerImage(image) {
		s.t.Logf("smoke GDAL skipped: docker image %s is not present", image)
		return
	}
	target := "/vsicurl/" + s.base + "/collections/" + id + "/items.fgb"
	out, err := exec.Command("docker", "run", "--rm", "--network", "host", image, "ogrinfo", "-so", "-al", target).CombinedOutput()
	if err != nil {
		s.t.Fatalf("ogrinfo %s: %v\n%s", target, err, out)
	}
	count := smokeFeatureCount(string(out))
	if count <= 0 {
		s.t.Fatalf("ogrinfo reports no features:\n%s", out)
	}
	s.t.Logf("smoke GDAL ogrinfo %s: %d features", target, count)
}

func smokeFeatureCount(out string) int {
	for _, line := range strings.Split(out, "\n") {
		after, ok := strings.CutPrefix(strings.TrimSpace(line), "Feature Count:")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(after))
		if err == nil {
			return n
		}
	}
	return 0
}

func smokeHasDockerImage(image string) bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return exec.Command("docker", "image", "inspect", image).Run() == nil
}

func (s *smoke) rustgis(id string, box [4]float64) {
	s.t.Helper()
	bin := os.Getenv("POSTFGB_SMOKE_RUSTGIS")
	if bin == "" {
		s.t.Log("smoke RustGIS skipped: POSTFGB_SMOKE_RUSTGIS is not set")
		return
	}
	if _, err := os.Stat(bin); err != nil {
		s.t.Logf("smoke RustGIS skipped: %s not found", bin)
		return
	}
	dir, err := os.MkdirTemp(smokeScratch(), "smoke-")
	if err != nil {
		s.t.Fatalf("scratch dir: %v", err)
	}
	defer os.RemoveAll(dir)
	_, body := s.get("/viewer.rgis")
	var project map[string]any
	if err := json.Unmarshal(body, &project); err != nil {
		s.t.Fatal(err)
	}
	maps := project["maps"].([]any)
	layers := maps[0].(map[string]any)["layers"].([]any)
	layer := layers[0].(map[string]any)
	layer["visible"] = true
	layer["min_scale"] = nil
	layer["max_scale"] = nil
	source := layer["source"].(map[string]any)
	source["path"] = s.base + "/collections/" + url.PathEscape(id) + "/items?f=flatgeobuf&limit=25"
	maps[0].(map[string]any)["layers"] = []any{layer}
	projectBody, err := json.Marshal(project)
	if err != nil {
		s.t.Fatal(err)
	}
	projectPath := filepath.Join(dir, "project.rgis")
	if err := os.WriteFile(projectPath, projectBody, 0o644); err != nil {
		s.t.Fatal(err)
	}
	pngPath := filepath.Join(dir, "render.png")
	centerX := (box[0] + box[2]) / 2
	centerY := (box[1] + box[3]) / 2
	const delta = 0.01
	window := fmt.Sprintf(`{"extent":[%s]}`, smokeBBox([4]float64{centerX - delta, centerY - delta, centerX + delta, centerY + delta}))
	export := fmt.Sprintf(`{"path":%q,"width":512,"height":512}`, pngPath)
	out, err := exec.Command(bin, "run", "--data", projectPath,
		"--cmd", "map.zoomToExtent="+window,
		"--cmd", "share.exportMap="+export).CombinedOutput()
	if err != nil {
		s.t.Fatalf("rustgis-cli: %v\n%s", err, out)
	}
	info, err := os.Stat(pngPath)
	if err != nil || info.Size() == 0 {
		s.t.Fatalf("rustgis-cli wrote no render (%v, %s)", err, out)
	}
	s.t.Logf("smoke RustGIS rendered %d bytes with %s", info.Size(), bin)
}

func smokeScratch() string {
	if info, err := os.Stat("/tmp/opencode"); err == nil && info.IsDir() {
		return "/tmp/opencode"
	}
	return os.TempDir()
}

func smokeBBox(box [4]float64) string {
	parts := make([]string, 4)
	for i, v := range box {
		parts[i] = strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strings.Join(parts, ",")
}

func smokeEnvelope(raw json.RawMessage) ([4]float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return [4]float64{}, false
	}
	var geometry struct {
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if err := json.Unmarshal(raw, &geometry); err != nil {
		return [4]float64{}, false
	}
	var coordinates any
	if err := json.Unmarshal(geometry.Coordinates, &coordinates); err != nil {
		return [4]float64{}, false
	}
	box := [4]float64{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	smokeVisitCoordinates(coordinates, func(x, y float64) {
		box[0] = math.Min(box[0], x)
		box[1] = math.Min(box[1], y)
		box[2] = math.Max(box[2], x)
		box[3] = math.Max(box[3], y)
	})
	return box, box[0] <= box[2] && box[1] <= box[3]
}

func smokeVisitCoordinates(value any, visit func(x, y float64)) {
	values, ok := value.([]any)
	if !ok {
		return
	}
	if len(values) >= 2 {
		x, xok := values[0].(float64)
		y, yok := values[1].(float64)
		if xok && yok {
			visit(x, y)
			return
		}
	}
	for _, item := range values {
		smokeVisitCoordinates(item, visit)
	}
}
