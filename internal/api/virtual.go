package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/store"
)

var virtualMagic = [8]byte{0x66, 0x67, 0x62, 0x03, 0x66, 0x67, 0x62, 0x00}

type virtualRun struct {
	chunkID int64
	base    int64
	bytes   int
}

type virtualSegment struct {
	length int64
	read   func(ctx context.Context, from, to int64) ([]byte, error)
}

type virtualFile struct {
	name     string
	version  int64
	segments []virtualSegment
	features int64
	runs     []virtualRun
}

func (v *virtualFile) total() int64 {
	total := int64(len(virtualMagic))
	for _, segment := range v.segments {
		total += segment.length
	}
	return total + v.features
}

func byteSegment(body []byte, length int64) virtualSegment {
	return virtualSegment{length: length, read: func(_ context.Context, from, to int64) ([]byte, error) {
		return body[from:to], nil
	}}
}

type virtualCache struct {
	mu      sync.Mutex
	entries map[string]*virtualFile
}

func newVirtualCache() *virtualCache {
	return &virtualCache{entries: map[string]*virtualFile{}}
}

func (c *virtualCache) get(key string) *virtualFile {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[key]
}

func (c *virtualCache) put(key string, vf *virtualFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= 8 {
		for k := range c.entries {
			delete(c.entries, k)
			break
		}
	}
	c.entries[key] = vf
}

func fgbHeader(data []byte) *flattypes.Header {
	if len(data) < 12 {
		return nil
	}
	return flattypes.GetSizePrefixedRootAsHeader(data, 8)
}

func (s *Server) virtualFile(ctx context.Context, info store.CollectionInfo, tier int) (*virtualFile, error) {
	key := fmt.Sprintf("%s|%d|%d", info.Name, tier, info.Version)
	if cached := s.virtuals.get(key); cached != nil {
		return cached, nil
	}
	vf, err := s.storedVirtual(ctx, info, tier)
	if err != nil {
		return nil, err
	}
	s.virtuals.put(key, vf)
	return vf, nil
}

func (s *Server) storedVirtual(ctx context.Context, info store.CollectionInfo, tier int) (*virtualFile, error) {
	layout, err := s.Store.VirtualLayout(ctx, info.Name, info.Version, tier)
	if err != nil {
		return nil, err
	}
	runs, err := s.Store.VirtualRuns(ctx, info.Name, tier)
	if err != nil {
		return nil, err
	}
	name, version := info.Name, info.Version
	vf := &virtualFile{
		name:    name,
		version: version,
		segments: []virtualSegment{
			{length: int64(layout.HeaderBytes), read: func(ctx context.Context, from, to int64) ([]byte, error) {
				return s.Store.VirtualSegments(ctx, name, version, tier, 0, from, to)
			}},
			{length: layout.IndexBytes, read: func(ctx context.Context, from, to int64) ([]byte, error) {
				return s.Store.VirtualSegments(ctx, name, version, tier, 1, from, to)
			}},
		},
		features: layout.FeatureBytes,
		runs:     make([]virtualRun, 0, len(runs)),
	}
	for _, run := range runs {
		vf.runs = append(vf.runs, virtualRun{chunkID: run.ChunkID, base: run.Base, bytes: run.Bytes})
	}
	return vf, nil
}

func (s *Server) virtualRunBytes(ctx context.Context, collection string, run virtualRun, from, to int64) ([]byte, error) {
	row, err := s.Store.ChunkByID(ctx, collection, run.chunkID)
	if err != nil {
		return nil, err
	}
	raw, err := chunk.Decompress(row.Data)
	if err != nil {
		return nil, err
	}
	header := fgbHeader(raw)
	if header == nil {
		return nil, errors.New("virtual: unreadable chunk")
	}
	featuresAt, _, err := fgb.Regions(raw, header)
	if err != nil {
		return nil, err
	}
	start := featuresAt + int(from)
	end := featuresAt + int(to)
	if start < featuresAt {
		start = featuresAt
	}
	if end > len(raw) {
		end = len(raw)
	}
	if start > end {
		return nil, errors.New("virtual: run out of bounds")
	}
	return raw[start:end], nil
}

