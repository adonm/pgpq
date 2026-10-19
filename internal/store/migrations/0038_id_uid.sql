drop table if exists pfgb.ids;
create unique index if not exists collections_uid_idx on pfgb.collections (uid);
create table pfgb.ids (
	collection_uid bigint not null references pfgb.collections (uid) on delete cascade,
	hash bigint not null,
	key bigint not null,
	primary key (collection_uid, hash, key)
);
create index if not exists ids_key_idx on pfgb.ids (collection_uid, key);
