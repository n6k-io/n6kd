# n6k vs quack: security

Does each boundary hold under `--perms` and `--lockdown`? Each check has a written
expected result, because native DuckDB has no users to compare against. Same-as-native
behavior is in [`compat/`](../compat/README.md), and speed is in
[`bench/`](../bench/README.md).

Results are in [results.md](results.md).

## Run it yourself

```sh
go build -o n6kd . && ./n6kd install
security/run.py > security/results.md
```

To use local extension builds, set `N6K_EXTENSION_DIR` (see the top-level README),
in the environment or in the repo's `.env`. The `quack` extension must already be
installed for the DuckDB version in `run.py`. Ports 8151-8154 must be free.

The JWT check signs its own tokens with a key generated for the run, served by a
local OIDC issuer.
