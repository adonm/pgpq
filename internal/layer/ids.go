package layer

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/spatial"
)

type foundHit struct {
	file int64
	hit  Hit
}

func idHash(id string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(id))
	return h.Sum64()
}

func (l *Layer) Lookup(ctx context.Context, name, id string) (Hit, error) {
	coll, err := l.Collection(ctx, name)
	if err != nil {
		return Hit{}, err
	}
	rows, err := l.pool.Query(ctx, `select key from pfgb.ids where collection_uid = $1 and hash = $2`, coll.UID, int64(idHash(id)))
	if err != nil {
		return Hit{}, err
	}
	var probes []uint64
	for rows.Next() {
		var key int64
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return Hit{}, err
		}
		probes = append(probes, spatial.FromOrdered(key))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Hit{}, err
	}
	rows.Close()
	seen := map[int64]bool{}
	var hits []Hit
	for _, key := range probes {
		found, err := l.findInChunks(ctx, coll, spatial.Partition(key), key, id)
		if err != nil {
			return Hit{}, err
		}
		for _, f := range found {
			if !seen[f.file] {
				seen[f.file] = true
				hits = append(hits, f.hit)
			}
		}
	}
	switch len(hits) {
	case 0:
		return Hit{}, ErrFeatureNotFound
	case 1:
		return hits[0], nil
	}
	return Hit{}, fmt.Errorf("id %q: %w", id, ErrDuplicate)
}

func (l *Layer) findInChunks(ctx context.Context, coll Collection, cell, key uint64, id string) ([]foundHit, error) {
	rows, err := l.pool.Query(ctx, `select file_id from pfgb.chunks where collection = $1 and cell = $2 and first_key <= $3 and last_key >= $3 and level = 0`, coll.Name, spatial.ToOrdered(cell), spatial.ToOrdered(key))
	if err != nil {
		return nil, err
	}
	var fileIDs []int64
	for rows.Next() {
		var fileID int64
		if err := rows.Scan(&fileID); err != nil {
			rows.Close()
			return nil, err
		}
		fileIDs = append(fileIDs, fileID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var out []foundHit
	for _, fileID := range fileIDs {
		data, err := l.blocks.ReadAll(ctx, fileID)
		if err != nil {
			return nil, err
		}
		c, err := chunk.Open(data)
		if err != nil {
			return nil, err
		}
		features, err := c.SearchAll()
		if err != nil {
			return nil, err
		}
		cols := c.Columns()
		for _, f := range features {
			stored, err := storedID(f, cols, coll.IDColumn)
			if err != nil {
				return nil, err
			}
			if stored != id {
				continue
			}
			bounds, err := featureBounds(c, f)
			if err != nil {
				return nil, err
			}
			out = append(out, foundHit{file: fileID, hit: Hit{Key: spatial.Key(bounds), ID: id, Chunk: c, Feature: f}})
		}
	}
	return out, nil
}
