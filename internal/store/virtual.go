package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/fgb"
)

type VirtualLayout struct {
	HeaderBytes  int
	IndexBytes   int64
	FeatureBytes int64
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

type tierLayout struct {
	header, index                  []byte
	features, chunks, featureBytes int64
}

func (i *Ingest) buildVirtual(ctx context.Context) error {
	var version int64
	if err := i.tx.QueryRow(ctx, `select version from collections where name = $1`, i.collection).Scan(&version); err != nil {
		return err
	}
	layouts := map[int]tierLayout{}
	for tier, chunks := range i.virtual {
		if len(chunks.leaves) == 0 {
			continue
		}
		index, envelope, ok := fgb.Index(chunks.leaves, chunks.lengths, fgb.NodeSize)
		if !ok {
			continue
		}
		header, ok := fgb.Header(i.collection, chunks.header, chunks.count, envelope)
		if !ok {
			continue
		}
		layouts[tier] = tierLayout{
			header: header, index: index, features: int64(chunks.count),
			chunks: int64(len(chunks.lengths)), featureBytes: total(chunks.lengths),
		}
	}
	return writeLayouts(ctx, i.tx, i.collection, version, layouts)
}

func writeLayouts(ctx context.Context, tx pgx.Tx, collection string, version int64, tiers map[int]tierLayout) error {
	if _, err := tx.Exec(ctx, `delete from virtual_file where collection = $1 and version <> $2`, collection, version); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from virtual_segment where collection = $1 and version <> $2`, collection, version); err != nil {
		return err
	}
	for tier, layout := range tiers {
		if _, err := tx.Exec(ctx, `
			insert into virtual_file (collection, version, tier, header_bytes, index_bytes, feature_bytes, chunks, features)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
			on conflict (collection, version, tier) do update set
				header_bytes = excluded.header_bytes, index_bytes = excluded.index_bytes,
				feature_bytes = excluded.feature_bytes, chunks = excluded.chunks, features = excluded.features`,
			collection, version, tier, len(layout.header), len(layout.index), layout.featureBytes, layout.chunks, layout.features); err != nil {
			return err
		}
		for kind, data := range [][]byte{layout.header, layout.index} {
			for ordinal, at := 0, 0; at < len(data); ordinal++ {
				end := min(at+int(fgb.SegmentBytes), len(data))
				if _, err := tx.Exec(ctx, `
					insert into virtual_segment (collection, version, tier, kind, ordinal, data)
					values ($1, $2, $3, $4, $5, $6)
					on conflict (collection, version, tier, kind, ordinal) do update set data = excluded.data`,
					collection, version, tier, kind, ordinal, data[at:end]); err != nil {
					return err
				}
				at = end
			}
		}
	}
	return nil
}

func (s *Store) VirtualLayout(ctx context.Context, collection string, version int64, tier int) (VirtualLayout, error) {
	return virtualLayout(ctx, s.Pool, collection, version, tier)
}

func virtualLayout(ctx context.Context, conn db, collection string, version int64, tier int) (VirtualLayout, error) {
	var layout VirtualLayout
	err := conn.QueryRow(ctx, `
		select header_bytes, index_bytes, feature_bytes, features from virtual_file
		where collection = $1 and version = $2 and tier = $3`, collection, version, tier).
		Scan(&layout.HeaderBytes, &layout.IndexBytes, &layout.FeatureBytes, &layout.Features)
	if errors.Is(err, pgx.ErrNoRows) {
		return layout, ErrNotFound
	}
	return layout, err
}

func (s *Store) VirtualSegments(ctx context.Context, collection string, version int64, tier, kind int, from, to int64) ([]byte, error) {
	return readVirtual(ctx, s.Pool, collection, version, tier, kind, from, to)
}

func readVirtual(ctx context.Context, conn db, collection string, version int64, tier, kind int, from, to int64) ([]byte, error) {
	first := int(from / fgb.SegmentBytes)
	last := int((to - 1) / fgb.SegmentBytes)
	rows, err := conn.Query(ctx, `
		select ordinal, data from virtual_segment
		where collection = $1 and version = $2 and tier = $3 and kind = $4 and ordinal between $5 and $6
		order by ordinal`, collection, version, tier, kind, first, last)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]byte, 0, to-from)
	for rows.Next() {
		var ordinal int
		var data []byte
		if err := rows.Scan(&ordinal, &data); err != nil {
			return nil, err
		}
		start := int64(ordinal) * fgb.SegmentBytes
		lo := max(from-start, 0)
		hi := min(to-start, int64(len(data)))
		if lo >= hi {
			continue
		}
		out = append(out, data[lo:hi]...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if int64(len(out)) != to-from {
		return nil, errors.New("virtual: segment gap")
	}
	return out, nil
}

func storedFeatures(ctx context.Context, conn db, collection string, layout VirtualLayout) (int64, error) {
	if layout.Features > 0 {
		return layout.Features, nil
	}
	var count int64
	err := conn.QueryRow(ctx,
		`select coalesce(sum(feature_count), 0) from chunks where collection = $1 and tier = 0`, collection).Scan(&count)
	return count, err
}

func readLeaves(ctx context.Context, conn db, collection string, version int64, layout VirtualLayout, count int64) ([]fgb.Leaf, error) {
	index, err := readVirtual(ctx, conn, collection, version, 0, 1, 0, layout.IndexBytes)
	if err != nil {
		return nil, err
	}
	leaves, ok := fgb.Leaves(index, uint64(count), fgb.NodeSize)
	if !ok {
		return nil, errors.New("virtual: unreadable leaves")
	}
	if int64(len(leaves)) != count {
		return nil, fmt.Errorf("virtual: read %d leaves of %d", len(leaves), count)
	}
	return leaves, nil
}

func (s *Store) VirtualRuns(ctx context.Context, info CollectionInfo, tier int) ([]VirtualRun, error) {
	if info.Kind != "view" {
		return chunkRuns(ctx, s.Pool, info.Name, tier, 0)
	}
	members, err := viewMembers(ctx, s.Pool, info.Members)
	if err != nil {
		return nil, err
	}
	var runs []VirtualRun
	var base int64
	for _, member := range members {
		layout, err := virtualLayout(ctx, s.Pool, member.Name, member.Version, 0)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		part, err := chunkRuns(ctx, s.Pool, member.Name, 0, base)
		if err != nil {
			return nil, err
		}
		runs = append(runs, part...)
		base += layout.FeatureBytes
	}
	return runs, nil
}

func chunkRuns(ctx context.Context, conn db, collection string, tier int, base int64) ([]VirtualRun, error) {
	return queryAll(ctx, conn, func(row pgx.CollectableRow) (VirtualRun, error) {
		run := VirtualRun{Collection: collection}
		err := row.Scan(&run.ChunkID, &run.Bytes, &run.Base)
		run.Base += base
		return run, err
	}, `select id, feature_bytes, coalesce(sum(feature_bytes) over (order by id rows between unbounded preceding and 1 preceding), 0)
		from chunks where collection = $1 and tier = $2 order by id`, collection, tier)
}

func total(values []int64) int64 {
	var sum int64
	for _, value := range values {
		sum += value
	}
	return sum
}
