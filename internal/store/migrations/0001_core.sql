create table if not exists collections (
  name text primary key,
  created_at timestamptz not null default now()
);

create table if not exists chunks (
  id bigint generated always as identity primary key,
  collection text not null references collections(name) on delete cascade,
  feature_count int not null,
  size_bytes int not null,
  minx double precision not null,
  miny double precision not null,
  maxx double precision not null,
  maxy double precision not null,
  cover int8multirange not null,
  data bytea not null
);

create index if not exists chunks_cover_idx on chunks using gist (cover);
create index if not exists chunks_collection_idx on chunks (collection, id);
