do $$
declare
	names text[];
	qualified text;
begin
	select array_agg(format('%I.%I', n.nspname, c.relname))
	into names
	from pg_class c
	join pg_namespace n on n.oid = c.relnamespace
	where c.relkind in ('r', 'p') and c.relname like 'chunk_props%';
	foreach qualified in array coalesce(names, '{}') loop
		execute 'drop table if exists ' || qualified || ' cascade';
	end loop;
end $$;

alter table collections drop column if exists sortable;
