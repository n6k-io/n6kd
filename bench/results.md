# Results

Run on 2026-10-30 on an Apple M3 Pro over localhost.
DuckDB v1.5.6 on both ends. Local builds of `vcatbridge`, `n6k_client` and
`n6k_server`.

## Where each query runs

"Server" means the server does the work and sends back only the result. "Client"
means the server sends the rows and the client does the work. Taken from the
`EXPLAIN` plans in `plans/`.

| | n6k | quack |
| --- | --- | --- |
| Pick columns | server | server |
| `WHERE` filter | server | client |
| `LIMIT` | server | client, but it stops reading early |
| `ORDER BY … LIMIT` | server | client |
| `GROUP BY`, `sum`, `count` | server | client |
| Join two remote tables | server | fails: `Multiple streaming scans … not currently supported` |
| `UPDATE` | works | fails: `Can only update base table`. Use `quack_query` instead. |
| `DELETE` | works | fails: `Can only delete from base table`. Use `quack_query` instead. |

## Timings

Median ± sample standard deviation of 5 runs, from the `latency` DuckDB's JSON
profiling reports on the client. The std is rounded to 1 significant figure (2
when it starts with a 1) and the median to the same decimal place. Every client
and server runs with 4 threads. Table `a` has 1M or 10M rows, table `b` has 100.
The queries are in `queries/`.

1M rows:

| query | native | n6k perms | quack perms | n6k no perms | quack no perms |
| --- | --- | --- | --- | --- | --- |
| point (`WHERE i = 42`) | 0.2 ±0.5ms | 1.3 ±0.9ms | 14 ±3ms | 0.8 ±0.5ms | 9 ±7ms |
| limit | 0.11 ±0.06ms | 1.3 ±1.0ms | 1.8 ±0.3ms | 0.7 ±0.6ms | 1.5 ±0.3ms |
| top-N | 1.64 ±0.17ms | 2.7 ±1.0ms | 16 ±2ms | 2.1 ±0.6ms | 14 ±4ms |
| group by | 0.93 ±0.11ms | 1.9 ±1.0ms | 12.1 ±1.6ms | 1.6 ±0.6ms | 11 ±4ms |
| scan (`sum(i)`) | 0.38 ±0.07ms | 1.4 ±1.0ms | 12 ±4ms | 0.9 ±0.5ms | 11 ±6ms |

10M rows:

| query | native | n6k perms | quack perms | n6k no perms | quack no perms |
| --- | --- | --- | --- | --- | --- |
| point (`WHERE i = 42`) | 0.24 ±0.06ms | 1.4 ±1.0ms | 63.7 ±1.8ms | 0.9 ±0.4ms | 57 ±5ms |
| limit | 0.11 ±0.05ms | 1.2 ±0.9ms | 1.5 ±0.6ms | 0.7 ±0.6ms | 1.6 ±0.6ms |
| top-N | 3.5 ±0.5ms | 4.7 ±1.1ms | 97 ±3ms | 3.8 ±0.6ms | 93 ±5ms |
| group by | 6.5 ±0.7ms | 5.3 ±1.1ms | 55.5 ±1.4ms | 7 ±2ms | 51 ±6ms |
| scan (`sum(i)`) | 1.8 ±0.4ms | 2.7 ±1.2ms | 51 ±4ms | 2 ±2ms | 55 ±3ms |

Every run succeeded and every result was correct.

## Data transfer

KiB up / down from the client for one run of the query, attach excluded, from
the client's own DuckDB log: `WS` for n6k (n6k frames, without WebSocket
framing), `HTTP` for quack (request and response bodies). It comes from an
extra, untimed run, since logging may cost time.

| query | n6k 1M | n6k 10M | quack 1M | quack 10M |
| --- | --- | --- | --- | --- |
| point (`WHERE i = 42`) | 2 / <1 | 2 / <1 | 3 / 15,649 | 26 / 156,484 |
| limit | 1 / 1 | 1 / 1 | <1 / 385 | <1 / 385 |
| top-N | 1 / 1 | 1 / 1 | 3 / 15,649 | 27 / 156,484 |
| group by | 1 / 2 | 1 / 2 | 3 / 7,829 | 26 / 78,288 |
| scan (`sum(i)`) | 1 / <1 | 1 / <1 | 3 / 7,829 | 27 / 78,288 |

n6k runs every one of these queries on the server and sends back only the result.
Quack sends every row to the client, except for `LIMIT`, where it stops early.
