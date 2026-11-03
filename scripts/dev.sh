#!/usr/bin/env bash
# Development sandbox: local Postgres, seeded SQLite files, a config home with
# connections and replications, then tummy against it.
#   scripts/dev.sh            TUI
#   scripts/dev.sh serve      daemon
#   scripts/dev.sh run sales  any tummy subcommand
#   scripts/dev.sh reset      wipe .dev and start over
#   scripts/dev.sh install    copy the sandbox config into ~/.config/tummy, existing files kept as *.bak.TIMESTAMP
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
DEV=$ROOT/.dev
HOME_DIR=$DEV/home
STATE=$DEV/state.db

if [ "${1:-}" = "install" ]; then
  "$0" list >/dev/null || true   # makes sure .dev exists and the cluster runs
  DEST=${XDG_CONFIG_HOME:-$HOME/.config}/tummy
  STAMP=$(date +%Y%m%d-%H%M%S)
  mkdir -p -m 700 "$DEST/replications"
  backup() { if [ -e "$1" ]; then cp -p "$1" "$1.bak.$STAMP"; echo "saved $1.bak.$STAMP"; fi; }
  backup "$DEST/connections.toml"
  (umask 077; sed "s|\.\./data/|$DEV/data/|" "$HOME_DIR/connections.toml" > "$DEST/connections.toml.new")
  mv "$DEST/connections.toml.new" "$DEST/connections.toml"
  for f in "$HOME_DIR"/replications/*.toml; do
    backup "$DEST/replications/$(basename "$f")"
    cp "$f" "$DEST/replications/"
  done
  echo "installed sandbox config into $DEST (postgres: scripts/devpg.sh start)"
  exit 0
fi

if [ "${1:-}" = "reset" ]; then
  "$ROOT/scripts/devpg.sh" stop >/dev/null 2>&1 || true
  rm -rf "$DEV"
  shift || true
fi

DSN=$("$ROOT/scripts/devpg.sh" start)
mkdir -p "$HOME_DIR/replications" "$DEV/data"

if [ ! -f "$DEV/data/shop.db" ]; then
  sqlite3 "$DEV/data/shop.db" <<'EOF'
create table products (id integer primary key, sku text, name text, price real, updated_at text);
insert into products values (1,'A1','Kettle',29.99,'2026-01-01'),(2,'B2','Toaster',24.5,'2026-01-02'),(3,'C3','Blender',89,'2026-01-03');
create table stock (sku text primary key, qty integer, updated_at text);
insert into stock values ('A1',10,'2026-01-01'),('B2',0,'2026-01-02'),('C3',4,'2026-01-03');
EOF
fi

if [ ! -f "$HOME_DIR/connections.toml" ]; then
  cat >"$HOME_DIR/connections.toml" <<EOF
# local throwaway postgres, scripts/devpg.sh
[conn.DEVPG]
type = "postgres"
url = "$DSN"
schema = "public"

[conn.SHOP]
type = "sqlite"
path = "../data/shop.db"

[conn.LOCAL]
type = "sqlite"
path = "../data/local.db"

[conn.JSONPLACEHOLDER]
type = "api"
base_url = "https://jsonplaceholder.typicode.com"
rate_limit = "600/m"

[conn.POKEAPI]
type = "api"
base_url = "https://pokeapi.co/api/v2"
rate_limit = "100/m"
EOF
fi

if [ ! -f "$HOME_DIR/replications/sales.toml" ]; then
  cat >"$HOME_DIR/replications/sales.toml" <<'EOF'
# postgres -> sqlite, every table in sales, orders incrementally
source = "DEVPG"
target = "LOCAL"
mode = "incremental"
primary_key = ["id"]
exclude = ["sales.tmp_*"]

[streams."sales.*"]

[streams."sales.orders"]
update_key = "updated_at"

[streams."sales.order_items"]
primary_key = ["order_id", "line"]

[streams."analytics.daily_sales"]
mode = "full-refresh"
primary_key = []
EOF
  cat >"$HOME_DIR/replications/hr.toml" <<'EOF'
# postgres -> postgres, into a different schema on the same server
source = "DEVPG"
target = "DEVPG"
mode = "full-refresh"
object = "analytics.{stream_table}_copy"
schedule = "*/10 * * * *"

[streams."hr.employees"]
select = ["employee_id", "full_name", "dept", "hired", "active"]
where = "active"
EOF
  cat >"$HOME_DIR/replications/shop.toml" <<'EOF'
# sqlite -> postgres
source = "SHOP"
target = "DEVPG"
mode = "incremental"
update_key = "updated_at"
object = "public.shop_{stream_table}"

[streams.products]
primary_key = ["id"]

[streams.stock]
primary_key = ["sku"]
EOF
  cat >"$HOME_DIR/replications/pokemon.toml" <<'EOF'
# slow by design: 100 requests a minute, good for watching progress
source = "POKEAPI"
target = "LOCAL"
mode = "full-refresh"
records = "results[]"
paginate = "cursor"
cursor_field = "next"
page_size = 20

[streams.pokemon]
path = "/pokemon"
limit = 200

[streams.berry]
path = "/berry"
EOF
  cat >"$HOME_DIR/replications/placeholder.toml" <<'EOF'
# api -> postgres, parent/child endpoints
source = "JSONPLACEHOLDER"
target = "DEVPG"
mode = "full-refresh"
object = "public.jp_{stream_name}"

[streams.users]
path = "/users"

[streams.user_posts]
path = "/users/{users.id}/posts"
for_each = "users"
add_columns = { user_name = "{users.name}" }
EOF
fi

exec tummy --home "$HOME_DIR" --state "$STATE" "$@"
