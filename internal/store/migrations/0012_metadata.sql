alter table collections add column if not exists title text;
alter table collections add column if not exists description text;
alter table collections add column if not exists license text;
alter table collections add column if not exists keywords text[] not null default '{}';
