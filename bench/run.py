#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["duckdb==1.5.6"]
# ///
"""Usage: [ROWS=] [N=] [THREADS=] [N6KD=] [N6K_CLIENT=] bench/run.py

Relative paths resolve from bench/.
"""

import json
import math
import os
import re
import socket
import statistics
import subprocess
import sys
import tempfile
import time
from contextlib import ExitStack
from pathlib import Path

import duckdb

BENCH = Path(__file__).resolve().parent
os.chdir(BENCH)
N6KD = os.environ.get("N6KD", "../n6kd")
ROWS = int(os.environ.get("ROWS", "1000000"))
N = int(os.environ.get("N", "5"))
# Each quack scan thread opens its own connection, and quack's server queues only
# cpp-httplib's CPPHTTPLIB_LISTEN_BACKLOG of them; more threads than that and
# connects fail. --threads applies only under --lockdown, so it is passed.
THREADS = int(os.environ.get("THREADS", "4"))
N6K_CLIENT = os.environ.get("N6K_CLIENT", "n6k_client")

SCHEMA = Path("schema.sql").read_text()
SET_ROWS = f"SET VARIABLE rows = {ROWS};"
SERVER = ["--threads", str(THREADS), "--lockdown"]
BOOT = ["-c", f"{SET_ROWS} ATTACH ':memory:' AS app; USE app;", "-f", "schema.sql", *SERVER]

# name, port, n6kd args, client attach
TARGETS = {
    "n6k": (8081, [*BOOT, "--perms", "perms.yml"], "n6k"),
    "quack": (8082, ["quack", *BOOT, "--perms", "perms.yml", "--token", "bench"], "quack"),
    "n6k_noperms": (8083, BOOT, "n6k"),
    # no-perms quack serves only the main database
    "quack_noperms": (8084, ["quack", "-c", SET_ROWS, "-f", "schema.sql", *SERVER, "--token", "bench"], "quack"),
}
COLUMNS = ["local", *TARGETS]
HEADERS = ["native", "n6k perms", "quack perms", "n6k no perms", "quack no perms"]
TABLES = ["r.a", "r.b"]


def log(msg):
    print(f"{time.strftime('%T')} {msg}", file=sys.stderr, flush=True)


def check_version():
    mod = Path("../go.mod").read_text()
    v = int(re.search(r"github\.com/duckdb/duckdb-go/v2 v2\.(\d+)", mod)[1])
    want = f"{v // 10000}.{v // 100 % 100}.{v % 100}"
    if duckdb.__version__ != want:
        sys.exit(f"duckdb is {duckdb.__version__}; n6kd embeds {want}")


def listening(port):
    with socket.socket() as s:
        return s.connect_ex(("localhost", port)) == 0


def start_servers(stack):
    for name, (port, args, _) in TARGETS.items():
        if listening(port):
            sys.exit(f":{port} is already in use")
        out = stack.enter_context(open(f"{name}.log", "w"))  # noqa: SIM115
        proc = subprocess.Popen([N6KD, *args, "--addr", f"localhost:{port}"], stdout=out, stderr=out)
        stack.callback(proc.wait)
        stack.callback(proc.terminate)
        while not listening(port):
            if proc.poll() is not None:
                sys.exit(f"server on :{port} exited; see bench/{name}.log")
            time.sleep(0.2)


def attach_sql(column):
    threads = f"SET threads = {THREADS};"
    if column == "local":
        # the same data in-process, as the reference each target's results must match
        return f"{threads} {SET_ROWS} ATTACH ':memory:' AS r; USE r; {SCHEMA}"
    port, _, kind = TARGETS[column]
    if kind == "n6k":
        return f"{threads} LOAD '{N6K_CLIENT}'; ATTACH 'n6k://localhost:{port}' AS r (TYPE n6k, CATALOG 'app');"
    return f"{threads} LOAD quack; CREATE SECRET (TYPE quack, TOKEN 'bench'); ATTACH 'quack:localhost:{port}' AS r;"


def connect(column, pre=""):
    con = duckdb.connect(config={"allow_unsigned_extensions": True})
    if pre:
        con.execute(pre)
    con.execute(attach_sql(column))
    return con


def is_write(query):
    return query.lstrip().upper().startswith(("UPDATE", "DELETE", "INSERT"))


def fingerprint(con, query):
    """A sanity check, not a full comparison: row counts and order-independent
    hashes. A LIMIT without ORDER BY may return any rows, so only its count is
    compared. A write is compared by its affected-row count and the tables'
    state after it."""

    def digest(sql):
        return con.execute(f"SELECT count(*), sum(hash(t)::HUGEINT) FROM ({sql}) t").fetchone()

    query = query.rstrip().rstrip(";")
    if is_write(query):
        return (con.execute(query).fetchone(), *(digest(f"FROM {t}") for t in TABLES))
    if "LIMIT" in query and "ORDER BY" not in query:
        return con.execute(f"SELECT count(*) FROM ({query})").fetchone()
    return digest(query)


