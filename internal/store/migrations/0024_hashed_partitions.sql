do $$
declare
	p record;
	i record;
	fresh text;
begin
	for p in
		select c.oid, c.relname as old_name, pc.relname as parent,
		       substring(pg_get_expr(c.relpartbound, c.oid) from '''(.*)''') as collection
		from pg_inherits h
		join pg_class c on c.oid = h.inhrelid
		join pg_class pc on pc.oid = h.inhparent
		where pc.relname in ('chunks', 'chunk_props')
	loop
		fresh := p.parent || '_' || left(encode(sha256(convert_to(p.collection, 'UTF8')), 'hex'), 32);
		for i in
			select ic.relname from pg_index x join pg_class ic on ic.oid = x.indexrelid where x.indrelid = p.oid
		loop
			execute format('alter index %I rename to %I', i.relname, fresh || substr(i.relname, length(p.old_name) + 1));
		end loop;
		execute format('alter table %I rename to %I', p.old_name, fresh);
	end loop;
end $$;
