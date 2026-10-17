package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/adonm/postfgb/internal/store"
)

const (
	viewFileMaxBytes  = 32 << 20
	viewCacheMaxBytes = 128 << 20
)

type viewFile struct {
	data []byte
	etag string
}

type viewFileCache struct {
	mu      sync.Mutex
	entries map[string]*viewFile
	locks   map[string]*sync.Mutex
	bytes   int64
}

func newViewFileCache() *viewFileCache {
	return &viewFileCache{entries: map[string]*viewFile{}, locks: map[string]*sync.Mutex{}}
}

func (c *viewFileCache) get(key string) *viewFile {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[key]
}

func (c *viewFileCache) put(key string, file *viewFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if file == nil || int64(len(file.data)) > viewFileMaxBytes {
		return
	}
	if existing := c.entries[key]; existing != nil {
		c.bytes -= int64(len(existing.data))
	}
	c.entries[key] = file
	c.bytes += int64(len(file.data))
	for c.bytes > viewCacheMaxBytes {
		evicted := false
		for k := range c.entries {
			if k == key {
				continue
			}
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

func (c *viewFileCache) lock(key string) func() {
	c.mu.Lock()
	held := c.locks[key]
	if held == nil {
		held = &sync.Mutex{}
		c.locks[key] = held
	}
	c.mu.Unlock()
	held.Lock()
	return held.Unlock
}

func (s *Server) viewFileKey(ctx context.Context, info store.CollectionInfo, r *http.Request) (string, error) {
	version, err := s.cacheVersion(ctx, info)
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	fmt.Fprintf(sum, "%s|%s|", info.Name, version)
	query := r.URL.Query()
	names := make([]string, 0, len(query))
	for name := range query {
		if name != "f" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(sum, "%s=%s|", name, query.Get(name))
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func (s *Server) viewFile(ctx context.Context, r *http.Request, info store.CollectionInfo, q featureQuery) ([]byte, string, error) {
	key, err := s.viewFileKey(ctx, info, r)
	if err != nil {
		return nil, "", err
	}
	if cached := s.viewFiles.get(key); cached != nil {
		return cached.data, cached.etag, nil
	}
	unlock := s.viewFiles.lock(key)
	defer unlock()
	if cached := s.viewFiles.get(key); cached != nil {
		return cached.data, cached.etag, nil
	}
	data, _, err := s.viewFGB(ctx, info, q)
	if err != nil || data == nil {
		return nil, "", err
	}
	etag := fmt.Sprintf("%q", key[:16])
	if int64(len(data)) <= viewFileMaxBytes {
		s.viewFiles.put(key, &viewFile{data: data, etag: etag})
	}
	return data, etag, nil
}

func (s *Server) serveViewFile(w http.ResponseWriter, r *http.Request) {
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
	limit, err := parseLimit(r, flatgeobufLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	offset, err := parseOffset(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q, err := s.parseFeatureQuery(ctx, r, info, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data, etag, err := s.viewFile(ctx, r, info, q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if data == nil {
		w.Header().Set("Content-Type", "application/geo+json")
		fw := newFeatureWriter(w, limit, nil)
		fw.close("")
		return
	}
	s.serveBlob(w, r, etag, data)
}
