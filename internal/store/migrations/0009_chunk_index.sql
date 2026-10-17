drop index if exists chunks_tier_idx;

create index if not exists chunks_tier_idx on chunks (tier, id);

do $$
begin
	alter table chunks alter column data set compression lz4;
exception when others then null;
end $$;
