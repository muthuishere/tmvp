create table customers (id serial primary key, email text not null, region text not null, created_at timestamptz default now());
create table orders (id serial primary key, customer_id int references customers(id), total_cents int not null,
  status text not null, payment_provider text not null, created_at timestamptz default now(), updated_at timestamptz default now());
create index orders_status_idx on orders(status, created_at);
create table webhook_events (id serial primary key, provider text, event text, status_code int, error text, received_at timestamptz default now());
create table events (id bigserial primary key, kind text, payload jsonb, at timestamptz default now());
create table app_config (key text primary key, value text);
