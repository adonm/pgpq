package api

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"
)

type metrics struct {
	mu       sync.Mutex
	requests map[string]int64
	routes   map[string]routeStat
	ingests  map[string]int64
	features int64
	chunks   int64
}

type routeStat struct {
	calls   int64
	bytes   int64
	seconds float64
}

func newMetrics() *metrics {
	return &metrics{requests: map[string]int64{}, routes: map[string]routeStat{}, ingests: map[string]int64{}}
}

func (m *metrics) recordRequest(route, method string, status, size int, elapsed time.Duration) {
	labels := fmt.Sprintf(`route=%q,method=%q`, route, method)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[fmt.Sprintf(`%s,status="%d"`, labels, status)]++
	stat := m.routes[labels]
	stat.calls++
	stat.bytes += int64(size)
	stat.seconds += elapsed.Seconds()
	m.routes[labels] = stat
}

func (m *metrics) recordIngest(result string, features, chunks int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ingests[fmt.Sprintf(`result=%q`, result)]++
	m.features += int64(features)
	m.chunks += int64(chunks)
}

func (m *metrics) write(w io.Writer) {
	var out bytes.Buffer
	m.mu.Lock()
	m.render(&out)
	m.mu.Unlock()
	out.WriteTo(w)
}

func (m *metrics) render(w io.Writer) {
	family(w, "postfgb_http_requests_total", "counter", "HTTP requests by route, method and status.")
	for _, labels := range slices.Sorted(maps.Keys(m.requests)) {
		fmt.Fprintf(w, "postfgb_http_requests_total{%s} %d\n", labels, m.requests[labels])
	}
	family(w, "postfgb_http_response_bytes_total", "counter", "HTTP response body bytes by route and method.")
	for _, labels := range slices.Sorted(maps.Keys(m.routes)) {
		fmt.Fprintf(w, "postfgb_http_response_bytes_total{%s} %d\n", labels, m.routes[labels].bytes)
	}
	family(w, "postfgb_http_request_duration_seconds", "summary", "HTTP request duration by route and method.")
	for _, labels := range slices.Sorted(maps.Keys(m.routes)) {
		stat := m.routes[labels]
		fmt.Fprintf(w, "postfgb_http_request_duration_seconds_count{%s} %d\n", labels, stat.calls)
		fmt.Fprintf(w, "postfgb_http_request_duration_seconds_sum{%s} %f\n", labels, stat.seconds)
	}
	family(w, "postfgb_ingest_total", "counter", "Collection ingests by result.")
	for _, labels := range slices.Sorted(maps.Keys(m.ingests)) {
		fmt.Fprintf(w, "postfgb_ingest_total{%s} %d\n", labels, m.ingests[labels])
	}
	family(w, "postfgb_ingest_features_total", "counter", "Features ingested.")
	fmt.Fprintf(w, "postfgb_ingest_features_total %d\n", m.features)
	family(w, "postfgb_ingest_chunks_total", "counter", "Chunks produced by ingest.")
	fmt.Fprintf(w, "postfgb_ingest_chunks_total %d\n", m.chunks)
}

func family(w io.Writer, name, kind, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}
