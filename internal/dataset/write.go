package dataset

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/adonm/pgpq/internal/geo"
	"github.com/adonm/pgpq/internal/spatial"
	"github.com/adonm/pgpq/internal/vparquet"
)

type encodedShard struct {
	data     []byte
	features []geo.Feature
	record   shardRecord
}

type placedShard struct {
	record   shardRecord
	features []geo.Feature
}

func encodeShards(ds geo.Dataset, features []geo.Feature, maxShardBytes int64) ([]encodedShard, error) {
	var out []encodedShard
	var walk func([]geo.Feature) error
	walk = func(part []geo.Feature) error {
		data, err := geo.EncodeShard(ds, part)
		if err != nil {
			return err
		}
		if int64(len(data)) > maxShardBytes && len(part) > 1 {
			middle := len(part) / 2
			if err := walk(part[:middle]); err != nil {
				return err
			}
			return walk(part[middle:])
		}
		if int64(len(data)) > maxShardBytes {
			return invalidf("one feature exceeds the %d-byte shard limit", maxShardBytes)
		}
		out = append(out, encodedShard{data: data, features: part})
		return nil
	}
	if err := walk(features); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *Manager) Put(ctx context.Context, name string, ds geo.Dataset) (Result, error) {
	features, err := prepare(ds)
	if err != nil {
		return Result{}, err
	}
	return m.write(ctx, name, ds, features, true)
}

func (m *Manager) Upsert(ctx context.Context, name string, ds geo.Dataset) (Result, error) {
	features, err := prepare(ds)
	if err != nil {
		return Result{}, err
	}
	return m.write(ctx, name, ds, features, false)
}

