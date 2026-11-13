-- Scenario 3: EU confirmation emails silently fail in prod.
-- Haystack (all envs): ~300 config keys, an email outbox, a deploy history.
insert into app_config(key,value) select 'feature.'||g||'.enabled', (array['true','false'])[1+g%2] from generate_series(1,180) g on conflict do nothing;
insert into app_config(key,value) select 'limits.'||g||'.max', (100+g*7)::text from generate_series(1,110) g on conflict do nothing;
insert into app_config values ('app.base_url','https://'||:'env'||'.acme.example'),('email.eu.region','eu-west-1'),
  ('email.us.region','us-east-1'),('email.from','orders@acme.example'),('email.eu.sender_domain','mail.acme.example'),
  ('ui.banner',:'env'||' build') on conflict (key) do update set value = excluded.value;
create table if not exists email_outbox(id serial primary key, order_id int, region text, status text, error text, attempts int default 0, created_at timestamptz default now());
insert into email_outbox(order_id,region,status,created_at) select g,(array['eu','us'])[1+g%2],'sent', now()-(g%2000)*interval '1 minute' from generate_series(1,4000) g;
create table if not exists deploys(id serial primary key, version text, changed text, at timestamptz);
insert into deploys(version,changed,at) values ('2026.11.10','refund page',now()-interval '3 days'),('2026.11.12','email template copy',now()-interval '22 hours');
-- decoys
\if :{?decoy_dev2}
update app_config set value='false' where key='feature.7.enabled';
\endif
\if :{?decoy_staging}
update app_config set value='250' where key='limits.12.max';
update email_outbox set status='retrying', error='send failed: 429 rate limited', attempts=1 where id in (select id from email_outbox where region='us' limit 3);
\endif
-- the fault (prod only): a trailing space in one of ~300 values
\if :{?fault}
update app_config set value='eu-west-1 ' where key='email.eu.region';
update email_outbox set status='retrying', error='send failed: 400', attempts=5 where region='eu' and created_at > now()-interval '20 hours';
update email_outbox set status='retrying', error='send failed: 429 rate limited', attempts=1 where id in (select id from email_outbox where region='us' limit 4);
\endif
