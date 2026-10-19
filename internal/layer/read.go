package layer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"
	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/spatial"
	"github.com/adonm/postfgb/internal/stats"
)

type Query struct {
	BBox   *[4]float64
	Limit  int
	Cursor *Cursor
}

type Hit struct {
	Key     uint64
	ID      string
	Chunk   *chunk.Chunk
	Feature *flattypes.Feature
}

type Page struct {
	Collection string
	Snapshot   int64
	Hits       []Hit
	Next       *Cursor
}

type chunkMeta struct {
	fileID  int64
	lastKey uint64
	lastID  string
}

func (l *Layer) Items(ctx context.Context, name string, q Query) (Page, error) {
	if q.Limit <= 0 {
		return Page{}, invalidf("limit must be positive")
	}
	coll, err := l.Collection(ctx, name)
	if err != nil {
		return Page{}, err
	}
	snapshot := coll.Snapshot
	if q.Cursor != nil {
		if q.Cursor.UID != coll.UID {
			return Page{}, ErrBadCursor
		}
		created, found, err := l.snapshotCreated(ctx, name, q.Cursor.Snapshot)
		if err != nil {
			return Page{}, err
		}
		if !found || time.Since(created) > Retention {
			return Page{}, ErrGone
		}
		snapshot = q.Cursor.Snapshot
	}
	metas, err := l.chunksAt(ctx, name, snapshot, q.BBox)
	if err != nil {
		return Page{}, err
	}
	page := Page{Collection: name, Snapshot: snapshot}
	for _, meta := range metas {
		if len(page.Hits) == q.Limit {
			break
		}
		if q.Cursor != nil && compareKeyID(meta.lastKey, meta.lastID, q.Cursor.Key, q.Cursor.ID) <= 0 {
			continue
		}
		hits, err := l.matches(ctx, coll.IDColumn, meta, q)
		if err != nil {
			return Page{}, err
		}
		for _, hit := range hits {
			if len(page.Hits) == q.Limit {
				break
			}
			page.Hits = append(page.Hits, hit)
		}
	}
	if len(page.Hits) == q.Limit {
		last := page.Hits[len(page.Hits)-1]
		page.Next = &Cursor{UID: coll.UID, Snapshot: snapshot, Key: last.Key, ID: last.ID}
	}
	return page, nil
}

func (l *Layer) matches(ctx context.Context, idColumn string, meta chunkMeta, q Query) ([]Hit, error) {
	data, err := l.blocks.ReadAll(ctx, meta.fileID)
	if err != nil {
		return nil, err
	}
	c, err := chunk.Open(data)
	if err != nil {
		return nil, err
	}
	var features []*flattypes.Feature
	if q.BBox != nil {
		features, err = c.Search(q.BBox[0], q.BBox[1], q.BBox[2], q.BBox[3])
	} else {
		features, err = c.SearchAll()
	}
	if err != nil {
		return nil, err
	}
	cols := c.Columns()
	var hits []Hit
	for _, f := range features {
		shape, err := chunk.ParseFlat(c.Header(), f)
		if err != nil {
			return nil, err
		}
		if shape == nil {
			return nil, errors.New("layer: stored feature has no geometry")
		}
		bounds, ok := shape.Bound()
		if !ok {
			return nil, errors.New("layer: stored feature has no extent")
		}
		key := spatial.Key(bounds)
		id, err := storedID(f, cols, idColumn)
		if err != nil {
			return nil, err
		}
		if q.Cursor != nil && compareKeyID(key, id, q.Cursor.Key, q.Cursor.ID) <= 0 {
			continue
		}
		hits = append(hits, Hit{Key: key, ID: id, Chunk: c, Feature: f})
	}
	slices.SortFunc(hits, func(a, b Hit) int { return compareKeyID(a.Key, a.ID, b.Key, b.ID) })
	return hits, nil
}

func storedID(f *flattypes.Feature, cols []chunk.Column, column string) (string, error) {
	value, ok := chunk.DecodeProperties(f.PropertiesBytes(), cols)[column]
	if !ok || value == nil {
		return "", fmt.Errorf("layer: stored feature lacks id %q", column)
	}
	text, ok := stats.CanonicalText(value)
	if !ok || text == "" {
		return "", fmt.Errorf("layer: stored id %q is unusable", column)
	}
	return text, nil
}

func (l *Layer) chunksAt(ctx context.Context, name string, snapshot int64, bbox *[4]float64) ([]chunkMeta, error) {
	sql := `select file_id, last_key, last_id from pfgb.chunks where collection = $1 and begin_snapshot <= $2 and (end_snapshot is null or end_snapshot > $2)`
	args := []any{name, snapshot}
	if bbox != nil {
		sql += ` and maxx >= $3 and minx <= $5 and maxy >= $4 and miny <= $6`
		args = append(args, bbox[0], bbox[1], bbox[2], bbox[3])
	}
	sql += ` order by first_key, first_id collate "C"`
	rows, err := l.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var metas []chunkMeta
	for rows.Next() {
		var m chunkMeta
		var lastKey int64
		if err := rows.Scan(&m.fileID, &lastKey, &m.lastID); err != nil {
			return nil, err
		}
		m.lastKey = spatial.FromOrdered(lastKey)
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

func (l *Layer) snapshotCreated(ctx context.Context, name string, snapshot int64) (time.Time, bool, error) {
	var created time.Time
	err := l.pool.QueryRow(ctx, `select created from pfgb.snapshots where collection = $1 and id = $2`, name, snapshot).Scan(&created)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return created, true, nil
}
