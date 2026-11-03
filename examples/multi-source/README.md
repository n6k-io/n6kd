# Combine BigQuery and local data in one server

One n6kd process serves a BigQuery dataset next to the
[quickstart](../quickstart/README.md)'s CSV and in-memory catalogs. Clients
join across all of them as if they were local. The BigQuery credentials stay
on the server, and clients only need the n6kd key.

## Before you start

- The [quickstart](../quickstart/README.md). This example reuses its
  `boot.sql`, client setup and write rules.
- A Google Cloud project to bill, and application default credentials:
  `gcloud auth application-default login`.
- Queries scan a public BigQuery table, billed to your project.

## Serve

[`boot.sql.template`](boot.sql.template) adds a `taxi` catalog on top of the
quickstart's `geo`, `transit` and `ops`. [`perms.yml`](perms.yml) covers all
four, and adds one rule for `taxi`:

```yaml
  taxi:
    tables:
      new_york_taxi_trips.tlc_yellow_trips_2022:
        select: "pickup_location_id IN ('132','138')"
```

Clients see JFK (132) and LaGuardia (138) pickups only, and no other table in
the dataset.

From this directory:

```sh
BQ_BILLING_PROJECT=your-project n6kd -f ../quickstart/boot.sql -t boot.sql.template \
  --perms perms.yml --api-key super-secret
```

## Query across sources

Connect as in the [quickstart](../quickstart/README.md#2-connect), then attach
all three read catalogs:

```sql
ATTACH 'n6k://localhost:8080' AS taxi    (TYPE n6k, CATALOG 'taxi');
ATTACH 'n6k://localhost:8080' AS geo     (TYPE n6k, CATALOG 'geo');
ATTACH 'n6k://localhost:8080' AS transit (TYPE n6k, CATALOG 'transit');
```

Daily airport pickups from BigQuery, named from the zones CSV, next to subway
ridership:

```sql
SELECT t.day, z.Zone, t.pickups, r.subways_total_estimated_ridership AS subway
FROM (
  SELECT pickup_datetime::DATE AS day, pickup_location_id, count(*) AS pickups
  FROM taxi.new_york_taxi_trips.tlc_yellow_trips_2022
  GROUP BY ALL
) t
JOIN geo.main.zones z ON z.LocationID = t.pickup_location_id::INTEGER
JOIN transit.main.daily_ridership r ON r.date::DATE = t.day
ORDER BY t.day, z.Zone;
```

The filter holds on every source:

```sql
SELECT count(*) FROM taxi.new_york_taxi_trips.tlc_yellow_trips_2022
WHERE pickup_location_id = '161';  -- Midtown Center
-- 0

SELECT count(*) FROM taxi.new_york_taxi_trips.tlc_green_trips_2022;
-- Catalog Error: Table with name tlc_green_trips_2022 does not exist!
```

Record a finding in `ops` under the quickstart's write rules:

```sql
ATTACH 'n6k://localhost:8080' AS ops (TYPE n6k, CATALOG 'ops');

INSERT INTO ops.main.zone_reviews
VALUES (1, 132, 'Compare JFK pickups with subway ridership', 'open');
```
