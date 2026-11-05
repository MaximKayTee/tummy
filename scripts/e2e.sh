#!/usr/bin/env bash
# First-run end-to-end: builds ~/.local/bin/tummy, then drives it with default paths
# (XDG redirected to a temp dir, removed on exit; KEEP=1 keeps it). Needs tmux and sqlite3;
# the API steps need network and are skipped without it. Run: scripts/e2e.sh
set -u
REPO=$(cd "$(dirname "$0")/.." && pwd)
BIN=$HOME/.local/bin/tummy
(cd "$REPO" && go build -o "$BIN" ./cmd/tummy) || { echo "build failed"; exit 1; }

ROOT=$(mktemp -d "${TMPDIR:-/tmp}/tummy-e2e.XXXXXX")
export XDG_CONFIG_HOME=$ROOT/config
export XDG_STATE_HOME=$ROOT/state
export TERM=xterm-256color LC_ALL=C.UTF-8
unset TUMMY_HOME TUMMY_STATE TUMMY_CONNECTIONS
CFG=$XDG_CONFIG_HOME/tummy
DB=$XDG_STATE_HOME/tummy/state.db
T="tmux -L tummy-e2e"
S=e2e
FAIL=0
cleanup() { $T kill-server 2>/dev/null; [ "${KEEP:-}" = 1 ] && echo "kept $ROOT" || rm -rf "$ROOT"; }
trap cleanup EXIT
pass(){ echo "PASS  $1"; }
# the first failure ends the run: everything after it would fail for the same reason
fail(){ echo "FAIL  $1"; FAIL=$((FAIL+1)); screen 2>/dev/null | sed '/^$/d' | sed 's/^/      /'; echo; echo "FAILURES: $FAIL  (${SECONDS}s)"; exit 1; }
section(){ echo "== $1  (${SECONDS}s)"; }
screen(){ $T capture-pane -t $S -p; }
until_seen(){ # until_seen "regex" [seconds]  polls the screen
  local n=$(( ${2:-6} * 20 )); while ! screen | grep -qE -- "$1"; do n=$((n-1)); [ $n -le 0 ] && return 1; sleep 0.05; done
}
until_true(){ # until_true seconds command...  polls a command
  local n=$(( $1 * 10 )); shift; while ! "$@" >/dev/null 2>&1; do n=$((n-1)); [ $n -le 0 ] && return 1; sleep 0.1; done
}
expect(){ # expect "label" "regex" [seconds]
  if until_seen "$2" "${3:-3}"; then pass "$1"; else fail "$1 (wanted /$2/)"; fi
}
check(){ # check "label" command...
  local label=$1; shift
  if "$@"; then pass "$label"; else fail "$label"; fi
}
k(){ $T send-keys -t $S "$@"; sleep 0.05; }
typ(){ $T send-keys -t $S -l "$1"; sleep 0.05; }
sum(){ cksum < "$1" 2>/dev/null; }
changed(){ [ "$(sum "$1")" != "$2" ]; }
q(){ sqlite3 "$DB" "$1"; }
online=0
curl -sf -m 5 -o /dev/null https://jsonplaceholder.typicode.com/users/1 && online=1

# a database with something in it, somewhere unrelated to the config dir
mkdir -p "$ROOT/data" && sqlite3 "$ROOT/data/shop.db" "create table orders (id integer primary key, customer text, total real, updated_at text); insert into orders values (1,'ann',10.5,'2026-01-01'),(2,'bob',20,'2026-01-02'); create table customers (id integer primary key, name text); insert into customers values (1,'Ann'),(2,'Bob');"

section "CLI on a fresh machine"
"$BIN" conns list >/dev/null 2>&1 && pass "conns list, no file" || fail "conns list, no file"
"$BIN" list >/dev/null 2>&1 && pass "list, no replications dir" || fail "list, no replications dir"
"$BIN" runs >/dev/null 2>&1 && pass "runs, no state" || fail "runs, no state"
"$BIN" --version | grep -q '^tummy ' && pass "--version" || fail "--version"

