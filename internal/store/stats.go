package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/postfgb/internal/chunk"
	"github.com/adonm/postfgb/internal/stats"
)

type BackfillResult struct {
	Collection string
	Chunks     int
	Rows       int64
	Bytes      int64
}

type chunkData struct {
	ID   int64
	Data []byte
}

func (s *Store) BackfillStats(ctx context.Context, collection string) ([]BackfillResult, error) {
	names, err := s.statsCollections(ctx, collection)
	if err != nil {
		return nil, err
	}
	results := make([]BackfillResult, 0, len(names))
	for _, name := range names {
		result, err := s.backfillStats(ctx, name)
		if err != nil {
			return results, fmt.Errorf("%s: %w", name, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func (s *Store) statsCollections(ctx context.Context, collection string) ([]string, error) {
	if collection == "" {
		return queryAll(ctx, s.Pool, pgx.RowTo[string], `select name from collections order by name`)
	}
	if !ValidName(collection) {
		return nil, fmt.Errorf("invalid collection name %q", collection)
	}
	if _, err := s.Collection(ctx, collection); err != nil {
		return nil, err
	}
	return []string{collection}, nil
}

func (s *Store) backfillStats(ctx context.Context, collection string) (BackfillResult, error) {
	result := BackfillResult{Collection: collection}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `create table if not exists `+ident(partitionName("chunk_stats", collection))+
		` partition of chunk_stats for values in ('`+collection+`')`); err != nil {
		return result, err
	}
	chunks, err := queryAll(ctx, tx, func(row pgx.CollectableRow) (chunkData, error) {
		var c chunkData
		err := row.Scan(&c.ID, &c.Data)
		return c, err
	}, `select id, data from chunks where collection = $1 order by id`, collection)
	if err != nil {
		return result, err
	}
	var ids []int64
	var names []string
	var mins, maxs []*float64
	var nulls []int64
	var blooms [][]byte
	for _, row := range chunks {
		c, err := chunk.Open(row.Data)
		if err != nil {
			return result, err
		}
		features, err := c.SearchAll()
		if err != nil {
			return result, err
		}
		cols := c.Columns()
		props := make([]map[string]any, len(features))
		for i, f := range features {
			props[i] = chunk.DecodeProperties(f.PropertiesBytes(), cols)
		}
		for _, stat := range stats.Compute(cols, props) {
			ids = append(ids, row.ID)
			names = append(names, stat.Column)
			mins = append(mins, stat.Min)
			maxs = append(maxs, stat.Max)
			nulls = append(nulls, stat.Nulls)
			blooms = append(blooms, stat.Bloom)
			result.Rows++
			result.Bytes += int64(len(stat.Bloom))
		}
		result.Chunks++
	}
	if _, err := tx.Exec(ctx, `delete from chunk_stats where collection = $1`, collection); err != nil {
		return result, err
	}
	if len(names) > 0 {
		if _, err := tx.Exec(ctx, `
			insert into chunk_stats (collection, chunk_id, "column", min, max, nulls, bloom)
			select $1, input.chunk_id, input.name, input.min, input.max, input.nulls, input.bloom
			from unnest($2::bigint[], $3::text[], $4::float8[], $5::float8[], $6::bigint[], $7::bytea[])
				as input(chunk_id, name, min, max, nulls, bloom)`,
			collection, ids, names, mins, maxs, nulls, blooms); err != nil {
			return result, err
		}
	}
	if _, err := tx.Exec(ctx, `update collections set stats_version = version where name = $1`, collection); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}
