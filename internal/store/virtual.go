package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/fgb"
)

type VirtualLayout struct {
	HeaderBytes  int
	IndexBytes   int64
	FeatureBytes int64
	Chunks       int
	Features     int64
}

type VirtualRun struct {
	Collection string
	ChunkID    int64
	Base       int64
	Bytes      int
}

type virtualTier struct {
	header  []byte
	leaves  [][]fgb.Leaf
	lengths []int64
	count   uint64
}

func (i *Ingest) buildVirtual(ctx context.Context) error {
	var version int64
	if err := i.tx.QueryRow(ctx, `select version from collections where name = $1`, i.collection).Scan(&version); err != nil {
		return err
	}
	if _, err := i.tx.Exec(ctx, `delete from virtual_file where collection = $1 and version <> $2`, i.collection, version); err != nil {
		return err
	}
	if _, err := i.tx.Exec(ctx, `delete from virtual_segment where collection = $1 and version <> $2`, i.collection, version); err != nil {
		return err
	}
	for tier, chunks := range i.virtual {
		if len(chunks.leaves) == 0 {
			continue
		}
		indexBytes, envelope, ok := fgb.Index(chunks.leaves, chunks.lengths, fgb.NodeSize)
		if !ok {
			continue
		}
		header, ok := fgb.Header(i.collection, chunks.header, chunks.count, envelope)
		if !ok {
			continue
		}
		var featureBytes int64
		for _, length := range chunks.lengths {
			featureBytes += length
		}
		if _, err := i.tx.Exec(ctx, `
			insert into virtual_file (collection, version, tier, header_bytes, index_bytes, feature_bytes, chunks)
			values ($1, $2, $3, $4, $5, $6, $7)
			on conflict (collection, version, tier) do update set
				header_bytes = excluded.header_bytes, index_bytes = excluded.index_bytes,
				feature_bytes = excluded.feature_bytes, chunks = excluded.chunks`,
			i.collection, version, tier, len(header), len(indexBytes), featureBytes, len(chunks.lengths)); err != nil {
			return err
		}
		if err := i.putSegment(ctx, version, tier, 0, 0, header); err != nil {
			return err
		}
		for ordinal, at := 0, 0; at < len(indexBytes); ordinal++ {
			end := at + int(fgb.SegmentBytes)
			if end > len(indexBytes) {
				end = len(indexBytes)
			}
			if err := i.putSegment(ctx, version, tier, 1, ordinal, indexBytes[at:end]); err != nil {
				return err
			}
			at = end
		}
	}
	return nil
}

func (i *Ingest) putSegment(ctx context.Context, version int64, tier, kind, ordinal int, data []byte) error {
	_, err := i.tx.Exec(ctx, `
		insert into virtual_segment (collection, version, tier, kind, ordinal, data)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (collection, version, tier, kind, ordinal) do update set data = excluded.data`,
		i.collection, version, tier, kind, ordinal, data)
	return err
}

func (s *Store) VirtualLayout(ctx context.Context, collection string, version int64, tier int) (VirtualLayout, error) {
	var layout VirtualLayout
	err := s.Pool.QueryRow(ctx, `
		select header_bytes, index_bytes, feature_bytes, chunks, features from virtual_file
		where collection = $1 and version = $2 and tier = $3`, collection, version, tier).
		Scan(&layout.HeaderBytes, &layout.IndexBytes, &layout.FeatureBytes, &layout.Chunks, &layout.Features)
	if errors.Is(err, pgx.ErrNoRows) {
		return layout, ErrNotFound
	}
	return layout, err
}

func (s *Store) VirtualSegments(ctx context.Context, collection string, version int64, tier, kind int, from, to int64) ([]byte, error) {
	return readVirtual(ctx, s.Pool, collection, version, tier, kind, from, to)
}

func (s *Store) VirtualRuns(ctx context.Context, collection string, tier int) ([]VirtualRun, error) {
	rows, err := s.Pool.Query(ctx, `
		select id, feature_bytes, coalesce(sum(feature_bytes) over (order by id rows between unbounded preceding and 1 preceding), 0)
		from chunks where collection = $1 and tier = $2 order by id`, collection, tier)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []VirtualRun
	for rows.Next() {
		var run VirtualRun
		if err := rows.Scan(&run.ChunkID, &run.Bytes, &run.Base); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}
