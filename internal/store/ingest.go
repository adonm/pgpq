package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/flatgeobuf/flatgeobuf/src/go/flattypes"

	"github.com/adonm/postfgb/internal/fgb"
	"github.com/adonm/postfgb/internal/stats"
)

var maintenanceWorkers = strconv.Itoa(min(runtime.NumCPU(), 8))

type Ingest struct {
	store      *Store
	tx         pgx.Tx
	collection string
	virtual    map[int]*virtualTier
}

type partition struct {
	parent  string
	table   string
	indexes []string
}

var partitions = []partition{
	{
		parent: "chunks",
		table: `create table %[1]s (
	id bigint not null,
	collection text not null,
	tier smallint not null default 0,
	feature_count int not null,
	size_bytes int not null,
	feature_bytes int not null default 0,
	minx double precision not null,
	miny double precision not null,
	maxx double precision not null,
	maxy double precision not null,
	checksum text,
	columns jsonb not null default '[]',
	data bytea not null,
	primary key (collection, id)
)`,
		indexes: []string{
			`create index on %[1]s using gist (box(point(minx, miny), point(maxx, maxy)))`,
			`create index on %[1]s (tier, id)`,
		},
	},
	{
		parent: "chunk_stats",
		table: `create table %[1]s (
	collection text not null,
	chunk_id bigint not null,
	"column" text not null,
	min double precision,
	max double precision,
	nulls bigint not null default 0,
	bloom bytea,
	primary key (collection, chunk_id, "column")
)`,
	},
}

func partitionName(parent, collection string) string {
	sum := sha256.Sum256([]byte(collection))
	return parent + "_" + hex.EncodeToString(sum[:16])
}

var virtualPartitions = []partition{
	{
		parent: "virtual_file",
		table: `create table %[1]s (
	collection text not null,
	version bigint not null,
	tier smallint not null,
	header_bytes int not null,
	index_bytes bigint not null,
	feature_bytes bigint not null,
	chunks int not null,
	features bigint not null default 0,
	primary key (collection, version, tier)
)`,
	},
	{
		parent: "virtual_segment",
		table: `create table %[1]s (
	collection text not null,
	version bigint not null,
	tier smallint not null,
	kind smallint not null,
	ordinal int not null,
	data bytea storage external not null,
	primary key (collection, version, tier, kind, ordinal)
)`,
	},
}

const nodeTableDDL = `create table %[1]s (
	tier smallint not null,
	level int not null,
	ordinal bigint not null,
	data bytea storage external not null,
	primary key (tier, level, ordinal)
)`

func stagedName(parent, collection string) string {
	return pgx.Identifier{"staging", partitionName(parent, collection)}.Sanitize()
}

func stageVirtual(ctx context.Context, conn db, collection string) error {
	for _, p := range virtualPartitions {
		if _, err := conn.Exec(ctx, fmt.Sprintf(p.table, stagedName(p.parent, collection))); err != nil {
			return err
		}
	}
	_, err := conn.Exec(ctx, fmt.Sprintf(nodeTableDDL, stagedName("virtual_node", collection)))
	return err
}

func swapVirtual(ctx context.Context, tx pgx.Tx, collection string) error {
	for _, p := range virtualPartitions {
		final := ident(partitionName(p.parent, collection))
		if _, err := tx.Exec(ctx, `drop table if exists `+final); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `delete from `+ident(p.parent)+` where collection = $1`, collection); err != nil {
			return err
		}
		for _, stmt := range []string{
			`alter table ` + stagedName(p.parent, collection) + ` set schema public`,
			`alter table ` + ident(p.parent) + ` attach partition ` + final + ` for values in ('` + collection + `')`,
		} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
	}
	return nil
}

const partitionLockKey int64 = 0x50464c4b

func (i *Ingest) staged(parent string) string {
	return stagedName(parent, i.collection)
}

func (s *Store) BeginIngest(ctx context.Context, name string) (*Ingest, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid collection name %q", name)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	ing := &Ingest{store: s, tx: tx, collection: name, virtual: map[int]*virtualTier{}}
	if err := ing.stage(ctx); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	return ing, nil
}

func (i *Ingest) stage(ctx context.Context) error {
	if _, err := i.tx.Exec(ctx, `insert into collections (name) values ($1) on conflict (name) do nothing`, i.collection); err != nil {
		return err
	}
	for _, p := range partitions {
		if _, err := i.tx.Exec(ctx, fmt.Sprintf(p.table, i.staged(p.parent))); err != nil {
			return err
		}
	}
	return stageVirtual(ctx, i.tx, i.collection)
}

