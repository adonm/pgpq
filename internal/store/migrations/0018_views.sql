alter table collections add column if not exists members jsonb;
alter table virtual_file add column if not exists features bigint not null default 0;
