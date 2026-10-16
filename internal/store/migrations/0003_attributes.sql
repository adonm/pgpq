create table if not exists chunk_props (
  chunk_id bigint not null references chunks(id) on delete cascade,
  feature_ord int not null,
  props jsonb not null,
  primary key (chunk_id, feature_ord)
);

create index if not exists chunk_props_gin_idx on chunk_props using gin (props jsonb_path_ops);