def error_line(e):
    return str(e).splitlines()[0]


def fmt_ms(times):
    """median ± sample standard deviation, the std rounded to 1 significant
    figure (2 when it starts with a 1) and the median to the same place. For an
    even N the median is the lower middle run."""
    ms = sorted(t * 1000 for t in times)
    med = ms[(len(ms) - 1) // 2]
    sd = statistics.stdev(ms) if len(ms) > 1 else 0
    if sd == 0:
        return f"{med:.2f}ms"
    e = math.floor(math.log10(sd))
    if 10**e > sd:
        e -= 1
    elif 10 ** (e + 1) <= sd:
        e += 1
    place = e - (int(sd / 10**e) == 1)
    dp = max(0, -place)
    return f"{round(med, -place):.{dp}f} ±{round(sd, -place):.{dp}f}ms"


def measure(con, query, profile):
    """The profiled latency of N runs, or the error when every run failed. An IO
    error or a run that fails where others succeeded exits the bench."""
    con.execute(f"PRAGMA enable_profiling = 'json'; PRAGMA profiling_output = '{profile}';")
    times, errors = [], []
    for _ in range(N):
        try:
            con.execute(query).fetchall()
            times.append(json.loads(Path(profile).read_text())["latency"])
        except duckdb.IOException:
            raise
        except duckdb.Error as e:
            errors.append(e)
    con.execute("PRAGMA disable_profiling")
    if errors and times:
        sys.exit(f"{len(times)}/{N} runs ok: {error_line(errors[0])}")
    if errors:
        return f"error: {error_line(errors[0])}"
    return fmt_ms(times)


def transfer(column, query):
    """KiB sent / received by the client for one run of the query, attach
    excluded, from the client's own log: WS for n6k (n6k frames, without
    WebSocket framing), HTTP for quack (request body length and response
    Content-Length, so a response without that header counts as 0). Its own
    untimed run, since logging may cost time. Logging must be on before the
    attach: a connection the attach opens logs only to the log storage current
    then, and enable_logging and truncate_duckdb_logs replace it. So the
    attach's traffic is subtracted instead."""
    if TARGETS[column][2] == "n6k":
        kind = "WS"
        sent = "sum(bytes) FILTER (direction = 'send')"
        recv = "sum(bytes) FILTER (direction = 'recv')"
    else:
        kind = "HTTP"
        sent = "sum(request.request_body_length)"
        recv = "sum(response.headers['Content-Length']::BIGINT)"
    totals = f"SELECT coalesce({sent}, 0), coalesce({recv}, 0) FROM duckdb_logs_parsed('{kind}')"
    try:
        con = connect(column, pre=f"CALL enable_logging('{kind}', storage = 'memory');")
        before = con.execute(totals).fetchone()
        con.execute(query).fetchall()
        after = con.execute(totals).fetchone()
    except duckdb.IOException:
        raise
    except duckdb.Error:
        return "-"

    def kib(n):
        return "<1" if n < 1024 else f"{round(n / 1024):,}"

    return f"{kib(after[0] - before[0])} / {kib(after[1] - before[1])}"


def explain(con, query):
    try:
        return "\n".join(row[1] for row in con.execute(f"EXPLAIN {query}").fetchall())
    except duckdb.Error as e:
        return str(e)


def main():
    check_version()
    Path("plans").mkdir(exist_ok=True)
    profile = Path(tempfile.mkdtemp()) / "profile.json"
    with ExitStack() as stack:
        log(f"starting servers with {ROWS} rows")
        start_servers(stack)
        print(f"| query | {' | '.join(HEADERS)} |")
        print(f"| --- |{' --- |' * len(HEADERS)}")
        bytes_rows = []
        for file in sorted(Path("queries").glob("*.sql")):
            name, query = file.stem, file.read_text().strip()
            want = fingerprint(connect("local"), query)
            row, bytes_row = [name], [name]
            for column in COLUMNS:
                log(f"{name} {column}")
                con = connect(column)
                Path(f"plans/{column}-{name}.txt").write_text(explain(con, query))
                if column in ("n6k", "quack"):
                    bytes_row.append(transfer(column, query))
                cell = measure(con, query, profile)
                if column != "local" and not cell.startswith("error"):
                    try:
                        if fingerprint(con, query) != want:
                            cell += " ✗ result differs"
                    except duckdb.Error as e:
                        cell += f" ✗ check failed: {error_line(e)}"
                row.append(cell)
            print(f"| {' | '.join(row)} |", flush=True)
            bytes_rows.append(f"| {' | '.join(bytes_row)} |")
    print("\nKiB up / down from the client, one run\n")
    print("| query | n6k | quack |")
    print("| --- | --- | --- |")
    print("\n".join(bytes_rows))


if __name__ == "__main__":
    main()
