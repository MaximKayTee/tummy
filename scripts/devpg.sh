#!/usr/bin/env bash
# Local throwaway Postgres for development and tests. Needs the postgresql package.
# scripts/devpg.sh start|stop|status|reset|psql|dsn
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
PGROOT=${XDG_CACHE_HOME:-$HOME/.cache}/tummy/pg
PGDATA=$PGROOT/data
PGPORT=${TUMMY_DEVPG_PORT:-54329}
PGUSER=tummy
PGDB=tummy_dev
DSN="postgres://$PGUSER@127.0.0.1:$PGPORT/$PGDB?sslmode=disable"

init() {
  [ -d "$PGDATA" ] && return
  mkdir -p "$PGROOT"
  initdb -D "$PGDATA" -U "$PGUSER" -A trust --locale=C.UTF-8 -E UTF8 >"$PGROOT/initdb.log" 2>&1
  cat >>"$PGDATA/postgresql.conf" <<EOF
port = $PGPORT
listen_addresses = '127.0.0.1'
unix_socket_directories = '$PGROOT'
logging_collector = off
fsync = off
synchronous_commit = off
full_page_writes = off
EOF
}

running() { pg_ctl -D "$PGDATA" status >/dev/null 2>&1; }

start() {
  init
  if ! running; then
    pg_ctl -D "$PGDATA" -l "$PGROOT/postgres.log" -w start >/dev/null
  fi
  if ! psql -h 127.0.0.1 -p "$PGPORT" -U "$PGUSER" -d postgres -tAc "select 1 from pg_database where datname='$PGDB'" | grep -q 1; then
    createdb -h 127.0.0.1 -p "$PGPORT" -U "$PGUSER" "$PGDB"
    seed
  fi
}

seed() {
  psql -q -h 127.0.0.1 -p "$PGPORT" -U "$PGUSER" -d "$PGDB" -v ON_ERROR_STOP=1 <<'EOF'
create schema if not exists sales;
create schema if not exists hr;
create schema if not exists analytics;

create table sales.customers (
  id serial primary key,
  name text not null,
  email text,
  country char(2),
  created_at timestamp not null default now()
);
create table sales.orders (
  id serial primary key,
  customer_id int references sales.customers(id),
  status text not null,
  total numeric(12,2) not null,
  placed_on date not null,
  updated_at timestamp not null default now(),
  meta jsonb
);
create table sales.order_items (
  order_id int references sales.orders(id),
  line int,
  sku text,
  qty int,
  price numeric(10,2),
  primary key (order_id, line)
);
create table hr.employees (
  employee_id int primary key,
  full_name text,
  dept text,
  salary numeric(10,2),
  hired date,
  active boolean default true,
  updated_at timestamptz default now()
);
create table sales.tmp_scratch (x int);

insert into sales.customers (name, email, country, created_at)
select 'Customer ' || g, 'c' || g || '@example.com', (array['NL','DE','GB','US'])[1 + g % 4], now() - (g || ' days')::interval
from generate_series(1, 200) g;

insert into sales.orders (customer_id, status, total, placed_on, updated_at, meta)
select 1 + (g % 200), (array['paid','pending','refunded'])[1 + g % 3], round((random() * 500)::numeric, 2),
       date '2026-01-01' + (g % 300), timestamp '2026-01-01' + (g || ' hours')::interval,
       jsonb_build_object('source', (array['web','app','phone'])[1 + g % 3], 'items', 1 + g % 5)
from generate_series(1, 5000) g;

insert into sales.order_items (order_id, line, sku, qty, price)
select o.id, l, 'SKU-' || ((o.id * 7 + l) % 97), 1 + (o.id + l) % 4, round((random() * 90 + 10)::numeric, 2)
from sales.orders o, generate_series(1, 2) l;

insert into hr.employees (employee_id, full_name, dept, salary, hired, active)
select g, 'Employee ' || g, (array['eng','ops','sales','hr'])[1 + g % 4], 30000 + g * 37, date '2015-01-01' + g * 11, g % 13 <> 0
from generate_series(1, 150) g;

create view analytics.daily_sales as
select placed_on as day, status, count(*) as orders, sum(total) as revenue
from sales.orders group by 1, 2;

create table analytics.kv (k text primary key, v text);
insert into analytics.kv values ('seeded_at', now()::text);
EOF
}

case "${1:-status}" in
  start) start; echo "$DSN" ;;
  stop) running && pg_ctl -D "$PGDATA" -w stop -m fast >/dev/null; echo stopped ;;
  status) running && echo "running on $PGPORT" || echo "not running" ;;
  reset) running && pg_ctl -D "$PGDATA" -w stop -m fast >/dev/null; rm -rf "$PGROOT"; start; echo "$DSN" ;;
  psql) shift; exec psql -h 127.0.0.1 -p "$PGPORT" -U "$PGUSER" -d "$PGDB" "$@" ;;
  dsn) echo "$DSN" ;;
  *) echo "usage: $0 start|stop|status|reset|psql|dsn"; exit 1 ;;
esac
