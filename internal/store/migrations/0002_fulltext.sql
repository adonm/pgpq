create extension if not exists pg_trgm;

create table if not exists chunk_text (
  chunk_id bigint not null references chunks(id) on delete cascade,
  feature_ord int not null,
  tsv tsvector not null,
  primary key (chunk_id, feature_ord)
);

create index if not exists chunk_text_tsv_idx on chunk_text using gin (tsv);
