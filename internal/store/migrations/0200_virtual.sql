drop table if exists pgpq.ids cascade;
drop table if exists pgpq.cell_chunks cascade;
drop table if exists pgpq.cells cascade;
drop table if exists pgpq.collections cascade;
drop sequence if exists pgpq.cell_ids;

create table pgpq.collections (
	name text primary key,
	version bigint not null default 0,
	id_column text not null,
	geometry text not null,
	columns_json jsonb not null,
	rows bigint not null default 0,
	bytes bigint not null default 0,
	updated timestamptz not null default now()
);

create table pgpq.shards (
	shard_id bigint generated always as identity primary key,
	collection text not null references pgpq.collections (name) on delete cascade,
	bytes bigint not null,
	data_start bigint not null,
	data_end bigint not null,
	rows int not null,
	first_key bigint not null,
	last_key bigint not null,
	created timestamptz not null default now()
);

create table pgpq.collection_shards (
	collection text not null references pgpq.collections (name) on delete cascade,
	version bigint not null,
	seq int not null,
	shard_id bigint not null references pgpq.shards (shard_id) on delete cascade,
	virtual_base bigint not null,
	primary key (collection, version, seq)
);

create table pgpq.shard_chunks (
	shard_id bigint not null references pgpq.shards (shard_id) on delete cascade,
	no int not null,
	data bytea not null,
	primary key (shard_id, no),
	constraint shard_chunk_shape check (no >= 0 and octet_length(data) between 1 and 8120)
) with (toast_tuple_target = 8160);

alter table pgpq.shard_chunks alter column data set storage external;

create table pgpq.virtual_files (
	collection text not null references pgpq.collections (name) on delete cascade,
	version bigint not null,
	footer bytea not null,
	footer_start bigint not null,
	total_length bigint not null,
	etag text not null,
	created timestamptz not null default now(),
	primary key (collection, version)
);

create table pgpq.ids (
	collection text not null references pgpq.collections (name) on delete cascade,
	hash bigint not null,
	id text not null,
	shard_id bigint not null references pgpq.shards (shard_id) on delete cascade,
	primary key (collection, hash, id)
);

create index pgpq_ids_shard_idx on pgpq.ids (shard_id);
