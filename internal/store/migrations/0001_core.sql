create extension if not exists postgis;

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
  bbox geometry(polygon, 4326) generated always as (st_makeenvelope(minx, miny, maxx, maxy, 4326)) stored,
  data bytea not null
);

create index if not exists chunks_bbox_idx on chunks using gist (bbox);
create index if not exists chunks_collection_idx on chunks (collection, id);
