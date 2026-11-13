-- Scenario 2: refunds page fails only in staging (staging missed migration 42).
-- Run against each env DB; set :env first:  psql -v env=staging -f 2-missed-migration.sql
create table if not exists schema_migrations(version int primary key, applied_at timestamptz default now());
insert into schema_migrations(version)
  select g from generate_series(1, case when :'env' = 'staging' then 41 else 42 end) g on conflict do nothing;
-- every env except staging gets migration 42
\if :{?skip42}
\else
alter table orders add column if not exists refund_reason text;
update orders set refund_reason = 'damaged' where status = 'refunded' and id % 3 = 0;
\endif
