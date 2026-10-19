alter table pfgb.chunks add column col_file bigint not null default 0;
alter table pfgb.chunks add column zone text not null default '[]';
