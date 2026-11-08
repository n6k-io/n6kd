# Sign real users in with Keycloak

Users sign in with Keycloak, and n6kd shows each one only the rows their
token allows. Here a sales team shares one `deals` table, and each rep sees
only their own region.

## Before you start

- [Install n6kd](../../README.md#install).
- Install the [DuckDB 1.5.6](https://duckdb.org/install/) CLI and Docker. The
  client extension must match that version.

Run the commands below from this directory.

## 1. Start Keycloak

```sh
docker run --rm -p 8081:8080 \
  -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin \
  -v "$PWD/realm.json:/opt/keycloak/data/import/realm.json" \
  quay.io/keycloak/keycloak start-dev --import-realm
```

[`realm.json`](realm.json) creates the `n6k` realm, the `n6k-cli` client, and
two users, password `pw`:

| User | `region` |
| --- | --- |
| `grace` | `east` |
| `linus` | `west` |

## 2. Serve

[`boot.sql`](boot.sql) loads the files in `data/` into a `deals` table in the
`sales` catalog. A token's claims can be used in `perms.yml` as `$name`, so
[`perms.yml`](perms.yml) filters on `$region`, the signed-in user's region:

```yaml
catalogs:
  sales:
    tables:
      main.deals:
        select: "region = $region"
```

```sh
n6kd -f boot.sql --perms perms.yml \
  --oidc-issuer-url http://localhost:8081/realms/n6k --oidc-audience n6kd \
  --oidc-client-id n6k-cli
```

n6kd accepts only tokens from this realm that carry `aud: n6kd`.
`--oidc-client-id` names the client `n6kd auth login` signs users in with.

## 3. Log in as Grace

```sh
n6kd auth login localhost:8080
# Sign in at http://localhost:8081/realms/n6k/device (code: WDJB-MJHT)
```

Open the link, enter the code, sign in as `grace` / `pw`, and choose **Yes**.

The login is saved as a persistent DuckDB secret for `localhost:8080`, so
`ATTACH` below needs no token.

## 4. Query

The client extension is unsigned, so start DuckDB with `duckdb -unsigned`.
`httpfs` is needed because the client refreshes the saved login with an HTTP
POST:

```sql
INSTALL httpfs;
LOAD httpfs;
INSTALL n6k_client FROM 'https://storage.googleapis.com/n6k-duckdb-release';
LOAD n6k_client;
ATTACH 'n6k://localhost:8080' AS sales (TYPE n6k, CATALOG 'sales');
```

```sql
SELECT region, customer, amount, stage FROM sales.deals;
```

```
east  Acme Corp  12000  open
east  Globex     45000  negotiation
east  Initech     8000  open
east  Umbrella   30000  won
```

Run step 3 again as `linus`, restart DuckDB, and the same query returns only
`west` deals.

## Next

- [Add a user](setup-key-cloak.md#add-a-user).
- Use your own issuer, such as Okta or Auth0: point `--oidc-issuer-url` at it,
  register a public client with device login for `--oidc-client-id`, and have
  its tokens carry `aud: n6kd` and a `region` claim.
