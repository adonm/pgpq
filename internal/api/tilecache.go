package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/adonm/postfgb/internal/store"
)

const maxTileCacheBytes = 1 << 20

func filterKeyOf(r *http.Request) string {
	sum := sha256.Sum256([]byte(r.URL.RawQuery))
	return hex.EncodeToString(sum[:8])
}

func tileETag(version string, tier, z, x, y int, filterKey string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d|%d|%s", version, tier, z, x, y, filterKey)))
	return `"` + hex.EncodeToString(sum[:8]) + `"`
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

func (s *Server) maybeSweep() {
	s.sweepMu.Lock()
	if time.Since(s.lastSweep) < time.Hour {
		s.sweepMu.Unlock()
		return
	}
	s.lastSweep = time.Now()
	s.sweepMu.Unlock()
	go func() {
		if err := s.Store.PurgeTileCache(context.Background()); err != nil {
			slog.Error("tile cache sweep failed", "error", err)
		}
	}()
}
