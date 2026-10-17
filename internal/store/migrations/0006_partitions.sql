drop extension if exists pg_trgm;

alter index chunks_pkey rename to chunks_legacy_pkey;
alter index chunk_text_pkey rename to chunk_text_legacy_pkey;
alter index chunk_props_pkey rename to chunk_props_legacy_pkey;
drop index chunks_cover_idx;
drop index chunks_collection_idx;
drop index chunks_tier_idx;
drop index chunk_text_tsv_idx;
drop index chunk_props_gin_idx;

alter sequence chunks_id_seq rename to chunks_legacy_id_seq;

alter table chunks rename to chunks_legacy;
alter table chunk_text rename to chunk_text_legacy;
alter table chunk_props rename to chunk_props_legacy;

alter table collections add column if not exists kind text not null default 'native',
	add column if not exists feature_count bigint not null default 0,
	add column if not exists chunk_count bigint not null default 0,
	add column if not exists minx double precision,
	add column if not exists miny double precision,
	add column if not exists maxx double precision,
	add column if not exists maxy double precision,
	add column if not exists time_min timestamptz,
	add column if not exists time_max timestamptz,
	add column if not exists sortable text[] not null default '{}',
	add column if not exists members jsonb;

create table chunks (
	id bigint generated always as identity,
	collection text not null references collections(name) on delete cascade,
	tier smallint not null default 0,
	feature_count int not null,
	size_bytes int not null,
	minx double precision not null,
	miny double precision not null,
	maxx double precision not null,
	maxy double precision not null,
	cover int8multirange not null,
	checksum text,
	columns jsonb not null default '[]',
	data bytea not null,
	primary key (collection, id)
) partition by list (collection);

create table chunk_text (
	collection text not null,
	chunk_id bigint not null,
	feature_ord int not null,
	tsv tsvector not null,
	primary key (collection, chunk_id, feature_ord),
	foreign key (collection, chunk_id) references chunks (collection, id) on delete cascade
) partition by list (collection);

create table chunk_props (
	collection text not null,
	chunk_id bigint not null,
	feature_ord int not null,
	props jsonb not null,
	primary key (collection, chunk_id, feature_ord),
	foreign key (collection, chunk_id) references chunks (collection, id) on delete cascade
) partition by list (collection);

do $$
declare c text;
begin
	for c in select name from collections loop
		execute format('create table %I partition of chunks for values in (%L)', 'chunks_' || c, c);
		execute format('create table %I partition of chunk_text for values in (%L)', 'chunk_text_' || c, c);
		execute format('create table %I partition of chunk_props for values in (%L)', 'chunk_props_' || c, c);
	end loop;
end $$;

insert into chunks (id, collection, tier, feature_count, size_bytes, minx, miny, maxx, maxy, cover, checksum, columns, data)
overriding system value
select id, collection, tier, feature_count, size_bytes, minx, miny, maxx, maxy, cover, checksum, columns, data
from chunks_legacy;

insert into chunk_text (collection, chunk_id, feature_ord, tsv)
select c.collection, t.chunk_id, t.feature_ord, t.tsv
from chunk_text_legacy t join chunks_legacy c on c.id = t.chunk_id;

insert into chunk_props (collection, chunk_id, feature_ord, props)
select c.collection, p.chunk_id, p.feature_ord, p.props
from chunk_props_legacy p join chunks_legacy c on c.id = p.chunk_id;

select setval(pg_get_serial_sequence('chunks', 'id'), coalesce((select max(id) from chunks), 0) + 1, false);

drop table chunk_text_legacy;
drop table chunk_props_legacy;
drop table chunks_legacy;

create index chunks_cover_idx on chunks using gist (cover);
create index chunks_tier_idx on chunks (collection, tier, id);
create index chunk_text_tsv_idx on chunk_text using gin (tsv);
create index chunk_props_gin_idx on chunk_props using gin (props jsonb_path_ops);

update collections c set
	chunk_count = s.chunks,
	feature_count = s.features,
	minx = s.minx, miny = s.miny, maxx = s.maxx, maxy = s.maxy
from (
	select collection, count(*) chunks, coalesce(sum(feature_count), 0) features,
	       min(minx) minx, min(miny) miny, max(maxx) maxx, max(maxy) maxy
	from chunks where tier = 0 group by collection
) s
where c.name = s.collection;

create schema if not exists layers;

do $$
declare c text;
begin
	for c in select name from collections loop
		execute format('create view layers.%I as select * from chunks where collection = %L and tier = 0', c, c);
	end loop;
end $$;