section "TUI first run"
$T kill-server 2>/dev/null; $T new-session -d -s $S -x 110 -y 28 "$BIN"; expect "empty list" "^ +none *$"
k n; expect "n without connections opens the connection form" "no connections"
k Escape; k c; expect "connections empty" "^ +none *$"

section "create sqlite connection SHOP"
k n; typ "SHOP"; k Tab; k Right; k Right; k Right; k Tab; typ "$ROOT/data/shop.db"; k C-g; expect "probe connected" "connected"
expect "probe lists tables" "customers"
k C-s; expect "SHOP saved" "SHOP +sqlite"
check "connections.toml created at default path" test -f "$CFG/connections.toml"
check "connections.toml is 0600" test "$(stat -c %a "$CFG/connections.toml")" = 600

section "create sqlite connection LOCAL with a relative path"
k n; typ "LOCAL"; k Tab; k Right; k Right; k Right; k Tab; typ "local.db"; k C-s; expect "LOCAL saved" "LOCAL +sqlite"

if [ $online = 1 ]; then
  section "create API connection"
  k n; typ "PLACEHOLDER"; k Tab; k Right; k Right; k Right; k Right; k Tab; typ "https://jsonplaceholder.typicode.com"; k Tab; k Tab; typ "300/m"; k C-g; expect "api probe" "connected" 10
  k C-s; expect "API saved" "PLACEHOLDER +api"
  "$BIN" conns test PLACEHOLDER | grep -q '^ok  HTTP' && pass "conns test on an API" || fail "conns test on an API"
else
  echo "SKIP  API connection (offline)"
fi

section "test a connection from the list"
k G; until_seen "› +SHOP"; k t; expect "test result shown" "SHOP .*✓ ok"

section "explore SHOP: preview, tick, add to new replication"
k G; until_seen "› +SHOP"; k Enter; expect "explorer tables" "orders"
k Enter; expect "preview grid" "customer"
expect "preview rows" "ann"
k Right; k Right; k Space; expect "untick total" "3/4 columns"
k +; expect "target pick" "Add to replication"
k Tab; typ "shop"; k Tab; k Enter; k Enter; expect "stream form" "Add stream to shop"
k C-s; expect "status after add" "saved shop.toml › orders"
REP=$CFG/replications/shop.toml
grep -q 'select = \["id", "customer", "updated_at"\]' "$REP" && pass "columns written" || { fail "columns written"; cat "$REP"; }
grep -q '^target = "LOCAL"' "$REP" && pass "target LOCAL" || { fail "target LOCAL"; cat "$REP"; }
"$BIN" check shop >/dev/null && pass "tummy check accepts the TUI's file" || { fail "tummy check"; "$BIN" check shop; }

section "back to list, run it in-process"
k Escape; k Escape; k Escape; expect "list shows shop" "shop +SHOP +LOCAL"
k r; expect "ran ok" "shop .*✓ ok"
sqlite3 "$CFG/local.db" "select count(*) from orders" | grep -q '^2$' && pass "rows landed in local.db next to config" || fail "rows in local.db"
sqlite3 "$CFG/local.db" "pragma table_info(orders)" | grep -q total && fail "unticked column was copied" || pass "unticked column left out"

section "edit replication: add customers via picker, set schedule, save"
BEFORE=$(sum "$REP")
k E; expect "edit basics" "Edit replication shop"
k Tab; k Tab; k Tab; k Right; k F3; expect "defaults page" "^ Defaults"
k 3; expect "streams page" "a add tables"
k a; expect "picker" "SHOP +[0-9]+ selected"
typ "cust"; k Space; k Enter; expect "customers added" "› customers"
k C-s; expect "review" "replications/shop.toml"
expect "review shows the change" "^ +\+ \[streams.customers\]"
k C-s; check "file saved" until_true 6 changed "$REP" "$BEFORE"
expect "list shows schedule" "\*/5 \* \* \* \*"
grep -q '^schedule = "\*/5 \* \* \* \*"' "$REP" && pass "schedule written" || { fail "schedule written"; cat "$REP"; }
grep -q 'select = \["id", "customer", "updated_at"\]' "$REP" && pass "earlier columns kept through edit" || { fail "columns lost in edit"; cat "$REP"; }

