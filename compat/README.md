# n6k vs quack vs native: behavior

Does a statement run over n6k or quack give the same result, or the same exception
class, as running it on native DuckDB? Speed and bytes are in
[`bench/`](../bench/README.md).

Results are in [results.md](results.md).

## Run it yourself

```sh
go build -o n6kd . && ./n6kd install
compat/run.py > compat/results.md
```

To use local extension builds, set `N6K_EXTENSION_DIR` (see the top-level README),
in the environment or in the repo's `.env`. The `quack` extension must already be
installed for the DuckDB version in `run.py`.

Each case runs in its own process group with a timeout, so a crash or hang fails
only that cell. Ports 8141 and 8142 must be free.
