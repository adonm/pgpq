package api

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

type durationStat struct {
	sum   float64
	count int64
}

type metrics struct {
	mu        sync.Mutex
	counters  map[string]int64
	durations map[string]durationStat
}

var metricHelp = map[string]string{
	"postfgb_http_requests_total":           "HTTP requests by route, method and status.",
	"postfgb_http_request_duration_seconds": "HTTP request duration by route and method.",
	"postfgb_http_response_bytes_total":     "HTTP response body bytes by route and method.",
	"postfgb_tile_cache_lookups_total":      "Tile cache lookups by result.",
	"postfgb_tiles_rendered_total":          "MVT tiles rendered from chunks.",
	"postfgb_chunk_fetches_total":           "Raw chunk downloads.",
	"postfgb_ingest_total":                  "Collection ingests by result.",
	"postfgb_ingest_features_total":         "Features ingested.",
	"postfgb_ingest_chunks_total":           "Chunks produced by ingest.",
}

func newMetrics() *metrics {
	return &metrics{
		counters:  map[string]int64{},
		durations: map[string]durationStat{},
	}
}

func (m *metrics) add(name, labels string, delta int64) {
	m.mu.Lock()
	m.counters[name+labels] += delta
	m.mu.Unlock()
}

func (m *metrics) observe(name, labels string, seconds float64) {
	m.mu.Lock()
	stat := m.durations[name+labels]
	stat.sum += seconds
	stat.count++
	m.durations[name+labels] = stat
	m.mu.Unlock()
}

func (m *metrics) render(w io.Writer) {
	m.mu.Lock()
	counters := make(map[string]int64, len(m.counters))
	for key, value := range m.counters {
		counters[key] = value
	}
	durations := make(map[string]durationStat, len(m.durations))
	for key, value := range m.durations {
		durations[key] = value
	}
	m.mu.Unlock()
	writeSamples(w, map[string]map[string]string{
		"counter": counterSamples(counters),
		"summary": durationSamples(durations),
	})
}

func counterSamples(values map[string]int64) map[string]string {
	samples := make(map[string]string, len(values))
	for key, value := range values {
		samples[key] = fmt.Sprintf("%d", value)
	}
	return samples
}

func durationSamples(values map[string]durationStat) map[string]string {
	samples := make(map[string]string, len(values)*2)
	for key, value := range values {
		name, labels := key, ""
		if idx := strings.IndexByte(key, '{'); idx >= 0 {
			name, labels = key[:idx], key[idx:]
		}
		samples[name+"_sum"+labels] = formatFloat(value.sum)
		samples[name+"_count"+labels] = fmt.Sprintf("%d", value.count)
	}
	return samples
}

func writeSamples(w io.Writer, families map[string]map[string]string) {
	for _, kind := range []string{"counter", "summary"} {
		samples := families[kind]
		names := make([]string, 0, len(samples))
		grouped := map[string][]string{}
		for key := range samples {
			name := key
			if idx := strings.IndexByte(key, '{'); idx >= 0 {
				name = key[:idx]
			}
			if kind == "summary" {
				name = strings.TrimSuffix(name, "_sum")
				name = strings.TrimSuffix(name, "_count")
			}
			if _, ok := grouped[name]; !ok {
				names = append(names, name)
			}
			grouped[name] = append(grouped[name], key)
		}
		sort.Strings(names)
		for _, name := range names {
			help, ok := metricHelp[name]
			if !ok {
				continue
			}
			fmt.Fprintf(w, "# HELP %s %s\n", name, help)
			fmt.Fprintf(w, "# TYPE %s %s\n", name, kind)
			keys := grouped[name]
			sort.Strings(keys)
			for _, key := range keys {
				fmt.Fprintf(w, "%s %s\n", key, samples[key])
			}
		}
	}
}

func formatFloat(value float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", value), "0"), ".")
}

func metricLabels(pairs ...string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pairs[i])
		b.WriteString(`="`)
		b.WriteString(escapeLabel(pairs[i+1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return strings.ReplaceAll(value, "\n", `\n`)
}
