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
}

func negotiateEncoding(header string) string {
	zstdQ, gzipQ := 0.0, 0.0
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(part, ";")
		q := 1.0
		if _, value, ok := strings.Cut(params, "q="); ok {
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				q = parsed
			}
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
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
	h := e.Header()
	ct := h.Get("Content-Type")
	if h.Get("Content-Encoding") != "" || h.Get("Content-Length") != "" ||
		!(strings.HasPrefix(ct, "application/json") || strings.HasPrefix(ct, "application/geo+json")) {
		return
	}
	writer, err := newEncoder(e.encoding, e.ResponseWriter)
	if err != nil {
		return
	}
	h.Del("Content-Length")
	h.Set("Content-Encoding", e.encoding)
	h.Add("Vary", "Accept-Encoding")
	e.writer = writer
}

func newEncoder(encoding string, w io.Writer) (io.WriteCloser, error) {
	if encoding == "zstd" {
		return zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(1<<20))
	}
	return gzip.NewWriter(w), nil
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

func (e *encodingWriter) close() {
	if e.writer != nil {
		e.writer.Close()
	}
}
