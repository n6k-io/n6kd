# Threat model

This file defines n6kd's threat model. Protocol and bridge guarantees live in
`duckdb-n6k-protocol` and `duckdb-virtual-catalog`.

## Trusted

- **Host.** Runs n6kd: boot SQL, perms.yml, flags, environment.

## Attacker

- **Client.** Sends any frame, SQL and token, over any number of connections.

## Boundary

- **Bridged (`--perms`).** Each client gets its own database; perms.yml grants are the boundary.
- **Direct and quack.** Clients share one database and reach each other.

## n6kd must

- Serve nothing until the token is accepted.
- Never hand a client the source database or the environment.
- Let a client disrupt only its own connection, never another client's.

## Accepted leaks

- **SQL error text** reaches the client.
