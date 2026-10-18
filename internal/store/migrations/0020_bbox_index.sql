do $$
declare
	names text[];
	v text;
begin
	select coalesce(array_agg(viewname::text), '{}') into names from pg_views where schemaname = 'layers';
	foreach v in array names loop
		execute format('drop view layers.%I', v);
	end loop;
	alter table chunks drop column if exists cover;
	foreach v in array names loop
		execute format('create view layers.%I as select * from chunks where collection = %L and tier = 0', v, v);
	end loop;
end $$;

create index if not exists chunks_bbox_idx on chunks using gist (box(point(minx, miny), point(maxx, maxy)));
