alter table chunks add column if not exists checksum text;
alter table chunks add column if not exists columns jsonb not null default '[]';
