# Give each client its own rows

With `--oidc-issuer-url`, each client signs in with a JWT, and the token's
claims can be used in `perms.yml` as `$name`. One table, one policy, and each
client sees and edits only its own rows.

This is a pattern, not a runnable demo: it needs an OIDC issuer you control
whose tokens carry an `email` claim and an `aud` of `n6kd`.

[`boot.sql`](boot.sql) creates two reviews, owned by `ada@example.com` and
`grace@example.com`. [`perms.yml`](perms.yml):

```yaml
catalogs:
  ops:
    tables:
      main.zone_reviews:
        select: "owner = $email"
        insert: "owner = $email"
        update: "owner = $email"
        delete: "owner = $email"
```

## Serve

From this directory:

```sh
n6kd -f boot.sql --perms perms.yml \
  --oidc-issuer-url https://issuer.example.com --oidc-audience n6kd
```

## Query

Set up the client as in the [quickstart](../quickstart/README.md#2-connect),
then pass the JWT as the ATTACH token:

```sql
ATTACH 'n6k://localhost:8080' AS ops (TYPE n6k, CATALOG 'ops', TOKEN '<ada jwt>');

SELECT id, note FROM ops.main.zone_reviews;
-- 1  Check JFK pickup wait times
```

With Grace's token the same query returns row 2 only.

A token that fails signature, `iss`, `aud` or `exp` checks is refused as
`unauthorized`. A token without an `email` claim is refused at connect.
