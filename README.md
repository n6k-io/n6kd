# n6kd

Serves DuckDB catalogs over the n6k protocol via WebSocket.

## Build

```sh
go build -o n6kd .
n6kd install   # download the n6k DuckDB extensions
```

To run against local extension builds instead, point `N6K_EXTENSION_DIR` at
their `build/release/extension` directories as an OS path list (`:` on Unix,
`;` on Windows); each is searched
for `<name>/<name>.duckdb_extension`. `vcatbridge` comes from
`duckdb-virtual-catalog`, `n6k_client` and `n6k_server` from `duckdb-n6k-protocol`.
Tests skip when it is unset.

## Run

```sh
n6kd -f schema.sql --perms perms.yml
```

Boot SQL is run in the order given by `-f` (file) and `-c` (statement). Without
`--perms`, every catalog in the loaded database is served read/write, unbridged.

With `--listen=<path>` (or `N6K_LISTEN=<path>`), n6kd listens on that Unix
socket instead of `--addr` and serves every client that connects, each in its
own session, in n6k stream framing. A stale socket file is replaced, one a live
server holds is refused, and the file is mode 0600 and removed on exit. The
dialing parent authenticates; the HELLO token is not checked, and `--api-key` /
`--oidc-*` are refused at startup.

### Flags

| Flag | Description |
| --- | --- |
| `-f, --file` | SQL file run at boot; repeatable |
| `-c, --command` | SQL run at boot; repeatable |
| `--perms` | perms.yml naming the catalogs to serve bridged |
| `--addr` | listen address (default `:8080`, or `$PORT`) |
| `--listen` | Unix socket to listen on instead of `--addr` (`N6K_LISTEN`) |
| `--api-key` | token clients must present (`N6KD_API_KEY`) |
| `--oidc-issuer-url` | OIDC issuer whose JWTs clients must present (`N6KD_OIDC_ISSUER_URL`); not with `--api-key` |
| `--oidc-audience` | `aud` claim a client's JWT must carry (`N6KD_OIDC_AUDIENCE`); required with `--oidc-issuer-url` |
| `--load-dotenv[=path]` | load environment variables from `.env` (or `path`) first; variables already set win |
| `--allow-origin` | browser origins allowed to connect; repeatable, default any (`N6KD_ALLOW_ORIGIN`, comma-separated) |
| `--lockdown` / `--no-lockdown` | deny served connections filesystem and extension access |
| `--memory-limit` / `--threads` | locked in by `--lockdown`, per session database; default DuckDB's (`N6KD_MEMORY_LIMIT`, `N6KD_THREADS`) |
| `--boot-only` | run the boot SQL, then exit without serving |
| `--debug` | per-connection and per-catalog logging |
| `--json-logging` | JSON logs with Cloud Logging severity fields |

### Commands

- `n6kd install` — download the extensions into DuckDB's extension directory
- `n6kd selftest` — bridge a throwaway catalog end to end
- `n6kd quack` — serve over DuckDB's quack protocol instead; see below

## Quack

```sh
n6kd quack -f schema.sql --perms perms.yml --token super_secret
```

```sql
-- from any DuckDB client
INSTALL quack; LOAD quack;
CREATE SECRET (TYPE quack, TOKEN 'super_secret');
ATTACH 'quack:localhost:8080' AS app;
```

A different model from n6k: one database shared by every client and one token,
with no users and no JWT claims. With `--perms`, perms.yml must name exactly one
catalog, served bridged as the main database, since that is all a quack client's
`ATTACH` sees; policies get no `$name` context values. Without
`--perms`, the loaded database is served directly. Quack speaks plain HTTP, so
put TLS in a proxy.

Quack's `ATTACH` cannot run `UPDATE` or `DELETE`; send those through
`quack_query('quack:localhost:8080', '...')`.

Takes `-f`, `-c`, `--perms`, the lockdown flags and `--boot-only` as above, plus:

| Flag | Description |
| --- | --- |
| `--addr` | listen address (default `:8080`, or `$PORT`); a host other than `localhost` listens beyond this machine |
| `--token` | token every client presents, at least 4 characters (`N6KD_QUACK_TOKEN`); generated and logged when empty |

With `N6K_EXTENSION_DIR` set, the local `vcatbridge` build is installed into
DuckDB's extension directory first: the bridged main database loads it by name,
and the source must load that same copy. `quack` and `httpfs` load from there too
(`n6kd install` downloads them).

## perms.yml

```yaml
catalogs:
  app:
    tables:
      main.users:
        primary_key: [id]
        select: true
        insert: true
        update: true
        delete: true

      main.audit_log:
        select: true

      main.orders:
        select: "status <> 'deleted'"
        insert: "amount > 0"
        update: { using: "status = 'open'", check: "amount > 0" }
        delete: "status = 'closed'"
```

Each catalog's value is a `vcatbridge` policy document, passed to
`vcatbridge_configure_policy` as written; `policy_version` defaults to `1`. See the
bridge's `docs/policy-v1.schema.json` for every setting (`schemas`, `functions`,
`foreign_keys`, ...). The bridge validates the document when a client connects,
and an unlisted verb is denied.

| Verb | Value |
| --- | --- |
| `select` | `true` or a `USING` predicate: rows a client can see |
| `insert` | `true` or a `WITH CHECK` predicate: rows a client can add |
| `update` | `true`, a predicate, or `{using, check}`: rows a client can change, and what they may become |
| `delete` | `true` or a `USING` predicate: rows a client can remove |

`update` and `delete` need a primary key. `primary_key` overrides it where the
bridge cannot discover one from the schema.

Each catalog is served under its own name:

```sql
ATTACH 'n6k://localhost:8080' AS eggs (TYPE n6k, CATALOG 'app');
SELECT * FROM eggs.main.users;
```

The alias is the client's to pick; `CATALOG` selects the served catalog. An
unlisted table does not exist for clients.

`memory`, `system`, and `temp` cannot be served — every DuckDB already has
them, so a served catalog cannot take those names. Boot SQL must `ATTACH` a
catalog of its own rather than relying on the default.

### JWT claims

With `--oidc-issuer-url`, a client passes its JWT as the ATTACH token:

```sql
ATTACH 'n6k://localhost:8080' AS app (TYPE n6k, TOKEN '<jwt>');
```

n6kd discovers the issuer's signing keys at boot and checks each token's
signature, `iss`, `aud`, and `exp`. A token that fails is refused as
`unauthorized`.

Every string, boolean, and number claim in the token is available to policy
predicates as `$name` — `$sub`, `$iss`, `$exp`, `$email`, and any custom
claim. Array and object claims are left out, and so is `aud` when it is a list.

```yaml
catalogs:
  app:
    tables:
      main.orders:
        select: "owner = $sub"
        insert: "owner = $sub"
```

Values are bound as typed constants, never spliced into the SQL. A predicate
naming a claim the token does not carry refuses the connection.

## SQL functions

| Function | Description |
| --- | --- |
| `getenv(name)` | environment variable, NULL when unset; only available inside `-f` boot SQL and, with `--perms`, `n6kd_set_interval` bodies |
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
