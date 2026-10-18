package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		elapsed := time.Since(start)
		s.metrics.recordRequest(route, r.Method, sw.status, sw.bytes, elapsed)
		slog.Info("request",
			"method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery, "route", route,
			"status", sw.status, "duration_ms", elapsed.Milliseconds(), "bytes", sw.bytes)
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if s.Store == nil {
		http.Error(w, "no database", http.StatusServiceUnavailable)
		return
	}
	if err := s.Store.Ready(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ready\n"))
}

func (s *Server) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.write(w)
	if s.Store == nil {
		return
	}
	stat := s.Store.Pool.Stat()
	family(w, "postfgb_pg_pool_connections", "gauge", "PostgreSQL pool connections by state.")
	fmt.Fprintf(w, "postfgb_pg_pool_connections{state=\"total\"} %d\n", stat.TotalConns())
	fmt.Fprintf(w, "postfgb_pg_pool_connections{state=\"acquired\"} %d\n", stat.AcquiredConns())
	fmt.Fprintf(w, "postfgb_pg_pool_connections{state=\"idle\"} %d\n", stat.IdleConns())
	fmt.Fprintf(w, "postfgb_pg_pool_connections{state=\"max\"} %d\n", stat.MaxConns())
}
