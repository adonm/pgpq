create table pfgb.id_runs (
	file_id bigint primary key,
	collection text not null references pfgb.collections (name) on delete cascade,
	entries bigint not null,
	begin_snapshot bigint not null,
	end_snapshot bigint
);

create index id_runs_live_idx on pfgb.id_runs (collection) where end_snapshot is null;
