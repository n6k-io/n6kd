# Results

Run on 2026-11-07 on Apple M3 Pro over localhost.
DuckDB v2.0.0 on both ends.
Local extension builds.

The queries are in `queries/`. A query with a `.reset` file changes data, restored after each run.
Native on the two-server join is two in-process databases, so it is a best-case floor.
Quack serves one catalog, so the two-catalog join is `n/a` for it.

## 1,000,000 rows

Median ± sample standard deviation of 5 runs of the client-side profiled `query.total_time`.

| query | native | n6k perms | quack perms | n6k no perms | quack no perms |
| --- | --- | --- | --- | --- | --- |
| 1_point | 0.21 ±0.03ms | 1.6 ±0.5ms | 2.3 ±0.3ms | 1.0 ±0.3ms | 1.54 ±0.14ms |
| 2_limit | 0.13 ±0.03ms | 1.5 ±0.2ms | 2.1 ±0.3ms | 0.98 ±0.14ms | 1.47 ±0.07ms |
| 3_topn | 1.42 ±0.12ms | 3.2 ±0.4ms | 3.7 ±0.5ms | 2 ±3ms | 2.7 ±0.2ms |
| 4_groupby | 1.3 ±0.3ms | 2.3 ±0.4ms | 4.1 ±0.5ms | 2.3 ±0.6ms | 2.53 ±0.16ms |
| 5_join | 0.59 ±0.05ms | 3.1 ±0.3ms | 2.9 ±0.3ms | 1.7 ±0.7ms | 2.1 ±0.2ms |
| 6_scan | 0.34 ±0.04ms | 1.9 ±0.3ms | 2.54 ±0.16ms | 1.21 ±0.16ms | 1.77 ±0.14ms |
| 7_join_catalogs | 0.54 ±0.04ms | 3.1 ±0.3ms | n/a | 1.7 ±0.2ms | n/a |
| 8_join_servers | 0.54 ±0.09ms | 3.53 ±0.15ms | 15.2 ±0.9ms | 2.2 ±0.3ms | 12.0 ±1.2ms |
| 9_join_local | 2.8 ±0.6ms | 46 ±11ms | 27 ±5ms | 50 ±80ms | 26 ±3ms |
| 10_in_subquery | 1.19 ±0.13ms | 7.7 ±1.1ms | 7.6 ±0.7ms | 3.7 ±0.5ms | 3.3 ±0.3ms |
| 11_view | 0.44 ±0.05ms | 2.6 ±0.6ms | 11.7 ±0.9ms | 1.6 ±0.4ms | 9 ±3ms |
| 12_insert | 2 ±2ms | 5 ±2ms | 4.2 ±1.7ms | 3.0 ±0.3ms | 3.0 ±1.4ms |
| 13_update | 0.49 ±0.03ms | 2.2 ±0.5ms | 2.7 ±0.3ms | 1.4 ±0.2ms | 1.8 ±0.2ms |
| 14_delete | 0.694 ±0.018ms | 2.2 ±0.3ms | 2.7 ±0.2ms | 1.9 ±0.4ms | 2.3 ±1.0ms |
| 15_join_local_filter | 0.55 ±0.07ms | 3.4 ±0.3ms | 12.9 ±0.8ms | 1.72 ±0.18ms | 12 ±3ms |
| 16_udf_filter | 0.46 ±0.03ms | 2.1 ±0.3ms | error: Catalog Error: Scalar Function with name under does not exist! | 1.41 ±0.15ms | error: Catalog Error: Scalar Function with name under does not exist! |
| 17_lambda | 3.58 ±0.06ms | 17.6 ±0.4ms | 15.9 ±0.4ms | 15.3 ±0.4ms | 5.0 ±0.3ms |

KiB up / down from the client for one untimed run, counted at the TCP layer, framing included.

| query | n6k | quack |
| --- | --- | --- |
| 1_point | 2 / <1 | <1 / <1 |
| 2_limit | 1 / <1 | <1 / <1 |
| 3_topn | 1 / <1 | <1 / <1 |
| 4_groupby | 2 / 2 | <1 / 2 |
| 5_join | 3 / 16 | <1 / 15 |
| 6_scan | 2 / <1 | <1 / <1 |
| 7_join_catalogs | 3 / 16 | n/a |
| 8_join_servers | 3 / 19 | 11 / 15,655 |
| 9_join_local | 16 / 15,988 | 12 / 15,654 |
| 10_in_subquery | 4 / 1 | <1 / <1 |
| 11_view | 3 / <1 | 11 / 7,833 |
| 12_insert | 2 / <1 | <1 / <1 |
| 13_update | 2 / <1 | <1 / <1 |
| 14_delete | 1 / <1 | <1 / <1 |
| 15_join_local_filter | 2 / 17 | 11 / 15,653 |
| 16_udf_filter | 1 / 9 | - |
| 17_lambda | 17 / 8,032 | <1 / <1 |

## Throughput

Without perms, median ± sample standard deviation of 5 runs. The connections
test uses table `a` at 1,000,000 rows. Client and server share one machine.

| | native | n6k | quack |
| --- | --- | --- | --- |
| large pull, 5M rows | 138 ±9M rows/s | 36.7 ±0.5M rows/s | 80 ±17M rows/s |
| bulk insert, 1M rows | 760 ±30M rows/s | 60.1 ±1.9M rows/s | 147 ±19M rows/s |
| 16 connections, 20 group by each | 153 ±6ms | 240 ±30ms | 253 ±12ms |

## 10,000,000 rows

