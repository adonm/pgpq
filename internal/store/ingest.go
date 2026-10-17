package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Ingest struct {
	tx         pgx.Tx
	collection string
	staging    map[string]string
	virtual    map[int]*virtualTier
}

func (s *Store) BeginIngest(ctx context.Context, name string) (*Ingest, error) {
	if !collectionName.MatchString(name) {
		return nil, fmt.Errorf("invalid collection name %q", name)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`insert into collections (name) values ($1) on conflict (name) do nothing`, name); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	ing := &Ingest{tx: tx, collection: name, staging: map[string]string{}, virtual: map[int]*virtualTier{}}
	if err := ing.createStaging(ctx); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	return ing, nil
}

func (i *Ingest) createStaging(ctx context.Context) error {
	for _, parent := range chunkPartitions {
		i.staging[parent] = parent + "_" + i.collection + "_next"
	}
	chunks := pgx.Identifier{i.staging["chunks"]}.Sanitize()
	if _, err := i.tx.Exec(ctx, `create table `+chunks+` (
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
		cover int8multirange not null,
		checksum text,
		columns jsonb not null default '[]',
		data bytea not null,
		primary key (collection, id)
	)`); err != nil {
		return err
	}
	text := pgx.Identifier{i.staging["chunk_text"]}.Sanitize()
	if _, err := i.tx.Exec(ctx, `create table `+text+` (
		collection text not null,
		chunk_id bigint not null,
		feature_ord int not null,
		tsv tsvector not null,
		primary key (collection, chunk_id, feature_ord)
	)`); err != nil {
		return err
	}
	props := pgx.Identifier{i.staging["chunk_props"]}.Sanitize()
	if _, err := i.tx.Exec(ctx, `create table `+props+` (
		collection text not null,
		chunk_id bigint not null,
		feature_ord int not null,
		props jsonb not null,
		primary key (collection, chunk_id, feature_ord)
	)`); err != nil {
		return err
	}
	return nil
}

func (i *Ingest) createIndexes(ctx context.Context) error {
	chunks := pgx.Identifier{i.staging["chunks"]}.Sanitize()
	if _, err := i.tx.Exec(ctx, `create index on `+chunks+` using gist (cover)`); err != nil {
		return err
	}
	if _, err := i.tx.Exec(ctx, `create index on `+chunks+` (tier, id)`); err != nil {
		return err
	}
	text := pgx.Identifier{i.staging["chunk_text"]}.Sanitize()
	if _, err := i.tx.Exec(ctx, `create index on `+text+` using gin (tsv)`); err != nil {
		return err
	}
	props := pgx.Identifier{i.staging["chunk_props"]}.Sanitize()
	_, err := i.tx.Exec(ctx, `create index on `+props+` using gin (props jsonb_path_ops)`)
	return err
}

func (i *Ingest) swap(ctx context.Context) error {
	view := pgx.Identifier{"layers", i.collection}.Sanitize()
	if _, err := i.tx.Exec(ctx, `drop view if exists `+view); err != nil {
		return err
	}
	for j := len(chunkPartitions) - 1; j >= 0; j-- {
		old := pgx.Identifier{chunkPartitions[j] + "_" + i.collection}.Sanitize()
		if _, err := i.tx.Exec(ctx, `drop table if exists `+old); err != nil {
			return err
		}
	}
	for _, parent := range chunkPartitions {
		staging := pgx.Identifier{i.staging[parent]}.Sanitize()
		final := pgx.Identifier{parent + "_" + i.collection}.Sanitize()
		if _, err := i.tx.Exec(ctx, `alter table `+staging+` rename to `+final); err != nil {
			return err
		}
		if _, err := i.tx.Exec(ctx, `alter table `+parent+` attach partition `+final+` for values in ('`+i.collection+`')`); err != nil {
			return err
		}
		if err := i.renameIndexes(ctx, parent+"_"+i.collection); err != nil {
			return err
		}
		if _, err := i.tx.Exec(ctx, `analyze `+final); err != nil {
			return err
		}
	}
	return nil
}

