drop index if exists pfgb.chunks_snapshot_idx;
drop index if exists pfgb.chunks_live_cell_idx;
alter table pfgb.chunks drop column if exists begin_snapshot;
alter table pfgb.chunks drop column if exists end_snapshot;
create index if not exists chunks_cell_idx on pfgb.chunks (collection, cell, level);
drop table if exists pfgb.id_runs;
drop table if exists pfgb.partitions;
drop table if exists pfgb.snapshots;
alter table pfgb.collections drop column if exists current_snapshot;
create table if not exists pfgb.ids (
	collection text not null references pfgb.collections (name) on delete cascade,
	hash bigint not null,
	cell bigint not null,
	primary key (collection, hash, cell)
);
