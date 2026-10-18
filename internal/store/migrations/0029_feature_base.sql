alter table chunks add column if not exists feature_base bigint not null default 0;

create index if not exists chunks_feature_base_idx on chunks (collection, tier, feature_base);

with base as (
	select id,
		coalesce(sum(feature_count) over (
			partition by collection, tier
			order by id
			rows between unbounded preceding and 1 preceding
		), 0) as feature_base
	from chunks
)
update chunks set feature_base = base.feature_base from base where chunks.id = base.id;
