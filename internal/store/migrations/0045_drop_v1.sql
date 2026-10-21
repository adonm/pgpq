do $$
declare rel record;
begin
	for rel in
		select c.relname
		from pg_class c
		join pg_namespace n on n.oid = c.relnamespace
		where n.nspname = 'public' and c.relkind = 'r' and c.relname <> 'schema_migrations'
	loop
		execute format('drop table if exists public.%I cascade', rel.relname);
	end loop;
end
$$;
