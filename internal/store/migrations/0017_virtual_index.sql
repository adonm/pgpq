alter table chunks add column if not exists feature_bytes int not null default 0;

create table if not exists virtual_file (
	collection text not null,
	version bigint not null,
	tier smallint not null,
	header_bytes int not null,
	index_bytes bigint not null,
	feature_bytes bigint not null,
	chunks int not null,
	primary key (collection, version, tier)
);

create table if not exists virtual_segment (
	collection text not null,
	version bigint not null,
	tier smallint not null,
	kind smallint not null,
	ordinal int not null,
	data bytea not null,
	primary key (collection, version, tier, kind, ordinal)
);
