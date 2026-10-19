create table pfgb.overview_dirty (
	collection text not null references pfgb.collections (name) on delete cascade,
	cell bigint not null,
	level int not null,
	dirty_since timestamptz not null default now(),
	primary key (collection, cell, level)
);
