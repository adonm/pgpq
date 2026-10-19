create schema if not exists pfgb;

create table pfgb.collections (
	name text primary key,
	uid bigint generated always as identity,
	current_snapshot bigint not null default 0,
	id_column text,
	columns_json jsonb not null default '[]',
	created timestamptz not null default now()
);

create table pfgb.snapshots (
	collection text not null references pfgb.collections (name) on delete cascade,
	id bigint not null,
	created timestamptz not null default now(),
	primary key (collection, id)
);

create table pfgb.partitions (
	collection text not null references pfgb.collections (name) on delete cascade,
	cell bigint not null,
	changed bigint not null,
	primary key (collection, cell)
);

create table pfgb.files (
	file_id bigint generated always as identity primary key,
	collection text not null,
	kind text not null,
	bytes bigint not null,
	published boolean not null default false,
	created timestamptz not null default now()
);

create table pfgb.blocks (
	file_id bigint not null,
	n int not null,
	data bytea storage plain not null,
	primary key (file_id, n)
);

create table pfgb.chunks (
	chunk_id bigint generated always as identity primary key,
	collection text not null references pfgb.collections (name) on delete cascade,
	cell bigint not null,
	seq int not null,
	first_key bigint not null,
	first_id text not null,
	last_key bigint not null,
	last_id text not null,
	minx double precision not null,
	miny double precision not null,
	maxx double precision not null,
	maxy double precision not null,
	features int not null,
	file_id bigint not null,
	begin_snapshot bigint not null,
	end_snapshot bigint
);

create index chunks_snapshot_idx on pfgb.chunks (collection, begin_snapshot);

create index chunks_live_cell_idx on pfgb.chunks (collection, cell) where end_snapshot is null;
