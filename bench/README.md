# n6k vs quack

We serve the same database four ways and query it from the DuckDB CLI:

- `n6kd`: n6k over WebSocket
- `n6kd quack`: DuckDB's quack protocol

each bridged through `perms.yml`, and with no `perms.yml`. "native" is the same
query on an in-process copy of the data, with no network.

Results are in [results.md](results.md).

## How they differ

| | n6k | quack |
| --- | --- | --- |
| Login | API key, or a JWT per user | one token shared by everyone |
| Per-user rules | yes, from JWT claims | no |
| Catalogs | many | one |

## Run it yourself

```sh
go build -o n6kd . && ./n6kd install
ROWS=1000000 N=5 bench/run.py
```

run.py also checks every answer against a local copy of the data (see
`fingerprint` in run.py). A wrong answer shows as `✗ result differs`.

To use local extension builds, set `N6K_EXTENSION_DIR` (see the top-level README)
and `N6K_CLIENT=<path>/n6k_client.duckdb_extension`. run.py runs under `uv`
with the `duckdb` package pinned to the version n6kd embeds, and checks this.
