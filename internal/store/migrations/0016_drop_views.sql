delete from collections where kind <> 'native';
alter table collections drop column if exists members;
