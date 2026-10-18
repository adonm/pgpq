create table attr_chunks (
	id bigint generated always as identity,
	collection text not null,
	feature_base bigint not null default 0,
	feature_count int not null,
	size_bytes int not null,
	checksum text,
	columns jsonb not null default '[]',
	data bytea storage external not null,
	primary key (collection, id)
) partition by list (collection);

create index if not exists attr_chunks_feature_base_idx on attr_chunks (collection, feature_base);
