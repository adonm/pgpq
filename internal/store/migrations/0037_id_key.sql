alter table pfgb.ids add column if not exists key bigint not null default 0;
alter table pfgb.ids drop constraint if exists ids_pkey;
alter table pfgb.ids add primary key (collection, hash, cell, key);
