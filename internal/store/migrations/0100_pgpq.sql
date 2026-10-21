create schema if not exists pgpq;

create sequence if not exists pgpq.cell_ids;

create table if not exists pgpq.collections (
	name text primary key,
	uid bigint generated always as identity unique,
	version bigint not null default 0,
	id_column text not null,
	geometry text not null,
	columns_json jsonb not null default '[]',
	created timestamptz not null default now()
);

create table if not exists pgpq.cells (
	collection_uid bigint not null references pgpq.collections (uid) on delete cascade,
	level smallint not null,
	cell bigint not null,
	cell_id bigint not null default nextval('pgpq.cell_ids') unique,
	features int not null,
	bytes bigint not null,
	minx double precision not null,
	miny double precision not null,
	maxx double precision not null,
	maxy double precision not null,
	first_key bigint not null,
	first_id text not null,
	last_key bigint not null,
	last_id text not null,
	zone text not null,
	primary key (collection_uid, level, cell)
);

create table if not exists pgpq.cell_chunks (
	cell_id bigint not null references pgpq.cells (cell_id) on delete cascade,
	no int not null,
	data bytea not null,
	primary key (cell_id, no),
	constraint cell_chunk_shape check (no >= 0 and octet_length(data) between 1 and 8120)
) with (toast_tuple_target = 8160);

alter table pgpq.cell_chunks alter column data set storage external;
alter table pgpq.cell_chunks set (
	autovacuum_vacuum_scale_factor = 0.01,
	autovacuum_analyze_scale_factor = 0.02,
	autovacuum_vacuum_threshold = 1000
);

create table if not exists pgpq.ids (
	collection_uid bigint not null references pgpq.collections (uid) on delete cascade,
	hash bigint not null,
	cell bigint not null,
	row int not null,
	primary key (collection_uid, hash, cell, row)
);
create index if not exists pgpq_ids_cell_idx on pgpq.ids (collection_uid, cell);

delete from schema_migrations where version <> '0100_pgpq.sql';
