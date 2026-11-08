# Keycloak setup

[`realm.json`](realm.json) creates a realm `n6k` with a public client
`n6k-cli` that signs users in with device login. Its tokens carry `aud: n6kd`
and each user's `region` attribute as a `region` claim.

The admin console is at http://localhost:8081 (`admin` / `admin`).

## Add a user

1. Switch to the `n6k` realm, then **Users → Add user**.
2. Set a username and email.
3. Under **Attributes**, add `region`.
4. Under **Credentials**, set a password with **Temporary** off.

## Issuer

Always log in through `localhost:8081`. Keycloak sets the token's `iss` from
the host it is called on, and n6kd refuses a token whose `iss` differs from
`--oidc-issuer-url`.
