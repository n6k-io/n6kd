# n6kd

Serves DuckDB catalogs over the n6k protocol via WebSocket.

## Build

```sh
go build -o n6kd .
n6kd install   # download the n6k DuckDB extensions
```

To run against local extension builds instead, point `N6K_EXTENSION_DIR` at
their `build/release/extension` directories, colon-separated; each is searched
for `<name>/<name>.duckdb_extension`. `virtual_catalog_bridge` comes from
`duckdb-virtual-catalog`, `n6k_client` and `n6k_server` from `duckdb-driver`
(see `.air.toml`). Tests skip when it is unset.

## Run

```sh
n6kd -f schema.sql --perms perms.yml
```

Boot SQL is run in the order given by `-f` (file) and `-c` (statement). Without
`--perms`, every catalog in the loaded database is served read/write, unbridged.

### Flags

| Flag | Description |
| --- | --- |
| `-f, --file` | SQL file run at boot; repeatable |
| `-c, --command` | SQL run at boot; repeatable |
| `--perms` | perms.yml naming the catalogs to serve bridged |
| `--addr` | listen address (default `:8080`, or `$PORT`) |
| `--api-key` | token clients must present (`N6KD_API_KEY`) |
| `--lockdown` / `--no-lockdown` | deny served connections filesystem and extension access |
| `--debug` | per-connection and per-catalog logging |
| `--json-logging` | JSON logs with Cloud Logging severity fields |

### Commands

- `n6kd install` — download the extensions into DuckDB's extension directory
- `n6kd selftest` — bridge a throwaway catalog end to end

## perms.yml

```yaml
expose:
  app.users: readwrite
  app.audit_log: read

keys:
  app.users: [id]
```

Each key names a table as the server has it — `catalog.table` (schema defaults
to `main`) or `catalog.schema.table` — and it is served under that same name:

```sql
ATTACH 'n6k://localhost:8080' AS eggs (TYPE n6k, CATALOG 'app');
SELECT * FROM eggs.main.users;
```

The alias is the client's to pick; `CATALOG` selects the served catalog. An
unlisted table does not exist for clients.

`memory`, `system`, and `temp` cannot be exposed — every DuckDB already has
them, so a served catalog cannot take those names. Boot SQL must `ATTACH` a
catalog of its own rather than relying on the default.

## SQL functions

| Function | Description |
| --- | --- |
| `getenv(name)` | environment variable, NULL when unset; only available inside `-f` boot SQL |
| `n6kd_log(message)` | echoes the message into the server log, prefixed `sql:` |
| `n6kd_set_interval(name, INTERVAL, sql)` | repeats `sql` against the source every interval; boot SQL only |

```sql
SELECT n6kd_set_interval('refresh', INTERVAL 5 MINUTE,
  'INSERT INTO audit_log SELECT max(id) + 1, ''tick'' FROM audit_log');
```

The body runs inline, before the call returns, so the boot statements after it
see its effect and a broken statement fails startup rather than the first tick.
It cannot register another interval, and the calling statement's own snapshot
predates it. Months and years are rejected; the interval must be positive.

## Docker

```dockerfile
FROM n6k/n6kd
COPY schema.sql /app/schema.sql
COPY perms.yml  /app/perms.yml
CMD ["-f", "/app/schema.sql", "--perms", "/app/perms.yml"]
```
