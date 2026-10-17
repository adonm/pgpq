package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type encodingWriter struct {
	http.ResponseWriter
	encoding string
	writer   io.WriteCloser
	decided  bool
	skip     bool
}

func negotiateEncoding(header string) string {
	zstdQ, gzipQ := 0.0, 0.0
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, params, _ := strings.Cut(part, ";")
		name = strings.ToLower(strings.TrimSpace(name))
		q := 1.0
		if strings.Contains(params, "q=") {
			_, value, _ := strings.Cut(params, "q=")
			value = strings.TrimSpace(strings.TrimSuffix(value, ","))
			if parsed, err := strconv.ParseFloat(value, 64); err == nil {
				q = parsed
			}
		}
		switch name {
		case "zstd":
			zstdQ = q
		case "gzip", "*":
			if q > gzipQ {
				gzipQ = q
			}
		}
	}
	switch {
	case zstdQ > 0 && zstdQ >= gzipQ:
		return "zstd"
	case gzipQ > 0:
		return "gzip"
	default:
		return ""
	}
}

func compressionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoding := negotiateEncoding(r.Header.Get("Accept-Encoding"))
		if encoding == "" {
			next.ServeHTTP(w, r)
			return
		}
		ew := &encodingWriter{ResponseWriter: w, encoding: encoding}
		defer ew.close()
		next.ServeHTTP(ew, r)
	})
}

func (e *encodingWriter) decide() {
	if e.decided {
		return
	}
	e.decided = true
	ct := e.Header().Get("Content-Type")
	e.skip = e.Header().Get("Content-Encoding") != "" || e.Header().Get("Content-Length") != "" ||
		!(strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "application/geo+json"))
	if e.skip {
		return
	}
	e.Header().Del("Content-Length")
	e.Header().Set("Content-Encoding", e.encoding)
	e.Header().Add("Vary", "Accept-Encoding")
	if e.encoding == "zstd" {
		writer, err := zstd.NewWriter(e.ResponseWriter, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(1<<20))
		if err != nil {
			e.skip = true
			return
		}
		e.writer = writer
		return
	}
	e.writer = gzip.NewWriter(e.ResponseWriter)
}

func (e *encodingWriter) WriteHeader(code int) {
	e.decide()
	e.ResponseWriter.WriteHeader(code)
}

func (e *encodingWriter) Write(b []byte) (int, error) {
	e.decide()
	if e.writer != nil {
		return e.writer.Write(b)
	}
	return e.ResponseWriter.Write(b)
}

func (e *encodingWriter) Flush() {
	e.decide()
	if e.writer != nil {
		if flusher, ok := e.writer.(interface{ Flush() error }); ok {
			flusher.Flush()
		}
	}
	if flusher, ok := e.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (e *encodingWriter) close() {
	if e.writer != nil {
		e.writer.Close()
	}
}