func (i *Ingest) createIndexes(ctx context.Context) error {
	for _, p := range partitions {
		for _, index := range p.indexes {
			if _, err := i.tx.Exec(ctx, fmt.Sprintf(index, i.staged(p.parent))); err != nil {
				return err
			}
		}
	}
	return nil
}

func (i *Ingest) swap(ctx context.Context) error {
	if _, err := i.tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, partitionLockKey); err != nil {
		return err
	}
	if err := dropPartitions(ctx, i.tx, i.collection); err != nil {
		return err
	}
	for _, p := range partitions {
		final := ident(partitionName(p.parent, i.collection))
		for _, stmt := range []string{
			`alter table ` + i.staged(p.parent) + ` set schema public`,
			`alter table ` + ident(p.parent) + ` attach partition ` + final + ` for values in ('` + i.collection + `')`,
			`analyze ` + final,
		} {
			if _, err := i.tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
	}
	if err := swapVirtual(ctx, i.tx, i.collection); err != nil {
		return err
	}
	_, err := i.tx.Exec(ctx, `update collections set stats_version = version where name = $1`, i.collection)
	return err
}

func dropPartitions(ctx context.Context, conn db, collection string) error {
	for _, p := range partitions {
		if _, err := conn.Exec(ctx, `drop table if exists `+ident(partitionName(p.parent, collection))); err != nil {
			return err
		}
	}
	for _, p := range virtualPartitions {
		if _, err := conn.Exec(ctx, `drop table if exists `+ident(partitionName(p.parent, collection))); err != nil {
			return err
		}
	}
	return nil
}

