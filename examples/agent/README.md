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

The agent lists the catalogs, attaches the ones it needs, and queries them.
A write that `perms.yml` forbids comes back to the agent as a tool error, with
the same message a DuckDB client gets.
