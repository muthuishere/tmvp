#!/bin/sh
# usage: seed.sh <env>   — runs inside the postgres container at init
set -e
ENVN="${TMVP_DEMO_ENV:-dev}"
psql -v ON_ERROR_STOP=1 -U acme -d acme -f /docker-entrypoint-initdb.d/01-schema.sql >/dev/null 2>&1 || true
psql -v ON_ERROR_STOP=1 -U acme -d acme <<SQL
insert into customers(email, region) select 'user'||g||'@example.test', (array['eu','us'])[1+g%2] from generate_series(1,2000) g;
insert into orders(customer_id,total_cents,status,payment_provider,created_at)
  select 1+(g%2000), 500+(g*37)%20000, (array['paid','paid','paid','shipped','refunded'])[1+g%5], 'payfast',
         now() - (g%720)*interval '1 minute' from generate_series(1,6000) g;
insert into webhook_events(provider,event,status_code,error,received_at)
  select 'payfast','payment.succeeded',200,null, now()-(g%720)*interval '1 minute' from generate_series(1,3000) g;
insert into app_config values ('payfast.webhook_secret_version','v3'),('payfast.api_base','https://api.payfast.example/v2');
SQL
if [ "$ENVN" = prod ]; then
psql -v ON_ERROR_STOP=1 -U acme -d acme <<SQL
-- the incident: a deploy 40 min ago rotated the webhook secret version in prod only
update app_config set value='v4' where key='payfast.webhook_secret_version';
insert into orders(customer_id,total_cents,status,payment_provider,created_at)
  select 1+(g%2000), 900+(g*53)%15000, 'pending_payment', 'payfast', now() - (g%38)*interval '1 minute' from generate_series(1,312) g;
insert into webhook_events(provider,event,status_code,error,received_at)
  select 'payfast','payment.succeeded',401,'signature mismatch (expected secret v4, got v3)', now()-(g%38)*interval '1 minute' from generate_series(1,312) g;
insert into events(kind,payload,at) select (array['view','click','cart','search'])[1+g%4], jsonb_build_object('n',g,'note','refund? maybe '||g), now()-(g%100000)*interval '1 second' from generate_series(1,2500000) g;
analyze;
SQL
else
psql -U acme -d acme -c "insert into events(kind,payload) select 'view', jsonb_build_object('n',g) from generate_series(1,5000) g; analyze;" >/dev/null
fi
