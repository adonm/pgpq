do $$
declare
	v text;
begin
	for v in select viewname from pg_views where schemaname = 'layers' loop
		execute format('drop view layers.%I', v);
	end loop;
end $$;

drop schema if exists layers;