Median ± sample standard deviation of 5 runs of the client-side profiled `query.total_time`.

| query | native | n6k perms | quack perms | n6k no perms | quack no perms |
| --- | --- | --- | --- | --- | --- |
| 1_point | 0.17 ±0.04ms | 1.9 ±0.4ms | 2.71 ±0.11ms | 1.0 ±0.2ms | 1.7 ±0.2ms |
| 2_limit | 0.12 ±0.04ms | 1.52 ±0.18ms | 2.2 ±0.3ms | 0.91 ±0.11ms | 2.9 ±1.0ms |
| 3_topn | 2.89 ±0.12ms | 5.0 ±0.2ms | 5.4 ±0.3ms | 4.0 ±0.3ms | 4.34 ±0.08ms |
| 4_groupby | 8.65 ±0.13ms | 8.8 ±0.8ms | 7.4 ±0.5ms | 11 ±5ms | 10.3 ±1.0ms |
| 5_join | 0.61 ±0.10ms | 2.9 ±0.4ms | 3.1 ±0.3ms | 1.7 ±0.7ms | 2.0 ±0.2ms |
| 6_scan | 1.57 ±0.14ms | 3.2 ±0.3ms | 3.93 ±0.19ms | 3.0 ±0.3ms | 3.70 ±0.13ms |
| 7_join_catalogs | 0.56 ±0.10ms | 3.2 ±0.4ms | n/a | 2.10 ±0.15ms | n/a |
| 8_join_servers | 0.53 ±0.08ms | 3.4 ±0.4ms | 83 ±9ms | 2.2 ±0.3ms | 34 ±13ms (1/5 failed: IO Error: Failed to send message: IO Error: Transferred a partial file error for HTTP POST to 'http://localhost:8084/quack') |
| 9_join_local | 6.2 ±0.3ms | 189 ±10ms | 135.4 ±1.8ms | 179 ±7ms | 103 ±4ms |
| 10_in_subquery | 7.0 ±0.3ms | 9.8 ±0.5ms | 9.6 ±0.5ms | 17.4 ±0.5ms | 8.54 ±0.12ms |
| 11_view | 0.44 ±0.04ms | 2.7 ±0.5ms | 70 ±3ms | 1.46 ±0.15ms | 22 ±8ms |
| 12_insert | 2.28 ±0.10ms | 5.4 ±0.5ms | 6.4 ±1.3ms | 4.01 ±0.20ms | 3.7 ±0.3ms |
| 13_update | 0.54 ±0.06ms | 2.1 ±0.4ms | 2.5 ±0.4ms | 1.4 ±0.2ms | 1.86 ±0.19ms |
| 14_delete | 0.71 ±0.04ms | 3.0 ±0.5ms | 3.2 ±0.4ms | 2.4 ±0.2ms | 2.7 ±0.4ms |
| 15_join_local_filter | 0.58 ±0.07ms | 2.6 ±0.3ms | 80 ±6ms | 1.8 ±0.2ms | 37 ±10ms |
| 16_udf_filter | 0.46 ±0.04ms | 2.1 ±0.7ms | error: Catalog Error: Scalar Function with name under does not exist! | 1.5 ±0.3ms | error: Catalog Error: Scalar Function with name under does not exist! |
| 17_lambda | 41 ±3ms | 133 ±8ms | 131.6 ±0.8ms | 106 ±14ms | 35.72 ±0.14ms |

KiB up / down from the client for one untimed run, counted at the TCP layer, framing included.

| query | n6k | quack |
| --- | --- | --- |
| 1_point | 2 / <1 | <1 / <1 |
| 2_limit | 1 / <1 | <1 / <1 |
| 3_topn | 1 / <1 | <1 / <1 |
| 4_groupby | 2 / 2 | <1 / 2 |
| 5_join | 3 / 16 | <1 / 15 |
| 6_scan | 2 / <1 | <1 / <1 |
| 7_join_catalogs | 3 / 16 | n/a |
| 8_join_servers | 3 / 19 | 31 / 156,474 |
| 9_join_local | 164 / 159,902 | 30 / 156,472 |
| 10_in_subquery | 4 / 1 | <1 / <1 |
| 11_view | 3 / <1 | 31 / 78,275 |
| 12_insert | 2 / <1 | <1 / <1 |
| 13_update | 2 / <1 | <1 / <1 |
| 14_delete | 1 / <1 | <1 / <1 |
| 15_join_local_filter | 2 / 17 | 31 / 156,472 |
| 16_udf_filter | 1 / 9 | - |
| 17_lambda | 164 / 80,328 | <1 / <1 |

## Where each query runs

"server" means the plan is one remote operation and the server sends back only the
result. "client and server" means the client does part of the work. Taken from the
`EXPLAIN` plans in `plans/`, with perms, at the last row count.

| query | n6k | quack |
| --- | --- | --- |
| 1_point | client and server | server |
| 2_limit | client and server | server |
| 3_topn | client and server | server |
| 4_groupby | client and server | server |
| 5_join | client and server | server |
| 6_scan | client and server | server |
| 7_join_catalogs | client and server | n/a |
| 8_join_servers | client and server | client and server |
| 9_join_local | client and server | client and server |
| 10_in_subquery | client and server | server |
| 11_view | client and server | client and server |
| 12_insert | client and server | server |
| 13_update | client and server | server |
| 14_delete | client and server | server |
| 15_join_local_filter | client and server | client and server |
| 16_udf_filter | client and server | fails |
| 17_lambda | client and server | server |
