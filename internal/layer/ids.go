package layer

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/adonm/pgpq/internal/spatial"
)

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
	rows, err := l.pool.Query(ctx, `select cell, row from pgpq.ids where collection_uid = $1 and hash = $2`, coll.UID, int64(idHash(id)))
	if err != nil {
		return Hit{}, err
	}
	var hits []Hit
	for rows.Next() {
		var cell int64
		var row int
		if err := rows.Scan(&cell, &row); err != nil {
			rows.Close()
			return Hit{}, err
		}
		meta := cellMeta{cell: spatial.FromOrdered(cell), level: 0}
		file, err := l.cellFile(ctx, coll, meta)
		if err != nil {
			rows.Close()
			return Hit{}, err
		}
		feature, err := file.FeatureAt(row, coll.IDColumn)
		if err != nil {
			rows.Close()
			return Hit{}, err
		}
		if feature.ID == id {
			hits = append(hits, Hit{Key: feature.Key, Feature: feature})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Hit{}, err
	}
	rows.Close()
	switch len(hits) {
	case 0:
		return Hit{}, ErrFeatureNotFound
	case 1:
		return hits[0], nil
	}
	return Hit{}, fmt.Errorf("id %q: %w", id, ErrDuplicate)
}