section "comments survive a TUI save"
sed -i '1i # my note' "$REP"
BEFORE=$(sum "$REP")
k e; expect "toggle off" "shop.*disabled"
k e; expect "toggle on" "shop.*enabled"
head -1 "$REP" | grep -q '# my note' && pass "toggle keeps comment" || fail "toggle lost comment"
grep -c '^enabled' "$REP" | grep -q '^1$' && pass "one enabled key" || { fail "enabled key count"; cat "$REP"; }
BEFORE=$(sum "$REP")
k E; expect "edit again" "Edit replication shop"; k C-s; expect "review again" "replications/shop.toml"; k C-s
check "wizard save wrote the file" until_true 6 changed "$REP" "$BEFORE"
head -1 "$REP" | grep -q '# my note' && pass "wizard save keeps comment" || { fail "wizard save lost comment"; head -3 "$REP"; }

if [ $online = 1 ]; then
  section "API replication through the wizard, then run"
  k n; typ "users"; k Tab; k Right; k Tab; k Enter; typ "LOCAL"; k Enter; k F3; expect "api defaults" "Records"
  k F3; expect "api streams" "^ Streams"; k a; expect "endpoint form" "New stream"
  typ "users"; k Tab; typ "/users"; k C-s; expect "endpoint listed" "path=/users"
  k C-s; expect "api review" "replications/users.toml"; k C-s; expect "users saved" "users +PLACEHOLDER +LOCAL"
  expect "cursor lands on the new replication" "› users"
  k r; expect "users ran" "10 rows" 15
fi

section "daemon with default paths"
$T new-session -d -s d -x 160 -y 20 "$BIN serve"; expect "badge alive" "● daemon"
k R; expect "next run shown" "in [0-9]+[ms]"
LAST=$(q "select coalesce(max(id),0) from runs")
k g; until_seen "› +shop"; k r; expect "run queued to the daemon" "queued: shop"
daemon_ran(){ [ "$(q "select count(*) from runs where id > $LAST and replication = 'shop' and trigger in ('tui', 'daemon') and status = 'success'")" -ge 1 ]; }
check "daemon wrote a new queued run" until_true 15 daemon_ran
$T capture-pane -t d -p -S - | grep -Eq 'msg=run replication=shop trigger=(tui|daemon)' && pass "daemon log shows the run" || fail "daemon log shows the run"
"$BIN" list | grep -q '^shop ' && pass "tummy list sees files" || fail "tummy list"
"$BIN" status >/dev/null; [ $? -le 1 ] && pass "tummy status exits 0 or 1" || fail "tummy status"
"$BIN" serve 2>&1 | grep -q 'already running' && pass "second daemon refused" || fail "second daemon refused"

section "delete connection and replication"
k c; k G; until_seen "› +SHOP"; k d; expect "delete prompt" "delete connection SHOP"
k y; expect "deleted" "deleted"; screen | grep -qE "^[› ] +SHOP " && fail "SHOP still listed" || pass "connection deleted"
head -1 "$CFG/connections.toml" | grep -q '^\[' && pass "no leading blank line after delete" || fail "leading blank line after delete"
if [ $online = 1 ]; then
  k Escape; k G; k d; expect "replication delete prompt" "delete users.toml"; k y; until_seen "deleted"
  screen | grep -qE "^[› ] +users " && fail "users still listed" || pass "replication deleted"
fi

section "narrow terminal"
$T resize-window -t $S -x 72 -y 16; until_seen "tummy" 1
W=0; while IFS= read -r line; do [ ${#line} -gt 72 ] && W=$((W+1)); done < <(screen)
[ "$W" = "0" ] && pass "no line wider than 72" || fail "$W lines wider than 72"

k q
echo; echo "FAILURES: $FAIL  (${SECONDS}s)"
exit $FAIL
