# n6k vs quack

We serve the same database four ways and query it from the DuckDB CLI:

- `n6kd`: n6k over WebSocket
- `n6kd quack`: DuckDB's quack protocol

each bridged through `perms.yml`, and with no `perms.yml`. No-perms quack
serves the default database, which `USE` does not change, so run.sh creates its
tables there. "native" is the same query on an in-process copy of the data, with
no network.

Results are in [results.md](results.md).

## How they differ

| | n6k | quack |
| --- | --- | --- |
| Login | API key, or a JWT per user | one token shared by everyone |
| Per-user rules | yes, from JWT claims | no |
| Catalogs | many | one |
| Status | | experimental until DuckDB v2.0 |

## Run it yourself

```sh
go build -o n6kd . && n6kd install
ROWS=1000000 N=5 bench/run.sh
```

run.sh also checks every answer. It runs the same query on a local copy of the
data and compares the row count and a hash of the rows. For a `LIMIT` with no
`ORDER BY`, any rows are a valid answer, so it compares only the count. A wrong
answer shows as `✗ result differs`.

To use local extension builds, set `N6K_EXTENSION_DIR` (see the top-level README)
and `N6K_CLIENT=<path>/n6k_client.duckdb_extension`. `DUCKDB` must be the same
version n6kd uses; run.sh checks this.
