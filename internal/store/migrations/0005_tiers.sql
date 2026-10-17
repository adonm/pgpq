alter table chunks add column if not exists tier smallint not null default 0;

create index if not exists chunks_tier_idx on chunks (collection, tier, id);
