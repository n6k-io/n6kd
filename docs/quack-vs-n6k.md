# Quack vs n6k

## Simple answer

Both connect a DuckDB client to a remote DuckDB server. They differ in what they send:

- **n6k** splits a query operator by operator and sends plans. It moves less data and handles client-side references better.
- **Quack** sends SQL text, all-or-nothing. It's simpler and has higher throughput, but it falls back to moving whole columns and sends some client-side references it can't resolve.

Mixed queries, which use both client and server data or more than one server, are where quack's fallback costs it.

## More detail

### Query splitting

- **n6k:** plans on the client and sends the server every part it can run.
- **Quack:** sends the whole statement if everything is on the server. Otherwise it scans the remote tables and does the filtering and joining on the client.

### Data moved

On a join across two servers at 10M rows:

- **n6k:** transfers 19 KiB, because its filters and joins mostly run on the server.
- **Quack:** transfers 156 MB.

### Client-side references

- **n6k:** keeps anything the server can't run on the client.
- **Quack:** evaluates what it can to literals, but sends parameters and UDFs to the server as text, where they fail.

### Throughput

Quack is faster for bulk data:

| | n6k | Quack |
| --- | --- | --- |
| Pull | 38M rows/s | 85M rows/s |
| Insert | 22M rows/s | 140M rows/s |

The likely reason is how each one sends data, though neither has been profiled:

- **n6k:** encodes to Arrow and buffers writes.
- **Quack:** sends native DuckDB chunks in parallel.

### Updates and deletes

When everything is on the server, quack is faster. At 10M rows without perms, an update takes 2.0ms on quack and 10.3ms on n6k.

- **n6k:** runs a key-based self-join that slows as the table grows.
- **Quack:** sends plain SQL.

When the statement uses anything not on the server:

- **n6k:** can update, delete or merge using it.
- **Quack:** can't.

### Isolation and security

- **n6k:** with perms, gives each client its own DuckDB instance, with identity and row rules.
- **Quack:** shares one instance across clients and only provides a token and an authorization hook.

## Limitations

**Core** limitations can be reduced but not removed. **Current** ones are fixable. This split is a judgment from the design, and none of the fixes have been scoped or tested.

### n6k: core

- **Pushdown is limited to what n6k can translate:** anything it can't express runs on the client and pulls data.
- **Updates and deletes using client data need a way to identify rows:** the server has to match the client's rows to its own, by key or something equivalent.

### n6k: current

- **DDL:** add the missing operations to the protocol.
- **Table definitions:** pass `CHECK`, foreign keys, defaults and generated columns through to the server.
- **Fully remote updates and deletes:** run them as direct filters on the server, removing the key requirement and the self-join.
- **`MERGE` and `RETURNING`:** run them on the server as one statement when all the data is there.
- **Writes:** stream batches instead of buffering the whole payload.
- **Session state:** keep a server session across requests so temp tables persist.

### Quack: core

- **The server only sees SQL text:** anything that exists only on the client must become a literal or stay on the client.
- **The wire format is DuckDB's own:** native chunks and DuckDB's binary serialization tie both ends to compatible DuckDB versions.
- **HTTP has no server push:** each message is one request and response, so the server can't announce changes such as another client's DDL.
- **Clients share one DuckDB instance:** catalogs, attached databases, secrets and global settings are shared. Per-client isolation would need a host running several instances.

### Quack: current

- **Scan pushdown:** enable filter pushdown, which is commented out, to cut the data pulled by mixed queries.
- **All-or-nothing pushdown:** send more of a mixed query to the server, beyond the fully remote subqueries it already sends separately.
- **Parameters:** add a parameter slot to the protocol.
- **UDFs:** detect client-only functions and run them locally, instead of failing on the server.
- **Updates, deletes and merges using client data:** implement the stubbed paths. These would still need a way to identify rows, as in n6k.
- **`RETURNING` from a local source:** implement it for inserts.
- **One statement per connection:** allow several, so one plan can have two scans, or a scan plus an insert, on the same server.
- **Identity and row rules:** quack leaves these to the host, and could add them through its authorization hook. That hook only sees SQL text, so this is harder than in n6k.
- **Transport retries:** `retries = 0` means a single transport fault fails the query.
