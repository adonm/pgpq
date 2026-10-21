alter table pgpq.cells add column bytes bigint not null default 0;
update pgpq.cells set bytes = octet_length(data);