func (s *Server) serveVirtual(w http.ResponseWriter, r *http.Request, vf *virtualFile) {
	s.serveRanges(w, r, vf.total(), virtualETag(vf.name, vf.version), func(out io.Writer, from, to int64) error {
		return s.writeVirtual(r.Context(), out, vf, from, to)
	})
}

func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request, etag string, data []byte) {
	s.serveRanges(w, r, int64(len(data)), etag, func(out io.Writer, from, to int64) error {
		_, err := out.Write(data[from:to])
		return err
	})
}

func virtualETag(name string, version int64) string {
	return fmt.Sprintf("\"%s-%d\"", name, version)
}

func (s *Server) serveRanges(w http.ResponseWriter, r *http.Request, total int64, etag string, write func(io.Writer, int64, int64) error) {
	w.Header().Set("Content-Type", "application/flatgeobuf")
	w.Header().Set("Accept-Ranges", "bytes")
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" && etag != "" && r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	from, to := int64(0), total
	status := http.StatusOK
	if rangeHeader != "" {
		var err error
		if from, to, err = parseRange(rangeHeader, total); err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
			http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
			return
		}
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to-1, total))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(to-from, 10))
	w.WriteHeader(status)
	if err := write(w, from, to); err != nil {
		slog.Error("range write failed", "path", r.URL.Path, "from", from, "to", to, "error", err)
	}
}

func parseRange(header string, total int64) (int64, int64, error) {
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, errors.New("unsupported range unit")
	}
	spec := strings.SplitN(strings.TrimPrefix(header, "bytes="), ",", 2)[0]
	start, end, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, errors.New("invalid range")
	}
	if start == "" {
		length, err := strconv.ParseInt(end, 10, 64)
		if err != nil || length <= 0 {
			return 0, 0, errors.New("invalid range")
		}
		from := total - length
		if from < 0 {
			from = 0
		}
		return from, total, nil
	}
	first, err := strconv.ParseInt(start, 10, 64)
	if err != nil || first < 0 {
		return 0, 0, errors.New("invalid range")
	}
	last := total - 1
	if end != "" {
		if last, err = strconv.ParseInt(end, 10, 64); err != nil || last < 0 {
			return 0, 0, errors.New("invalid range")
		}
	}
	if first >= total || last < first {
		return 0, 0, errors.New("range not satisfiable")
	}
	if last >= total {
		last = total - 1
	}
	return first, last + 1, nil
}

func (s *Server) writeVirtual(ctx context.Context, w io.Writer, vf *virtualFile, from, to int64) error {
	segments := make([]virtualSegment, 0, len(vf.segments)+len(vf.runs)+1)
	segments = append(segments, byteSegment(virtualMagic[:], int64(len(virtualMagic))))
	segments = append(segments, vf.segments...)
	for i := range vf.runs {
		run := vf.runs[i]
		segments = append(segments, virtualSegment{length: int64(run.bytes), read: func(ctx context.Context, lo, hi int64) ([]byte, error) {
			return s.virtualRunBytes(ctx, vf.name, run, lo, hi)
		}})
	}
	cursor := int64(0)
	for _, segment := range segments {
		start := cursor
		end := start + segment.length
		cursor = end
		if end <= from || start >= to {
			continue
		}
		body, err := segment.read(ctx, max64(from, start)-start, min64(to, end)-start)
		if err != nil {
			return err
		}
		if _, err := w.Write(body); err != nil {
			return err
		}
	}
	return nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func virtualRequest(r *http.Request) bool {
	if len(parseFilters(r)) > 0 {
		return false
	}
	for _, name := range []string{"bbox", "filter", "filter-lang", "q", "datetime", "limit", "offset", "sortby", "crs", "bbox-crs"} {
		if r.URL.Query().Has(name) {
			return false
		}
	}
	return true
}

func (s *Server) serveVirtualCollection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	info, err := s.Store.Collection(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "collection not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tier := 0
	if value := r.URL.Query().Get("overview"); value != "" && value != "auto" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 || parsed > 2 {
			http.Error(w, "overview must be 0, 1 or 2", http.StatusBadRequest)
			return
		}
		tier = parsed
	}
	vf, err := s.virtualFile(ctx, info, tier)
	if err != nil {
		slog.Error("virtual file failed", "collection", info.Name, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.serveVirtual(w, r, vf)
}

func (s *Server) cacheVersion(info store.CollectionInfo) string {
	return strconv.FormatInt(info.Version, 10)
}
