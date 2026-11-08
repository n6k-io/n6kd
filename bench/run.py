#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["duckdb==2.0.0", "numpy"]
# ///
"""Usage: [ROWS=1000000,10000000] [N=] [THREADS=] [N6KD=] [N6K_CLIENT=] bench/run.py > bench/results.md

Relative paths resolve from bench/. N6K_EXTENSION_DIR may come from the repo's .env.
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
import threading
import time
from contextlib import ExitStack
from pathlib import Path

import duckdb

BENCH = Path(__file__).resolve().parent
os.chdir(BENCH)
if Path("../.env").exists():
    for env_line in Path("../.env").read_text().splitlines():
        env_key, _, env_value = env_line.partition("=")
        if env_value and not env_key.startswith("#"):
            os.environ.setdefault(env_key.strip(), env_value.strip())
N6KD = os.environ.get("N6KD", "../n6kd")
SIZES = [int(size) for size in os.environ.get("ROWS", "1000000,10000000").split(",")]
N = int(os.environ.get("N", "5"))
# Each quack scan thread opens its own connection, and quack's server queues only
# cpp-httplib's CPPHTTPLIB_LISTEN_BACKLOG of them; more threads than that and
# connects fail. --threads applies only under --lockdown, so it is passed.
THREADS = int(os.environ.get("THREADS", "4"))


def default_client():
    for directory in os.environ.get("N6K_EXTENSION_DIR", "").split(os.pathsep):
        path = Path(directory) / "n6k_client" / "n6k_client.duckdb_extension"
        if directory and path.exists():
            return str(path.resolve())
    return "n6k_client"


N6K_CLIENT = os.environ.get("N6K_CLIENT") or default_client()

SCHEMA = Path("schema.sql").read_text()
OTHER = Path("other.sql").read_text()
PEER = Path("peer.sql").read_text()
BIG = Path("big.sql").read_text()
SERVER = ["--threads", str(THREADS), "--lockdown"]
ATTACH_APP = ["-c", "ATTACH ':memory:' AS app; USE app;"]
PEER_N6K = [*ATTACH_APP, "-f", "peer.sql", *SERVER]
SET_ROWS = ""
TARGETS = {}


def configure(rows):
    """Sets the row count the servers boot with and the local copy is built with."""
    global SET_ROWS
    SET_ROWS = f"SET VARIABLE rows = {rows};"
    boot = ["-c", f"{SET_ROWS} ATTACH ':memory:' AS app; USE app;", "-f", "schema.sql", *SERVER]
    n6k_boot = [*boot, "-f", "other.sql"]
    # no-perms quack serves only the main database
    quack_noperms = ["quack", "-c", SET_ROWS, "-f", "schema.sql", "-f", "big.sql", *SERVER, "--token", "bench"]
    # name: port, n6kd args, client kind, peer port, peer args
    TARGETS.update(
        {
            "n6k": (8081, [*n6k_boot, "--perms", "perms.yml"], "n6k", 8085, [*PEER_N6K, "--perms", "peer_perms.yml"]),
            "quack": (
                8082,
                ["quack", *boot, "--perms", "quack_perms.yml", "--token", "bench"],
                "quack",
                8086,
                ["quack", *ATTACH_APP, "-f", "peer.sql", *SERVER, "--perms", "peer_perms.yml", "--token", "bench"],
            ),
            "n6k_noperms": (8083, [*n6k_boot, "-f", "big.sql"], "n6k", 8087, PEER_N6K),
            "quack_noperms": (8084, quack_noperms, "quack", 8088, ["quack", "-f", "peer.sql", *SERVER, "--token", "bench"]),
        }
    )


COLUMNS = ["local", "n6k", "quack", "n6k_noperms", "quack_noperms"]
HEADERS = ["native", "n6k perms", "quack perms", "n6k no perms", "quack no perms"]
TABLES = ["r.a", "r.b", "r.sink"]
QUACK_NOT_APPLICABLE = {"7_join_catalogs"}
SERVER_NODES = {"Crossing Read", "Crossing Fence", "Quack Query By Name"}

THROUGHPUT_COLUMNS = {"local": "native", "n6k_noperms": "n6k", "quack_noperms": "quack"}
PULL_ROWS = 5_000_000
INSERT_ROWS = 1_000_000
CONNECTIONS = 16
QUERIES_PER_CONNECTION = 20
GROUP_BY = "SELECT k, count(*) FROM r.a GROUP BY k"
CLIENT_SQL = """CREATE TABLE l AS SELECT range AS k FROM range(1, 6);
CREATE TEMP VIEW v AS SELECT * FROM r.a WHERE i < 1000;"""


def log(msg):
    print(f"{time.strftime('%T')} {msg}", file=sys.stderr, flush=True)


def check_version():
    mod = Path("../go.mod").read_text()
    v = int(re.search(r"github\.com/duckdb/duckdb-go/v2 v2\.(\d+)", mod)[1])
    want = f"{v // 10000}.{v // 100 % 100}.{v % 100}"
    if duckdb.__version__ != want:
        sys.exit(f"duckdb is {duckdb.__version__}; n6kd embeds {want}")


def listening(port):
    try:
        socket.create_connection(("localhost", port)).close()
    except OSError:
        return False
    return True


def start_server(stack, name, port, args):
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


def start_servers(stack):
    for name, (port, args, _, peer_port, peer_args) in TARGETS.items():
        start_server(stack, name, port, args)
        start_server(stack, f"{name}_peer", peer_port, peer_args)


class CountingProxy:
    """Forwards TCP to a local port and counts payload bytes in each direction, so
    both protocols are measured at the same layer, framing included."""

    def __init__(self, target):
        self.target = target
        self.sent = 0
        self.received = 0
        self.lock = threading.Lock()
        self.listener = socket.create_server(("localhost", 0))
        self.port = self.listener.getsockname()[1]
        threading.Thread(target=self.accept, daemon=True).start()

    def accept(self):
        while True:
            try:
                client, _ = self.listener.accept()
            except OSError:
                return
            server = socket.create_connection(("localhost", self.target))
            for sock in (client, server):
                sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            threading.Thread(target=self.pump, args=(client, server, "sent"), daemon=True).start()
            threading.Thread(target=self.pump, args=(server, client, "received"), daemon=True).start()

    def pump(self, source, destination, counter):
        try:
            while data := source.recv(1 << 16):
                with self.lock:
                    setattr(self, counter, getattr(self, counter) + len(data))
                destination.sendall(data)
        except OSError:
            pass
        finally:
            for sock in (source, destination):
                try:
                    sock.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass

    def totals(self):
        with self.lock:
            return self.sent, self.received

    def close(self):
        self.listener.close()


def connect(column, port=None, peer_port=None):
    """A fresh in-process database attached to the column's target as r, the same
    server's other catalog as o where there is one, and the target's peer server
    as s. port and peer_port replace the target's, for going through a proxy."""
    con = duckdb.connect(config={"allow_unsigned_extensions": True})
    con.execute(f"SET threads = {THREADS};")
    if column == "local":
        # the same data in-process, as the reference each target's results must match
        con.execute(f"{SET_ROWS} ATTACH ':memory:' AS r; USE r; {SCHEMA}")
        con.execute(OTHER.replace("other", "o"))
        con.execute(f"ATTACH ':memory:' AS s; USE s; {PEER}")
        con.execute("USE memory")
    else:
        target_port, _, kind, target_peer_port, _ = TARGETS[column]
        port = port or target_port
        peer_port = peer_port or target_peer_port
        con.execute(f"LOAD '{N6K_CLIENT}';" if kind == "n6k" else "LOAD quack;")
        if kind == "n6k":
            con.execute(f"ATTACH 'n6k://localhost:{port}' AS r (TYPE n6k, CATALOG 'app');")
            con.execute(f"ATTACH 'n6k://localhost:{port}' AS o (TYPE n6k, CATALOG 'other');")
            con.execute(f"ATTACH 'n6k://localhost:{peer_port}' AS s (TYPE n6k, CATALOG 'app');")
        else:
            con.execute("CREATE SECRET (TYPE quack, TOKEN 'bench');")
            con.execute(f"ATTACH 'quack:localhost:{port}' AS r; ATTACH 'quack:localhost:{peer_port}' AS s;")
    con.execute(CLIENT_SQL)
    con.create_function("under", lambda v: v < 500, ["BIGINT"], "BOOLEAN", type="native")
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


