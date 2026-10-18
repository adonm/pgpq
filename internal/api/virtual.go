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
	collection string
	chunkID    int64
	base       int64
	bytes      int
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

const chunkByteCacheBytes = 64 << 20

type decompressedChunk struct {
	data       []byte
	featuresAt int
}

type chunkByteCache struct {
	mu      sync.Mutex
	entries map[string]decompressedChunk
	bytes   int64
}

func newChunkByteCache() *chunkByteCache {
	return &chunkByteCache{entries: map[string]decompressedChunk{}}
}

func (c *chunkByteCache) get(key string) (decompressedChunk, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	return entry, ok
}

func (c *chunkByteCache) put(key string, entry decompressedChunk) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if int64(len(entry.data)) > chunkByteCacheBytes {
		return
	}
	if old, ok := c.entries[key]; ok {
		c.bytes -= int64(len(old.data))
	}
	c.entries[key] = entry
	c.bytes += int64(len(entry.data))
	for c.bytes > chunkByteCacheBytes {
		evicted := false
		for k := range c.entries {
			c.bytes -= int64(len(c.entries[k].data))
			delete(c.entries, k)
			evicted = true
			break
		}
		if !evicted {
			break
		}
	}
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
	if errors.Is(err, store.ErrNotFound) && tier > 0 {
		tier = 0
		vf, err = s.storedVirtual(ctx, info, tier)
	}
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
	runs, err := s.Store.VirtualRuns(ctx, info, tier)
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
		vf.runs = append(vf.runs, virtualRun{collection: run.Collection, chunkID: run.ChunkID, base: run.Base, bytes: run.Bytes})
	}
	return vf, nil
}

func (s *Server) virtualRunBytes(ctx context.Context, collection string, version int64, run virtualRun, from, to int64) ([]byte, error) {
	key := fmt.Sprintf("%s|%d|%d", collection, run.chunkID, version)
	entry, ok := s.chunks.get(key)
	if !ok {
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
		entry = decompressedChunk{data: raw, featuresAt: featuresAt}
		s.chunks.put(key, entry)
	}
	start := entry.featuresAt + int(from)
	end := entry.featuresAt + int(to)
	if end > len(entry.data) {
		end = len(entry.data)
	}
	if start > end {
		return nil, errors.New("virtual: run out of bounds")
	}
	return entry.data[start:end], nil
}

func (s *Server) serveVirtual(w http.ResponseWriter, r *http.Request, vf *virtualFile) {
	s.serveRanges(w, r, vf.total(), virtualETag(vf.name, vf.version), func(out io.Writer, from, to int64) error {
		return s.writeVirtual(r.Context(), out, vf, from, to)
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
	cursor := int64(0)
	if err := writeRegion(w, &cursor, int64(len(virtualMagic)), from, to, func(lo, hi int64) ([]byte, error) {
		return virtualMagic[lo:hi], nil
	}); err != nil {
		return err
	}
	for i := range vf.segments {
		segment := &vf.segments[i]
		if err := writeRegion(w, &cursor, segment.length, from, to, func(lo, hi int64) ([]byte, error) {
			return segment.read(ctx, lo, hi)
		}); err != nil {
			return err
		}
	}
	for i := range vf.runs {
		if err := s.writeRun(ctx, w, &cursor, vf, &vf.runs[i], from, to); err != nil {
			return err
		}
	}
	return nil
}

func writeRegion(w io.Writer, cursor *int64, length, from, to int64, read func(lo, hi int64) ([]byte, error)) error {
	start := *cursor
	end := start + length
	*cursor = end
	if end <= from || start >= to {
		return nil
	}
	body, err := read(max(from, start)-start, min(to, end)-start)
	if err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func (s *Server) writeRun(ctx context.Context, w io.Writer, cursor *int64, vf *virtualFile, run *virtualRun, from, to int64) error {
	start := *cursor
	end := start + int64(run.bytes)
	*cursor = end
	if end <= from || start >= to {
		return nil
	}
	body, err := s.virtualRunBytes(ctx, run.collection, vf.version, *run, max(from, start)-start, min(to, end)-start)
	if err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func virtualRequest(r *http.Request) bool {
	for name := range r.URL.Query() {
		switch name {
		case "limit", "offset", "overview", "f":
			continue
		}
		return false
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
	overview, err := parseOverview(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tier := 0
	if overview != nil && info.Kind != "view" {
		tier = *overview
	}
	vf, err := s.virtualFile(ctx, info, tier)
	if err != nil {
		slog.Error("virtual file failed", "collection", info.Name, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.serveVirtual(w, r, vf)
}