func (m *Manager) write(ctx context.Context, name string, ds geo.Dataset, incoming []geo.Feature, replace bool) (Result, error) {
	if name == "" {
		return Result{}, invalidf("collection name is required")
	}
	columns, err := encodeColumns(ds.Columns)
	if err != nil {
		return Result{}, err
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(ctx)

	var current int64
	err = tx.QueryRow(ctx, `select version from pgpq.collections where name = $1 for update`, name).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `insert into pgpq.collections (name, version, id_column, geometry, columns_json) values ($1, 0, $2, $3, $4)`,
			name, ds.IDColumn, ds.Geometry, columns); err != nil {
			return Result{}, err
		}
		current = 0
		replace = true
	} else if err != nil {
		return Result{}, err
	} else if !replace {
		var storedID, storedGeometry string
		var stored []byte
		if err := tx.QueryRow(ctx, `select id_column, geometry, columns_json from pgpq.collections where name = $1`, name).
			Scan(&storedID, &storedGeometry, &stored); err != nil {
			return Result{}, err
		}
		existingColumns, err := decodeColumns(stored)
		if err != nil {
			return Result{}, err
		}
		if storedID != ds.IDColumn || storedGeometry != ds.Geometry || !sameSchema(existingColumns, ds.Columns) {
			return Result{}, fmt.Errorf("%w: reload the collection to change its schema", ErrConflict)
		}
	}

	version := current + 1
	currentShards, err := m.readShards(ctx, tx, name, current)
	if err != nil {
		return Result{}, err
	}
	selected := map[int64]shardRecord{}
	var combined []geo.Feature
	if replace {
		combined = incoming
	} else {
		incomingIDs := make(map[string]struct{}, len(incoming))
		for _, feature := range incoming {
			incomingIDs[feature.ID] = struct{}{}
		}
		idShards, err := m.shardsForIDs(ctx, tx, name, incoming, currentShards)
		if err != nil {
			return Result{}, err
		}
		for _, shard := range currentShards {
			if _, ok := idShards[shard.ID]; ok {
				selected[shard.ID] = shard
			}
		}
		minKey, maxKey := featureRange(incoming)
		for _, shard := range currentShards {
			if shard.LastKey >= minKey && shard.FirstKey <= maxKey {
				selected[shard.ID] = shard
			}
		}
		for _, shard := range selected {
			old, err := loadShardDataset(ctx, tx, shard, ds.IDColumn)
			if err != nil {
				return Result{}, fmtShardError(shard.ID, err)
			}
			for _, feature := range old.Features {
				if _, replaced := incomingIDs[feature.ID]; replaced {
					continue
				}
				feature.Key = spatial.Key(feature.Bounds)
				combined = append(combined, feature)
			}
		}
		combined = append(combined, incoming...)
		slices.SortFunc(combined, func(a, b geo.Feature) int {
			if a.Key < b.Key {
				return -1
			}
			if a.Key > b.Key {
				return 1
			}
			if a.ID < b.ID {
				return -1
			}
			if a.ID > b.ID {
				return 1
			}
			return 0
		})
	}

	encoded, err := encodeShards(ds, combined, m.maxShardBytes)
	if err != nil {
		return Result{}, err
	}
	newShards := make([]placedShard, 0, len(encoded)+len(currentShards))
	for _, item := range encoded {
		shard, err := insertShard(ctx, tx, name, item.data)
		if err != nil {
			return Result{}, err
		}
		first, last := featureRange(item.features)
		if err := setShardRange(ctx, tx, shard, first, last, len(item.features)); err != nil {
			return Result{}, err
		}
		shard.FirstKey, shard.LastKey, shard.Rows = first, last, len(item.features)
		newShards = append(newShards, placedShard{record: shard, features: item.features})
	}
	for _, shard := range currentShards {
		if _, changing := selected[shard.ID]; changing && !replace {
			continue
		}
		if replace {
			continue
		}
		newShards = append(newShards, placedShard{record: shard})
	}
	slices.SortFunc(newShards, func(a, b placedShard) int {
		if a.record.FirstKey < b.record.FirstKey {
			return -1
		}
		if a.record.FirstKey > b.record.FirstKey {
			return 1
		}
		if a.record.ID < b.record.ID {
			return -1
		}
		if a.record.ID > b.record.ID {
			return 1
		}
		return 0
	})

	if replace {
		if _, err := tx.Exec(ctx, `delete from pgpq.ids where collection = $1`, name); err != nil {
			return Result{}, err
		}
	} else if len(selected) > 0 {
		if _, err := tx.Exec(ctx, `delete from pgpq.ids where collection = $1 and shard_id = any($2)`, name, shardIDs(mapShards(selected))); err != nil {
			return Result{}, err
		}
	}

	var totalRows int64
	var totalBytes int64
	var base int64 = 4
	for i := range newShards {
		newShards[i].record.Seq = i
		newShards[i].record.VirtualBase = base
		base += shardDataLength(newShards[i].record)
		if _, err := tx.Exec(ctx, `insert into pgpq.collection_shards (collection, version, seq, shard_id, virtual_base) values ($1, $2, $3, $4, $5)`,
			name, version, i, newShards[i].record.ID, newShards[i].record.VirtualBase); err != nil {
			return Result{}, err
		}
		totalRows += int64(newShards[i].record.Rows)
		totalBytes += newShards[i].record.Bytes
		if len(newShards[i].features) > 0 {
			if err := insertIDs(ctx, tx, name, newShards[i].record.ID, newShards[i].features); err != nil {
				return Result{}, err
			}
		}
	}

	vshards := make([]vparquet.Shard, len(newShards))
	for i, shard := range newShards {
		vshards[i] = vparquet.Shard{Reader: shardReader(tx, nil, shard.record.ID, shard.record.Bytes), Size: shard.record.Bytes}
	}
	layout, err := vparquet.Build(vshards)
	if err != nil {
		return Result{}, err
	}
	if layout.DataLength != base {
		return Result{}, fmt.Errorf("dataset: virtual data length %d, want %d", layout.DataLength, base)
	}
	etag := fmt.Sprintf("pgpq-%s-v%d", name, version)
	if _, err := tx.Exec(ctx, `insert into pgpq.virtual_files (collection, version, footer, footer_start, total_length, etag) values ($1, $2, $3, $4, $5, $6)`,
		name, version, layout.Footer, layout.DataLength, layout.TotalLength, etag); err != nil {
		return Result{}, err
	}
	if _, err := tx.Exec(ctx, `update pgpq.collections set version = $2, id_column = $3, geometry = $4, columns_json = $5, rows = $6, bytes = $7, updated = now() where name = $1`,
		name, version, ds.IDColumn, ds.Geometry, columns, totalRows, totalBytes); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return Result{Collection: name, Version: version, Features: len(incoming), Shards: len(newShards), Bytes: totalBytes, ETag: etag}, nil
}

func mapShards(in map[int64]shardRecord) []shardRecord {
	out := make([]shardRecord, 0, len(in))
	for _, shard := range in {
		out = append(out, shard)
	}
	return out
}

func (m *Manager) shardsForIDs(ctx context.Context, tx pgx.Tx, name string, features []geo.Feature, shards []shardRecord) (map[int64]shardRecord, error) {
	hashes := make([]int64, len(features))
	wanted := make(map[string]struct{}, len(features))
	for i, feature := range features {
		hashes[i] = idHash(feature.ID)
		wanted[feature.ID] = struct{}{}
	}
	rows, err := tx.Query(ctx, `select shard_id, id from pgpq.ids where collection = $1 and hash = any($2)`, name, hashes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]shardRecord{}
	for rows.Next() {
		var id int64
		var featureID string
		if err := rows.Scan(&id, &featureID); err != nil {
			return nil, err
		}
		if _, ok := wanted[featureID]; !ok {
			continue
		}
		if shard, ok := shardByID(shards, id); ok {
			out[id] = shard
		}
	}
	return out, rows.Err()
}

func insertIDs(ctx context.Context, tx pgx.Tx, collection string, shardID int64, features []geo.Feature) error {
	rows := make([][]any, len(features))
	for i, feature := range features {
		rows[i] = []any{collection, idHash(feature.ID), feature.ID, shardID}
	}
	if len(rows) == 0 {
		return nil
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"pgpq", "ids"}, []string{"collection", "hash", "id", "shard_id"}, pgx.CopyFromRows(rows))
	return err
}
