create table pgpq.cell_chunks (
	collection_uid bigint not null references pgpq.collections (uid) on delete cascade,
	level smallint not null,
	cell bigint not null,
	no int not null,
	data bytea not null,
	primary key (collection_uid, level, cell, no),
	constraint cell_chunk_shape check (no >= 0 and octet_length(data) between 1 and 8120)
) with (toast_tuple_target = 8160);

alter table pgpq.cell_chunks alter column data set storage external;
alter table pgpq.cell_chunks set (
	autovacuum_vacuum_scale_factor = 0.01,
	autovacuum_analyze_scale_factor = 0.02,
	autovacuum_vacuum_threshold = 1000
);

insert into pgpq.cell_chunks (collection_uid, level, cell, no, data)
select c.collection_uid, c.level, c.cell, n,
       substring(c.data from (n * 8120 + 1)::int for 8120)
from pgpq.cells c
cross join generate_series(0, greatest(0, (c.bytes + 8119) / 8120 - 1)) as g(n);

alter table pgpq.cells drop column data;
