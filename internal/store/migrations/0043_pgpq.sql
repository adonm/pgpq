create schema if not exists pgpq;

create table pgpq.collections (
	name text primary key,
	uid bigint generated always as identity unique,
	version bigint not null default 0,
	id_column text not null,
	geometry text not null,
	columns_json jsonb not null default '[]',
	created timestamptz not null default now()
);

create table pgpq.cells (
	collection_uid bigint not null references pgpq.collections (uid) on delete cascade,
	level smallint not null,
	cell bigint not null,
	features int not null,
	minx double precision not null,
	miny double precision not null,
	maxx double precision not null,
	maxy double precision not null,
	first_key bigint not null,
	first_id text not null,
	last_key bigint not null,
	last_id text not null,
	zone text not null,
	data bytea not null,
	primary key (collection_uid, level, cell)
);
alter table pgpq.cells alter column data set storage external;

create table pgpq.ids (
	collection_uid bigint not null references pgpq.collections (uid) on delete cascade,
	hash bigint not null,
	cell bigint not null,
	row int not null,
	primary key (collection_uid, hash, cell, row)
);
create index pgpq_ids_cell_idx on pgpq.ids (collection_uid, cell);
