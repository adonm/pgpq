package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/flatgeobuf/flatgeobuf/src/go/index"
	"github.com/flatgeobuf/flatgeobuf/src/go/writer"
	flatbuffers "github.com/google/flatbuffers/go"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/store"
)

const virtualNodeSize uint16 = 16

var virtualMagic = [8]byte{0x66, 0x67, 0x62, 0x03, 0x66, 0x67, 0x62, 0x00}

type virtualRun struct {
	chunkID int64
	base    int64
	bytes   int
}

type virtualFile struct {
	name     string
	version  int64
	header   []byte
	index    []byte
	runs     []virtualRun
	features int64
}

func (v *virtualFile) total() int64 {
	return int64(len(v.header)) + int64(len(v.index)) + v.features + int64(len(virtualMagic))
}

type virtualItem struct {
	offset uint64
	bbox   [4]float64
}

func (v virtualItem) NodeItem() index.NodeItem {
	return index.NewNodeItemWithCoordinates(v.offset, v.bbox[0], v.bbox[1], v.bbox[2], v.bbox[3])
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
	metas, err := s.Store.ChunkMetas(ctx, info.Name, nil, 1_000_000, tier, 0)
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, errors.New("collection has no chunks at that overview")
	}
	vf := &virtualFile{name: info.Name, version: info.Version}
	items := make([]virtualItem, 0, len(metas))
	var src *flattypes.Header
	extent := index.NewNodeItem(0)
	var base int64
	for _, meta := range metas {
		row, err := s.Store.ChunkByID(ctx, info.Name, meta.ID)
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
		if src == nil {
			src = header
		}
		featuresAt, featuresLen, err := fgb.Regions(raw, header)
		if err != nil {
			return nil, err
		}
		indexLen := indexLength(header)
		if indexLen == 0 && header.FeaturesCount() > 0 {
			return nil, errors.New("virtual: chunk has no index")
		}
		for _, leaf := range readLeaves(raw[featuresAt-indexLen:featuresAt], header.FeaturesCount(), header.IndexNodeSize()) {
			item := virtualItem{offset: uint64(base) + leaf.offset, bbox: leaf.bbox}
			items = append(items, item)
			extent.Expand(item.NodeItem())
		}
		vf.runs = append(vf.runs, virtualRun{chunkID: meta.ID, base: base, bytes: featuresLen})
		base += int64(featuresLen)
	}
	if src == nil {
		return nil, errors.New("virtual: no chunks")
	}
	vf.features = base
	sort.Slice(items, func(i, j int) bool { return items[i].offset < items[j].offset })
	treeItems := make([]index.Item, len(items))
	for i := range items {
		treeItems[i] = items[i]
	}
	tree := index.NewPackedRTreeWithItems(treeItems, extent, virtualNodeSize)
	var buf bytes.Buffer
	if _, err := tree.Write(&buf); err != nil {
		return nil, err
	}
	vf.index = buf.Bytes()
	vf.header = virtualHeader(info.Name, src, uint64(len(items)), extent.ToSlice())
	s.virtuals.put(key, vf)
	return vf, nil
}

func virtualHeader(name string, src *flattypes.Header, count uint64, envelope []float64) []byte {
	builder := flatbuffers.NewBuilder(0)
	header := writer.NewHeader(builder).
		SetName(name).
		SetEnvelope(envelope).
		SetGeometryType(src.GeometryType()).
		SetHasZ(src.HasZ()).
		SetHasM(src.HasM()).
		SetFeaturesCount(count).
		SetIndexNodeSize(virtualNodeSize)
	columns := make([]*writer.Column, src.ColumnsLength())
	for i := range columns {
		var column flattypes.Column
		src.Columns(&column, i)
		columns[i] = writer.NewColumn(builder).SetName(string(column.Name())).SetType(column.Type())
	}
	header.SetColumns(columns)
	var crs flattypes.Crs
	if src.Crs(&crs) != nil {
		header.SetCrs(writer.NewCrs(builder).
			SetOrg(string(crs.Org())).
			SetCode(crs.Code()).
			SetName(string(crs.Name())).
			SetCodeString(string(crs.CodeString())))
	}
	offset := header.Build()
	builder.FinishSizePrefixed(offset)
	return builder.FinishedBytes()
}

