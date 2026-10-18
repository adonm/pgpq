alter table virtual_file rename to virtual_file_legacy;
alter table virtual_segment rename to virtual_segment_legacy;

create table virtual_file (
	collection text not null,
	version bigint not null,
	tier smallint not null,
	header_bytes int not null,
	index_bytes bigint not null,
	feature_bytes bigint not null,
	chunks int not null,
	features bigint not null default 0,
	primary key (collection, version, tier)
) partition by list (collection);

create table virtual_segment (
	collection text not null,
	version bigint not null,
	tier smallint not null,
	kind smallint not null,
	ordinal int not null,
	data bytea not null,
	primary key (collection, version, tier, kind, ordinal)
) partition by list (collection);

alter table virtual_segment alter column data set storage external;

create table virtual_file_default partition of virtual_file default;
create table virtual_segment_default partition of virtual_segment default;

alter table virtual_segment_default alter column data set storage external;

insert into virtual_file (collection, version, tier, header_bytes, index_bytes, feature_bytes, chunks, features)
	select collection, version, tier, header_bytes, index_bytes, feature_bytes, chunks, features from virtual_file_legacy;

insert into virtual_segment (collection, version, tier, kind, ordinal, data)
	select collection, version, tier, kind, ordinal, data from virtual_segment_legacy;

drop table virtual_file_legacy;
drop table virtual_segment_legacy;

alter index virtual_file_pkey1 rename to virtual_file_pkey;
alter index virtual_segment_pkey1 rename to virtual_segment_pkey;
