package store

import "context"

func (i *Ingest) StageFeatureBase(ctx context.Context) error {
	_, err := i.tx.Exec(ctx, `alter table `+i.staged("chunks")+` add column if not exists feature_base bigint not null default 0`)
	return err
}

func (i *Ingest) ComputeFeatureBase(ctx context.Context) error {
	_, err := i.tx.Exec(ctx, `
		with base as (
			select id, coalesce(sum(feature_count) over (
				partition by tier order by id
				rows between unbounded preceding and 1 preceding
			), 0) feature_base
			from `+i.staged("chunks")+`
		)
		update `+i.staged("chunks")+` c set feature_base = base.feature_base from base where c.id = base.id`)
	return err
}
