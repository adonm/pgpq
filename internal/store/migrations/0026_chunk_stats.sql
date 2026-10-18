create table chunk_stats (
	collection text not null,
	chunk_id bigint not null,
	"column" text not null,
	min double precision,
	max double precision,
	nulls bigint not null default 0,
	bloom bytea,
	primary key (collection, chunk_id, "column")
) partition by list (collection);

alter table collections add column if not exists stats_version bigint not null default 0;

create or replace function bloom_has(bits bytea, h1 bigint, h2 bigint) returns boolean
language sql immutable parallel safe as $$
	select bool_and(get_bit(bits, ((h1 + i * h2) % (length(bits)::bigint * 8))::int) = 1)
	from generate_series(0, 6) i
$$;
