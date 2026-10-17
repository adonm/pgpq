alter table collections add column if not exists version bigint not null default 1;

create table if not exists tile_cache (
	collection text not null,
	version text not null,
	tier smallint not null,
	z smallint not null,
	x int not null,
	y int not null,
	filter_key text not null,
	etag text not null,
	data bytea not null,
	created_at timestamptz not null default now(),
	primary key (collection, version, tier, z, x, y, filter_key)
);

create index if not exists tile_cache_age_idx on tile_cache (created_at);
