# Quickstart: share data with row-level rules

n6kd lets other DuckDB users attach your databases over the network. A
`perms.yml` controls which tables they see, which rows come back, and what they
can write. This example runs locally on public NYC data. No account needed.

It serves three databases:

- `geo`: NYC taxi zones
- `transit`: MTA daily ridership
- `ops`: a `zone_reviews` table clients can write to, and a `reviewers` table
  nobody is given

## Before you start

- [Install n6kd](../../README.md#install).
- Install the [DuckDB 1.5.6](https://duckdb.org/install/) CLI. The client
  extension must match that version.
- Run the commands below from this directory.

## 1. Serve

[`boot.sql`](boot.sql) loads the data. [`perms.yml`](perms.yml):

```yaml
catalogs:
  geo:
    tables:
      main.zones:
        select: "Borough = 'Queens'"
  transit:
    tables:
      main.daily_ridership:
        select: true
  ops:
    tables:
      main.zone_reviews:
        select: true
        insert: "status = 'open'"
        update: { using: "status = 'open'", check: "status IN ('open', 'resolved')" }
```

- Only listed tables exist for clients.
- `select` filters rows. Clients see Queens zones only.
- `insert`, `update` and `delete` are each opt-in. `zone_reviews` takes new
  `open` rows, and lets open rows be edited or resolved. It never allows
  deletes.

```sh
n6kd -f boot.sql --perms perms.yml --api-key super-secret
```

## 2. Connect

The client extension is unsigned, so start DuckDB with `duckdb -unsigned`:

```sql
INSTALL n6k_client FROM 'https://storage.googleapis.com/n6k-duckdb-release';
LOAD n6k_client;

ATTACH 'n6k://localhost:8080' AS geo (TYPE n6k, CATALOG 'geo');
-- Invalid Error: InvalidInputException: unauthorized
```

Store the key once as a secret scoped to the server. Every attach to that
server presents it:

```sql
CREATE SECRET (TYPE n6k, TOKEN 'super-secret', SCOPE 'n6k://localhost:8080');

ATTACH 'n6k://localhost:8080' AS geo     (TYPE n6k, CATALOG 'geo');
ATTACH 'n6k://localhost:8080' AS transit (TYPE n6k, CATALOG 'transit');
ATTACH 'n6k://localhost:8080' AS ops     (TYPE n6k, CATALOG 'ops');
```

## 3. Read

```sql
SELECT DISTINCT Borough FROM geo.main.zones;
-- Queens

SELECT count(*) FROM geo.main.zones WHERE Borough = 'Manhattan';
-- 0

SELECT LocationID, Zone FROM geo.main.zones WHERE Zone LIKE '%Airport';
-- 132  JFK Airport
-- 138  LaGuardia Airport

SELECT max(date)::DATE, round(avg(subways_of_comparable_pre_pandemic_day), 2)
FROM transit.main.daily_ridership WHERE date >= DATE '2025-01-01';
-- 2025-01-09  0.75
```

Unlisted tables do not exist:

```sql
SELECT * FROM ops.main.reviewers;
-- Catalog Error: Table with name reviewers does not exist!
```

## 4. Write

```sql
INSERT INTO ops.main.zone_reviews VALUES
  (1, 132, 'Check JFK pickup wait times', 'open'),
  (2, 138, 'Check LaGuardia pickup wait times', 'open');

UPDATE ops.main.zone_reviews SET status = 'resolved' WHERE id = 1;
```

Writes that break the rules are refused:

```sql
INSERT INTO ops.main.zone_reviews VALUES (3, 1, 'Newark review', 'resolved');
-- Invalid Input Error: ... row violates the check predicate on this table

UPDATE ops.main.zone_reviews SET status = 'archived' WHERE id = 2;
-- Invalid Input Error: ... row violates the check predicate on this table

UPDATE ops.main.zone_reviews SET note = 'edited' WHERE id = 1;
-- succeeds, 0 rows: resolved reviews fall outside `using`

DELETE FROM ops.main.zone_reviews;
-- Permission Error: ... 'zone_reviews' does not have 'delete' permission
```

Databases from one server join like local ones:

```sql
SELECT r.id, z.Zone, r.status
FROM ops.main.zone_reviews r JOIN geo.main.zones z ON z.LocationID = r.location_id;
-- 1  JFK Airport        resolved
-- 2  LaGuardia Airport  open
```

## Next

- [Let an AI agent query it](../agent/README.md)
- [Give each client its own rows](../per-client/README.md)
- [Combine BigQuery and local data](../multi-source/README.md)