def fmt_number(values, unit):
    """median ± sample standard deviation, the std rounded to 1 significant
    figure (2 when it starts with a 1) and the median to the same place. For an
    even N the median is the lower middle run."""
    ordered = sorted(values)
    med = ordered[(len(ordered) - 1) // 2]
    sd = statistics.stdev(ordered) if len(ordered) > 1 else 0
    if sd == 0:
        return f"{med:.2f}{unit}"
    e = math.floor(math.log10(sd))
    if 10**e > sd:
        e -= 1
    elif 10 ** (e + 1) <= sd:
        e += 1
    place = e - (int(sd / 10**e) == 1)
    dp = max(0, -place)
    return f"{round(med, -place):.{dp}f} ±{round(sd, -place):.{dp}f}{unit}"


def fmt_ms(times):
    return fmt_number([t * 1000 for t in times], "ms")


def measure(con, query, profile, reset=None):
    """The profiled latency of the runs that succeeded and how many failed, or
    the error when every run failed."""
    con.execute(f"PRAGMA enable_profiling = 'json'; PRAGMA profiling_output = '{profile}';")
    times, errors = [], []
    for _ in range(N):
        try:
            con.execute(query).fetchall()
            times.append(json.loads(Path(profile).read_text())["query"]["total_time"])
        except duckdb.Error as e:
            errors.append(e)
        if reset:
            con.execute(reset)
    con.execute("PRAGMA disable_profiling")
    if errors and not times:
        return f"error: {error_line(errors[0])}"
    cell = fmt_ms(times)
    return f"{cell} ({len(errors)}/{N} failed: {error_line(errors[0])})" if errors else cell


def transfer(column, query, reset=None):
    """KiB sent / received by the client for one untimed run of the query, attach
    excluded, counted by a TCP proxy in front of the target and its peer."""
    port, _, _, peer_port, _ = TARGETS[column]
    proxies = [CountingProxy(port), CountingProxy(peer_port)]

    def totals():
        counts = [proxy.totals() for proxy in proxies]
        return sum(sent for sent, _ in counts), sum(received for _, received in counts)

    def kib(n):
        return "<1" if n < 1024 else f"{round(n / 1024):,}"

    con = connect(column, proxies[0].port, proxies[1].port)
    try:
        before = totals()
        con.execute(query).fetchall()
        after = totals()
        if reset:
            con.execute(reset)
        return f"{kib(after[0] - before[0])} / {kib(after[1] - before[1])}"
    except duckdb.Error:
        return "-"
    finally:
        con.close()
        for proxy in proxies:
            proxy.close()


def explain(con, query):
    try:
        return "\n".join(row[1] for row in con.execute(f"EXPLAIN {query}").fetchall())
    except duckdb.Error as e:
        return str(e)


def large_pull(column):
    con = connect(column)
    if column == "local":
        con.execute(f"USE r; {BIG} USE memory;")
    rates, errors = [], []
    for _ in range(N):
        start = time.perf_counter()
        try:
            con.execute("CREATE OR REPLACE TEMP TABLE pulled AS SELECT * FROM r.big")
        except duckdb.Error as e:
            errors.append(error_line(e))
            continue
        elapsed = time.perf_counter() - start
        count = con.execute("SELECT count(*) FROM pulled").fetchone()[0]
        if count != PULL_ROWS:
            return f"error: pulled {count} rows"
        rates.append(PULL_ROWS / elapsed / 1e6)
    if not rates:
        return f"error: {errors[0]}"
    cell = fmt_number(rates, "M rows/s")
    return f"{cell} ({len(errors)}/{N} failed: {errors[0]})" if errors else cell


def bulk_insert(column):
    con = connect(column)
    con.execute(f"CREATE TEMP TABLE source AS SELECT range AS i, range AS k FROM range({INSERT_ROWS})")
    rates = []
    for _ in range(N):
        start = time.perf_counter()
        con.execute("INSERT INTO r.sink SELECT * FROM source")
        elapsed = time.perf_counter() - start
        count = con.execute("SELECT count(*) FROM r.sink").fetchone()[0]
        con.execute("DELETE FROM r.sink")
        if count != INSERT_ROWS:
            return f"error: inserted {count} rows"
        rates.append(INSERT_ROWS / elapsed / 1e6)
    return fmt_number(rates, "M rows/s")


def concurrency(column):
    total = CONNECTIONS * QUERIES_PER_CONNECTION
    shared = connect(column) if column == "local" else None
    times = []
    for _ in range(N):
        connections = [shared.cursor() if shared else connect(column) for _ in range(CONNECTIONS)]
        failures = []

        def run(con, failures):
            for _ in range(QUERIES_PER_CONNECTION):
                try:
                    con.execute(GROUP_BY).fetchall()
                except duckdb.Error as e:
                    failures.append(error_line(e))

        threads = [threading.Thread(target=run, args=(con, failures)) for con in connections]
        start = time.perf_counter()
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        times.append(time.perf_counter() - start)
        if failures:
            return f"error: {len(failures)}/{total} failed: {failures[0]}"
    return fmt_ms(times)


def throughput(rows):
    print(f"\n## Throughput\n\nWithout perms, median ± sample standard deviation of {N} runs. The connections")
    print(f"test uses table `a` at {rows:,} rows. Client and server share one machine.\n")
    print(f"| | {' | '.join(THROUGHPUT_COLUMNS.values())} |")
    print(f"| --- |{' --- |' * len(THROUGHPUT_COLUMNS)}")
    tests = {
        f"large pull, {PULL_ROWS // 1_000_000}M rows": large_pull,
        f"bulk insert, {INSERT_ROWS // 1_000_000}M rows": bulk_insert,
        f"{CONNECTIONS} connections, {QUERIES_PER_CONNECTION} group by each": concurrency,
    }
    for name, test in tests.items():
        row = [name]
        for column in THROUGHPUT_COLUMNS:
            log(f"{name} {column}")
            try:
                row.append(test(column))
            except duckdb.Error as e:
                row.append(f"error: {error_line(e)}")
        print(f"| {' | '.join(row)} |", flush=True)


def query_files():
    return sorted(Path("queries").glob("*.sql"), key=lambda file: int(file.stem.split("_")[0]))


def run_queries(rows, profile):
    print(f"\n## {rows:,} rows\n")
    print(f"Median ± sample standard deviation of {N} runs of the client-side profiled `query.total_time`.\n")
    print(f"| query | {' | '.join(HEADERS)} |")
    print(f"| --- |{' --- |' * len(HEADERS)}")
    bytes_rows = []
    for file in query_files():
        name, query = file.stem, file.read_text().strip()
        reset_file = file.with_suffix(".reset")
        reset = reset_file.read_text().strip() if reset_file.exists() else None
        want = fingerprint(connect("local"), query)
        row, bytes_row = [name], [name]
        for column in COLUMNS:
            if column != "local" and TARGETS[column][2] == "quack" and name in QUACK_NOT_APPLICABLE:
                row.append("n/a")
                if column == "quack":
                    bytes_row.append("n/a")
                continue
            log(f"{rows:,} rows: {name} {column}")
            con = connect(column)
            Path(f"plans/{column}-{name}.txt").write_text(explain(con, query))
            if column in ("n6k", "quack"):
                bytes_row.append(transfer(column, query, reset))
            cell = measure(con, query, profile, reset)
            if column != "local" and not cell.startswith("error"):
                try:
                    if fingerprint(con, query) != want:
                        cell += " ✗ result differs"
                except duckdb.Error as e:
                    cell += f" ✗ check failed: {error_line(e)}"
                if reset:
                    con.execute(reset)
            row.append(cell)
        print(f"| {' | '.join(row)} |", flush=True)
        bytes_rows.append(f"| {' | '.join(bytes_row)} |")
    print("\nKiB up / down from the client for one untimed run, counted at the TCP layer, framing included.\n")
    print("| query | n6k | quack |")
    print("| --- | --- | --- |")
    print("\n".join(bytes_rows), flush=True)


def where_it_runs(column, name):
    """server when the plan's only top-level node is the remote one; nested
    plan lines start with the outer box's border."""
    plan = Path(f"plans/{column}-{name}.txt")
    if not plan.exists():
        return "n/a"
    lines = [line for line in plan.read_text().splitlines() if not line.lstrip().startswith("│")]
    nodes = [node for line in lines for node in re.findall(r"╭─ ?([^─]+?) ?─", line)]
    if not nodes:
        return "fails"
    return "server" if len(nodes) == 1 and nodes[0] in SERVER_NODES else "client and server"


def print_where_it_runs():
    print("\n## Where each query runs\n")
    print("\"server\" means the plan is one remote operation and the server sends back only the")
    print("result. \"client and server\" means the client does part of the work. Taken from the")
    print("`EXPLAIN` plans in `plans/`, with perms, at the last row count.\n")
    print("| query | n6k | quack |")
    print("| --- | --- | --- |")
    for file in query_files():
        name = file.stem
        quack = "n/a" if name in QUACK_NOT_APPLICABLE else where_it_runs("quack", name)
        print(f"| {name} | {where_it_runs('n6k', name)} | {quack} |")


def main():
    check_version()
    Path("plans").mkdir(exist_ok=True)
    for stale in Path("plans").glob("*.txt"):
        stale.unlink()
    profile = Path(tempfile.mkdtemp()) / "profile.json"
    sysctl = ["sysctl", "-n", "machdep.cpu.brand_string"]
    cpu = subprocess.run(sysctl, check=False, capture_output=True, text=True).stdout.strip()
    print("# Results\n")
    print(f"Run on {time.strftime('%Y-%m-%d')} on {cpu or 'this machine'} over localhost.")
    print(f"DuckDB v{duckdb.__version__} on both ends.")
    print("Local extension builds." if os.environ.get("N6K_EXTENSION_DIR") else "Released extensions.")
    print("\nThe queries are in `queries/`. A query with a `.reset` file changes data, restored after each run.")
    print("Native on the two-server join is two in-process databases, so it is a best-case floor.")
    print("Quack serves one catalog, so the two-catalog join is `n/a` for it.")
    for index, rows in enumerate(SIZES):
        configure(rows)
        with ExitStack() as stack:
            log(f"starting servers with {rows:,} rows")
            start_servers(stack)
            run_queries(rows, profile)
            if index == 0:
                throughput(rows)
    print_where_it_runs()


if __name__ == "__main__":
    main()