func (i *Ingest) renameIndexes(ctx context.Context, table string) error {
	rows, err := i.tx.Query(ctx, `select indexname from pg_indexes where tablename = $1`, table)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range names {
		renamed := strings.Replace(name, "_next_", "_", 1)
		if renamed == name {
			continue
		}
		if _, err := i.tx.Exec(ctx,
			`alter index `+pgx.Identifier{name}.Sanitize()+` rename to `+pgx.Identifier{renamed}.Sanitize()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DropCollection(ctx context.Context, name string) error {
	if !collectionName.MatchString(name) {
		return fmt.Errorf("invalid collection name %q", name)
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx,
		`select exists (select 1 from collections where name = $1)`, name).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	view := pgx.Identifier{"layers", name}.Sanitize()
	if _, err := tx.Exec(ctx, `drop view if exists `+view); err != nil {
		return err
	}
	for j := len(chunkPartitions) - 1; j >= 0; j-- {
		partition := pgx.Identifier{chunkPartitions[j] + "_" + name}.Sanitize()
		if _, err := tx.Exec(ctx, `drop table if exists `+partition); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `delete from collections where name = $1`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from virtual_file where collection = $1`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from virtual_segment where collection = $1`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) PutMeta(ctx context.Context, name string, doc []byte, meta CollectionMeta) error {
	if !collectionName.MatchString(name) {
		return fmt.Errorf("invalid collection name %q", name)
	}
	keywords := any(nil)
	if meta.Keywords != nil {
		keywords = *meta.Keywords
	}
	_, err := s.Pool.Exec(ctx, `
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
	if err := i.createIndexes(ctx); err != nil {
		return err
	}
	if err := i.swap(ctx); err != nil {
		return err
	}
	if err := refreshStats(ctx, i.tx, i.collection); err != nil {
		return err
	}
	if _, err := i.tx.Exec(ctx, `create schema if not exists layers`); err != nil {
		return err
	}
	view := pgx.Identifier{"layers", i.collection}.Sanitize()
	if _, err := i.tx.Exec(ctx,
		`create or replace view `+view+` as select * from chunks where collection = '`+i.collection+`' and tier = 0`); err != nil {
		return err
	}
	if err := i.buildVirtual(ctx); err != nil {
		return err
	}
	return i.tx.Commit(ctx)
}

func (i *Ingest) Rollback(ctx context.Context) {
	i.tx.Rollback(ctx)
}

func (i *Ingest) PutChunk(ctx context.Context, payload Payload) (int64, error) {
	id, err := putChunk(ctx, i.tx, i.staging["chunks"], i.collection, payload)
	if err != nil {
		return id, err
	}
	tier := i.virtual[payload.Tier]
	if tier == nil {
		tier = &virtualTier{header: payload.HeaderBlob}
		i.virtual[payload.Tier] = tier
	}
	tier.leaves = append(tier.leaves, payload.Leaves)
	tier.lengths = append(tier.lengths, int64(payload.FeatureBytes))
	tier.count += uint64(payload.Count)
	return id, nil
}

func (i *Ingest) PutChunkText(ctx context.Context, chunkID int64, docs []string) error {
	return putChunkText(ctx, i.tx, i.staging["chunk_text"], i.collection, chunkID, docs)
}

func (i *Ingest) PutChunkProps(ctx context.Context, chunkID int64, docs []string) error {
	return putChunkProps(ctx, i.tx, i.staging["chunk_props"], i.collection, chunkID, docs)
}

func (i *Ingest) Chunks(ctx context.Context, bbox *[4]float64, limit, tier int) ([]ChunkRow, error) {
	return chunks(ctx, i.tx, i.staging["chunks"], i.collection, bbox, limit, tier)
}

func (i *Ingest) DeleteChunksAboveTier(ctx context.Context, tier int) error {
	return deleteChunksAboveTier(ctx, i.tx, i.staging["chunks"], tier)
}

func (i *Ingest) SetCatalog(ctx context.Context, sortable []string, idColumn, timeColumn string, timeMin, timeMax *time.Time) error {
	_, err := i.tx.Exec(ctx, `
		update collections set kind = 'native', version = version + 1, updated_at = now(), sortable = coalesce($2::text[], '{}'),
			id_column = $3, time_column = $4, time_min = $5, time_max = $6
		where name = $1`, i.collection, sortable, nullable(idColumn), nullable(timeColumn), timeMin, timeMax)
	return err
}

func SortExpr(name, cast string) string {
	key := strings.ReplaceAll(name, "'", "''")
	expr := `props->>'` + key + `'`
	switch cast {
	case "":
		return expr
	case "numeric", "boolean", "timestamptz":
		return `(` + expr + `)::` + cast
	default:
		return ""
	}
}

func (i *Ingest) CreateSortIndexes(ctx context.Context, cols []SortColumn) error {
	for idx, col := range cols {
		expr := SortExpr(col.Name, col.Cast)
		if expr == "" {
			return fmt.Errorf("sortable: unsupported cast %q", col.Cast)
		}
		name := pgx.Identifier{fmt.Sprintf("chunk_props_%s_sort_%d_idx", i.collection, idx)}.Sanitize()
		table := pgx.Identifier{i.staging["chunk_props"]}.Sanitize()
		if _, err := i.tx.Exec(ctx, `create index `+name+` on `+table+` ((`+expr+`))`); err != nil {
			return err
		}
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func putChunk(ctx context.Context, conn db, table, collection string, payload Payload) (int64, error) {
	var id int64
	err := conn.QueryRow(ctx, `
		insert into `+table+` (id, collection, tier, feature_count, size_bytes, feature_bytes, minx, miny, maxx, maxy, cover, checksum, columns, data)
		values (nextval(pg_get_serial_sequence('chunks', 'id')), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10::int8multirange, $11, $12::jsonb, $13)
		returning id`,
		collection, payload.Tier, payload.Count, payload.Size, payload.FeatureBytes,
		payload.MinX, payload.MinY, payload.MaxX, payload.MaxY, payload.Cover,
		payload.Checksum, payload.Columns, payload.Data).Scan(&id)
	return id, err
}

func putChunkText(ctx context.Context, conn db, table, collection string, chunkID int64, docs []string) error {
	if len(docs) == 0 {
		return nil
	}
	ords := make([]int32, len(docs))
	for i := range ords {
		ords[i] = int32(i)
	}
	_, err := conn.Exec(ctx, `
		insert into `+table+` (collection, chunk_id, feature_ord, tsv)
		select $1, $2, ord, to_tsvector('simple', doc)
		from unnest($3::int[], $4::text[]) as input(ord, doc)
		where doc <> ''`, collection, chunkID, ords, docs)
	return err
}

func putChunkProps(ctx context.Context, conn db, table, collection string, chunkID int64, docs []string) error {
	if len(docs) == 0 {
		return nil
	}
	ords := make([]int32, len(docs))
	for i := range ords {
		ords[i] = int32(i)
	}
	_, err := conn.Exec(ctx, `
		insert into `+table+` (collection, chunk_id, feature_ord, props)
		select $1, $2, ord, doc::jsonb
		from unnest($3::int[], $4::text[]) as input(ord, doc)
		where doc <> '' and doc <> '{}' and doc <> 'null'`, collection, chunkID, ords, docs)
	return err
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
