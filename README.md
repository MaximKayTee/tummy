# tummy

tummy copies tables between Postgres, MySQL, SQL Server and SQLite, and loads data from REST APIs into them.

Connections and replications are TOML files. `tummy serve` runs replications on a schedule; `tummy` on its own opens a terminal UI.

## Build

```
make install     # ~/.local/bin/tummy
make check       # gofmt, vet, tests
```

## Config

Config lives in `~/.config/tummy/`: `connections.toml`, plus one file per replication in `replications/`. Run history goes in `~/.local/state/tummy/state.db`. Sample files are in `examples/`.

```toml
# connections.toml
[conn.PG]
type = "postgres"
url = "postgres://user:${PG_PASS}@host:5432/db"

[conn.MS]
type = "mssql"
url = "sqlserver://app%40tenant:${SP_SECRET}@host:1433?database=db&fedauth=ActiveDirectoryServicePrincipal"
read_only = true

[conn.LOCAL]
type = "sqlite"
path = "local.db"
```

`read_only = true`: never offered or accepted as a target. URL schemes: `postgres://`, `mysql://`, `sqlserver://`. The UI composes the url from host, port, database, user, password and options. SQL Server named instance: host `srv\INST`. Entra auth: `fedauth=` with `ActiveDirectoryPassword`, `ActiveDirectoryServicePrincipal` (user `client id@tenant id`, password the secret), `ActiveDirectoryDefault`, `ActiveDirectoryManagedIdentity` or `ActiveDirectoryAzCli`.

```toml
# replications/sales.toml
source = "PG"
target = "LOCAL"
mode = "incremental"
primary_key = ["id"]
update_key = "updated_at"
schedule = "*/15 * * * *"

[streams."sales.*"]
```

```toml
# replications/videos.toml
source = "FILES_API"
target = "LOCAL"
mode = "incremental"

[streams.files]
path = "/files"
primary_key = ["fileID"]
update_key = "fileID"

[streams.videos]
path = "/files/{files.fileID}/video"
for_each = "files"
primary_key = ["fileID"]
add_columns = { fileID = "{files.fileID}" }
skip_existing = "fileID"
for_each_since = "created, 90d"
skip_on = [404]
```

`for_each` runs the stream once per record of the parent stream. `skip_existing` names a column of the stream's target table: parents whose value is already loaded are skipped, so a parent without a child row is tried on every run; the parent stream is then read in full, not from its watermark. `for_each_since` is `field, value`: parents below it are skipped, with `Nd` meaning N days ago.

`tummy check NAME` validates a replication.

Previews never change a stream. Enter on a loaded stream shows its target data, `p` its source data; steps there are for looking and bookmarking. A stream is edited with `E`; in that wizard's preview ctrl+s saves the stream, everywhere else ctrl+s saves a bookmark.

```toml
# bookmarks.toml
[bookmark.paid_orders]
conn = "OUT"
table = "orders"
steps = "filter status = 'paid' | remove internal_notes"
rep = "sales"
stream = "orders"
```

Bookmarks are saved queries on a connection: a `table` or `sql`, with `steps`. Nothing is written to the connection or the replication. `rep` and `stream` name the stream the bookmark was made on; such bookmarks list under the stream in the replication detail. In the UI: ctrl+s on any preview outside the stream wizard, which saves the open bookmark or asks a name for a new one; bookmarks also list first in the connection's table list; `y` copies the query (OSC 52); `b` runs it in full and shows rows and time. In the sql editor (`S`) ctrl+s previews and ctrl+e executes the text as written, rows affected shown. `D` on a table, or `drop table` in the command palette, drops it after its name is typed. Both refused on `read_only` connections.

## Commands

```
tummy                  terminal UI
tummy run NAME         run a replication now
tummy runs [NAME]      run history
tummy list             replications and when they next run
tummy check [NAME]     validate config
tummy status           daemon health, failed runs
tummy serve            run replications on their schedules
tummy conns test NAME  check a connection works
```

## Running on a schedule

`tummy serve` is the scheduler. As a systemd user service:

```ini
# ~/.config/systemd/user/tummy.service
[Unit]
Description=tummy scheduler

[Service]
ExecStart=%h/.local/bin/tummy serve
EnvironmentFile=-%h/.config/tummy/tummy.env
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure

[Install]
WantedBy=default.target
```

```
systemctl --user enable --now tummy
```

## Development

`make check` before a commit. Tests drive the TUI through its model (`internal/tui/*_test.go`), no terminal needed.

Layout: `cmd/tummy` CLI, `internal/config` TOML, `internal/db` dialects, `internal/engine` runs, `internal/steps` transforms, `internal/api` REST sources, `internal/state` run history, `internal/daemon` scheduler, `internal/tui` screens.

`scripts/dev.sh` builds a sandbox in `.dev/` (local Postgres, seeded SQLite files, config) and opens the UI. Needs the `postgresql` package.
