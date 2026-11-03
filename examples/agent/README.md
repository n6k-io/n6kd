# Let an AI agent query it

`n6kd mcp` gives an AI agent the same access as a DuckDB client, over MCP. The
server applies the same `perms.yml` to the agent as to anyone else.

## Before you start

- Run the [quickstart](../quickstart/README.md) server.
- An MCP client, for example [Claude Code](https://claude.com/claude-code).

## Setup

List the server in `~/.n6kd/mcp.yml`. `ws://localhost:8080/ws` is the same
server as `n6k://localhost:8080`:

```yaml
servers:
  nyc:
    url: ws://localhost:8080/ws
```

Store its key:

```sh
n6kd auth login --server ws://localhost:8080/ws --api-key super-secret
```

Register `n6kd mcp` with your agent:

```sh
claude mcp add n6kd -- n6kd mcp
```

The agent gets these tools: `list_servers`, `add_server`, `remove_server`,
`list_catalogs`, `attach`, `query` and `execute`.

## Ask

> How close is NYC subway ridership to pre-pandemic levels? Open a review of
> JFK Airport in ops.zone_reviews with what you find.

From a Claude Code run, condensed:

```
list_catalogs → nyc.geo, nyc.ops, nyc.transit
attach transit, ops, geo
query   SELECT * FROM geo.main.zones WHERE Zone ILIKE '%JFK%'
        → 132, Queens, JFK Airport, Airports
query   SELECT year(date) AS y, round(avg(subways_of_comparable_pre_pandemic_day), 3) AS avg_pct, ...
        → 2022 0.613, 2023 0.7, 2024 0.724, 2025 0.752
execute INSERT INTO ops.main.zone_reviews (id, location_id, note, status)
        VALUES (1, 132, 'NYC subway ridership recovery ... 2024 avg 72% of comparable pre-pandemic day ...', 'open')
```

A write that `perms.yml` forbids comes back to the agent as a tool error. Asked
to "delete every review in ops.zone_reviews":

```
execute DELETE FROM ops.main.zone_reviews
        → Permission Error: crossing: 'zone_reviews' does not have 'delete' permission
```

The agent reported the refusal and changed nothing.
