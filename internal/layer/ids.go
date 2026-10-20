package layer

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/fgb"
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
	var fileID, fenceFile int64
	err := l.pool.QueryRow(ctx, `select file_id, fence_file from pfgb.chunks where collection = $1 and cell = $2 and level = 0`,
		coll.Name, spatial.ToOrdered(cell)).Scan(&fileID, &fenceFile)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fenceFile == 0 {
		return nil, errors.New("layer: chunk has no key fence; reload the collection")
	}
	found, err := l.findInCell(ctx, coll, fileID, fenceFile, key, id)
	if err != nil {
		return nil, err
	}
	return found, nil
}

func (l *Layer) findInCell(ctx context.Context, coll Collection, fileID, fenceFile int64, key uint64, id string) ([]foundHit, error) {
	cell, err := l.cellFile(ctx, fileID, fenceFile)
	if err != nil {
		return nil, err
	}
	reader, err := l.blocks.Open(ctx, fileID)
	if err != nil {
		return nil, stale(err)
	}
	var out []foundHit
	rows := cell.layout.Rows()
	at := cell.fence.StartRow(key, id)
	for at < len(rows) {
		end := min(at+2*fgb.FenceEvery, len(rows))
		features, err := cell.layout.Features(reader, rows[at:end])
		if err != nil {
			return nil, err
		}
		for _, f := range features {
			bounds, ok := chunk.Bounds(cell.header, f)
			if !ok {
				return nil, errors.New("layer: stored feature has no geometry")
			}
			foundKey := spatial.Key(bounds)
			if foundKey > key {
				return out, nil
			}
			if foundKey != key {
				continue
			}
			stored, err := storedID(f, cell.cols, coll.IDColumn)
			if err != nil {
				return nil, err
			}
			if stored == id {
				out = append(out, foundHit{file: fileID, hit: Hit{Key: foundKey, ID: id, Header: cell.header, Feature: f}})
			}
		}
		at = end
	}
	return out, nil
}