type virtualLeaf struct {
	bbox   [4]float64
	offset uint64
}

func readLeaves(data []byte, count uint64, nodeSize uint16) []virtualLeaf {
	if count == 0 || nodeSize < 2 {
		return nil
	}
	leaves := make([]virtualLeaf, 0, count)
	for i := uint64(0); i < count; i++ {
		at := int((numNodes(count, nodeSize) - count + i) * 40)
		if at+40 > len(data) {
			break
		}
		leaves = append(leaves, virtualLeaf{
			bbox: [4]float64{
				math.Float64frombits(binary.LittleEndian.Uint64(data[at:])),
				math.Float64frombits(binary.LittleEndian.Uint64(data[at+8:])),
				math.Float64frombits(binary.LittleEndian.Uint64(data[at+16:])),
				math.Float64frombits(binary.LittleEndian.Uint64(data[at+24:])),
			},
			offset: binary.LittleEndian.Uint64(data[at+32:]),
		})
	}
	return leaves
}

func numNodes(count uint64, nodeSize uint16) uint64 {
	nodes := count
	for n := count; n > 1; {
		n = (n + uint64(nodeSize) - 1) / uint64(nodeSize)
		nodes += n
	}
	return nodes
}

func indexLength(header *flattypes.Header) int {
	nodeSize := header.IndexNodeSize()
	count := header.FeaturesCount()
	if nodeSize < 2 || count == 0 {
		return 0
	}
	nodes := count
	for n := count; n > 1; {
		n = (n + uint64(nodeSize) - 1) / uint64(nodeSize)
		nodes += n
	}
	return int(nodes * 40)
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
	segments := []virtualSegment{
		{start: 0, body: virtualMagic[:]},
		{start: int64(len(virtualMagic)), body: vf.header},
		{body: vf.index},
	}
	featuresStart := int64(len(virtualMagic)) + int64(len(vf.header)) + int64(len(vf.index))
	for i := range vf.runs {
		run := vf.runs[i]
		segments = append(segments, virtualSegment{
			start:  featuresStart + run.base,
			length: int64(run.bytes),
			run:    &run,
		})
	}
	cursor := int64(0)
	for i := range segments {
		segment := &segments[i]
		if segment.length == 0 {
			segment.length = int64(len(segment.body))
		}
		segmentStart := segment.start
		if segment.body != nil {
			segmentStart = cursor
		}
		segmentEnd := segmentStart + segment.length
		cursor = segmentEnd
		if segmentEnd <= from || segmentStart >= to {
			continue
		}
		lo := max64(from, segmentStart) - segmentStart
		hi := min64(to, segmentEnd) - segmentStart
		if segment.body != nil {
			if _, err := w.Write(segment.body[lo:hi]); err != nil {
				return err
			}
			continue
		}
		body, err := s.virtualRunBytes(ctx, vf.name, *segment.run, lo, hi)
		if err != nil {
			return err
		}
		if _, err := w.Write(body); err != nil {
			return err
		}
	}
	return nil
}

type virtualSegment struct {
	start  int64
	length int64
	body   []byte
	run    *virtualRun
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
	if info.Kind != "native" {
		s.serveViewFile(w, r)
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

func (s *Server) cacheVersion(ctx context.Context, info store.CollectionInfo) (string, error) {
	if info.Kind != "view" {
		return strconv.FormatInt(info.Version, 10), nil
	}
	members, err := parseMembers(info)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(info.Members)
	fmt.Fprintf(h, "|%d", info.Version)
	for _, member := range members {
		mi, err := s.Store.Collection(ctx, member.Collection)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "|%s:%d", mi.Name, mi.Version)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}