func deleteCollectionRows(ctx context.Context, conn db, name string) error {
	if err := dropPartitions(ctx, conn, name); err != nil {
		return err
	}
	for _, query := range []string{
		`delete from collections where name = $1`,
		`delete from virtual_file where collection = $1`,
		`delete from virtual_segment where collection = $1`,
	} {
		if _, err := conn.Exec(ctx, query, name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DropCollection(ctx context.Context, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid collection name %q", name)
	}
	if _, err := s.Collection(ctx, name); err != nil {
		return err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := dropPartitions(ctx, tx, name); err != nil {
		return err
	}
	if err := deleteCollectionRows(ctx, tx, name); err != nil {
		return err
	}
	if err := s.rebuildMemberViews(ctx, tx, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) PutMeta(ctx context.Context, name string, doc []byte, meta CollectionMeta) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid collection name %q", name)
	}
	return putMeta(ctx, s.Pool, name, doc, meta)
}

func putMeta(ctx context.Context, conn db, name string, doc []byte, meta CollectionMeta) error {
	keywords := any(nil)
	if meta.Keywords != nil {
		keywords = *meta.Keywords
	}
	_, err := conn.Exec(ctx, `
		insert into collections (name, stac, title, description, license, keywords)
		values ($1, $2::jsonb, $3, $4, $5, coalesce($6::text[], '{}'))
		on conflict (name) do update set
			updated_at = now(),
			stac = excluded.stac,
			title = coalesce(excluded.title, collections.title),
			description = coalesce(excluded.description, collections.description),
			license = coalesce(excluded.license, collections.license),
			keywords = case when $6::text[] is null then collections.keywords else excluded.keywords end`,
		name, doc, meta.Title, meta.Description, meta.License, keywords)
	return err
}

func (i *Ingest) Commit(ctx context.Context) error {
	if _, err := i.tx.Exec(ctx, `set local lock_timeout = '30s'`); err != nil {
		return err
	}
	if _, err := i.tx.Exec(ctx, `select set_config('max_parallel_maintenance_workers', $1, true)`, maintenanceWorkers); err != nil {
		return err
	}
	start := time.Now()
	if err := i.createIndexes(ctx); err != nil {
		return err
	}
	indexes := time.Since(start)
	start = time.Now()
	if err := i.buildVirtual(ctx); err != nil {
		return err
	}
	tree := time.Since(start)
	start = time.Now()
	if err := i.swap(ctx); err != nil {
		return err
	}
	swapped := time.Since(start)
	start = time.Now()
	if err := refreshStats(ctx, i.tx, i.collection); err != nil {
		return err
	}
	stats := time.Since(start)
	start = time.Now()
	if err := i.store.rebuildMemberViews(ctx, i.tx, i.collection); err != nil {
		return err
	}
	views := time.Since(start)
	slog.Info("ingest commit", "collection", i.collection, "indexes", indexes,
		"tree", tree, "swap", swapped, "stats", stats, "views", views)
	return i.tx.Commit(ctx)
}

func (i *Ingest) Rollback(ctx context.Context) {
	i.tx.Rollback(ctx)
}

func (i *Ingest) PutChunk(ctx context.Context, payload Payload) (int64, error) {
	var id int64
	err := i.tx.QueryRow(ctx, `
		insert into `+i.staged("chunks")+` (id, collection, tier, feature_count, size_bytes, feature_bytes, minx, miny, maxx, maxy, checksum, columns, data)
		values (nextval(pg_get_serial_sequence('chunks', 'id')), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12)
		returning id`,
		i.collection, payload.Tier, payload.Count, payload.Size, payload.FeatureBytes,
		payload.MinX, payload.MinY, payload.MaxX, payload.MaxY,
		payload.Checksum, payload.Columns, payload.Data).Scan(&id)
	if err != nil {
		return id, err
	}
	geometry := fgb.GeometryOf(payload.HeaderBlob)
	tier := i.virtual[payload.Tier]
	if tier == nil {
		tier = &virtualTier{
			header:   payload.HeaderBlob,
			geometry: geometry,
			sink:     newNodeSink(i.tx, i.staged("virtual_node"), payload.Tier, 0),
		}
		i.virtual[payload.Tier] = tier
	} else if geometry != tier.geometry {
		tier.geometry = flattypes.GeometryTypeUnknown
	}
	nodes := make([]byte, 0, len(payload.Leaves)*fgb.NodeBytes)
	for _, leaf := range payload.Leaves {
		nodes = fgb.AppendNode(nodes, leaf.BBox, uint64(tier.base)+leaf.Offset)
	}
	if err := tier.sink.write(ctx, nodes); err != nil {
		return id, err
	}
	tier.features += uint64(payload.Count)
	tier.featureBytes += int64(payload.FeatureBytes)
	tier.chunks++
	tier.base += int64(payload.FeatureBytes)
	return id, nil
}

func (i *Ingest) PutChunkStats(ctx context.Context, chunkID int64, rows []stats.Row) error {
	if len(rows) == 0 {
		return nil
	}
	names := make([]string, len(rows))
	mins := make([]*float64, len(rows))
	maxs := make([]*float64, len(rows))
	nulls := make([]int64, len(rows))
	blooms := make([][]byte, len(rows))
	for j, row := range rows {
		names[j] = row.Column
		mins[j] = row.Min
		maxs[j] = row.Max
		nulls[j] = row.Nulls
		blooms[j] = row.Bloom
	}
	_, err := i.tx.Exec(ctx, `
		insert into `+i.staged("chunk_stats")+` (collection, chunk_id, "column", min, max, nulls, bloom)
		select $1, $2, input.name, input.min, input.max, input.nulls, input.bloom
		from unnest($3::text[], $4::float8[], $5::float8[], $6::bigint[], $7::bytea[])
			as input(name, min, max, nulls, bloom)`,
		i.collection, chunkID, names, mins, maxs, nulls, blooms)
	return err
}

func (i *Ingest) SetCatalog(ctx context.Context, idColumn, timeColumn string, timeMin, timeMax *time.Time) error {
	_, err := i.tx.Exec(ctx, `
		update collections set kind = 'native', version = version + 1, updated_at = now(),
			id_column = $2, time_column = $3, time_min = $4, time_max = $5
		where name = $1`, i.collection, nullable(idColumn), nullable(timeColumn), timeMin, timeMax)
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func refreshStats(ctx context.Context, conn db, name string) error {
	_, err := conn.Exec(ctx, `
		update collections set
			chunk_count = s.chunks,
			feature_count = s.features,
			minx = s.minx, miny = s.miny, maxx = s.maxx, maxy = s.maxy
		from (
			select count(*) chunks, coalesce(sum(feature_count), 0) features,
			       min(minx) minx, min(miny) miny, max(maxx) maxx, max(maxy) maxy
			from chunks where collection = $1 and tier = 0
		) s
		where collections.name = $1`, name)
	return err
}
