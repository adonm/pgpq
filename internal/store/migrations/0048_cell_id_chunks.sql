create sequence pgpq.cell_ids;

alter table pgpq.cells add column cell_id bigint not null default nextval('pgpq.cell_ids') unique;

create table pgpq.cell_chunks_v2 (
	cell_id bigint not null references pgpq.cells (cell_id) on delete cascade,
	no int not null,
	data bytea not null,
	primary key (cell_id, no),
	constraint cell_chunk_v2_shape check (no >= 0 and octet_length(data) between 1 and 8120)
) with (toast_tuple_target = 8160);

alter table pgpq.cell_chunks_v2 alter column data set storage external;
alter table pgpq.cell_chunks_v2 set (
	autovacuum_vacuum_scale_factor = 0.01,
	autovacuum_analyze_scale_factor = 0.02,
	autovacuum_vacuum_threshold = 1000
);

insert into pgpq.cell_chunks_v2 (cell_id, no, data)
select c.cell_id, ch.no, ch.data
from pgpq.cell_chunks ch
join pgpq.cells c
  on c.collection_uid = ch.collection_uid and c.level = ch.level and c.cell = ch.cell;

drop table pgpq.cell_chunks;
alter table pgpq.cell_chunks_v2 rename to cell_chunks;
