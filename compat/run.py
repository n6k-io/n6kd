#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["duckdb==2.0.0", "numpy", "pandas", "pytz"]
# ///
"""Usage: [N6KD=] [N6K_CLIENT=] [N6K_EXTENSION_DIR=] compat/run.py > compat/results.md

Each case runs in its own process group, so a crash or hang fails only that cell.
Relative paths resolve from compat/.
"""

import json
import os
import signal
import socket
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass, field
from pathlib import Path

import duckdb
import pandas

HERE = Path(__file__).resolve().parent
os.chdir(HERE)
if Path("../.env").exists():
    for env_line in Path("../.env").read_text().splitlines():
        env_key, _, env_value = env_line.partition("=")
        if env_value and not env_key.startswith("#"):
            os.environ.setdefault(env_key.strip(), env_value.strip())
N6KD = str(Path(os.environ.get("N6KD", "../n6kd")).resolve())


def default_client():
    for directory in os.environ.get("N6K_EXTENSION_DIR", "").split(os.pathsep):
        path = Path(directory) / "n6k_client" / "n6k_client.duckdb_extension"
        if directory and path.exists():
            return str(path)
    return "n6k_client"


N6K_CLIENT = os.environ.get("N6K_CLIENT") or default_client()
LOGS = HERE / "logs"
SERVER = ["--threads", "4", "--lockdown"]
PORTS = {"n6k": 8141, "quack": 8142}
TARGETS = ("native", "n6k", "quack")
TIMEOUT = 30
PASS = "✅"
UNSUPPORTED = "➖"
ERROR = "💥"
WRONG = "❌"
REFUSALS = (
    "not supported",
    "does not support",
    "not currently supported",
    "not implemented",
)
LEGEND = {
    PASS: "matches native",
    UNSUPPORTED: "unsupported",
    ERROR: "error, crash or timeout",
    WRONG: "wrong result",
}

TYPES = {
    "HUGEINT": "170141183460469231731687303715884105727::HUGEINT",
    "UHUGEINT": "340282366920938463463374607431768211455::UHUGEINT",
    "DECIMAL(38,18)": "12345678901234567890.123456789012345678::DECIMAL(38,18)",
    "INTERVAL": "INTERVAL '1 month 2 days 3 seconds'",
    "TIMESTAMP_NS": "'2024-01-01 00:00:00.123456789'::TIMESTAMP_NS",
    "TIMESTAMPTZ": "'2024-01-01 00:00:00+00'::TIMESTAMPTZ",
    "TIME_NS": "'12:00:00.123456789'::TIME_NS",
    "TIMETZ": "'12:00:00+01'::TIMETZ",
    "DATE infinity": "'infinity'::DATE",
    "UUID": "'6ba7b810-9dad-11d1-80b4-00c04fd430c8'::UUID",
    "BLOB": "'\\xAA\\xFF'::BLOB",
    "BIT": "'101'::BIT",
    "ENUM": "'x'::ENUM('x','y')",
    "LIST": "[1,2,3]",
    "STRUCT": "{'a':1,'b':'x'}",
    "MAP": "MAP{'a':1}",
    "UNION": "union_value(a := 1)",
    "ARRAY": "[1,2,3]::INT[3]",
    "nested": "[{'a':[MAP{'k':[1]}]}]",
    "BIGNUM": "123456789012345678901234567890::BIGNUM",
    "VARIANT": "{'a':1}::VARIANT",
    "GEOMETRY": "'POINT(1 2)'::GEOMETRY",
    "DOUBLE nan": "'nan'::DOUBLE",
    "NULL": "NULL::INT",
}

BASE_SQL = """CREATE TABLE a (i BIGINT PRIMARY KEY, k BIGINT);
INSERT INTO a SELECT range, range % 100 FROM range(1000000);
"""


@dataclass
class Case:
    section: str
    name: str
    run: object


CASES = []


def case(section, name):
    def add(fn):
        CASES.append(Case(section, name, fn))
        return fn

    return add


def log(msg):
    print(f"{time.strftime('%T')} {msg}", file=sys.stderr, flush=True)


def first_line(e):
    return str(e).splitlines()[0] if str(e) else type(e).__name__


def listening(port):
    try:
        socket.create_connection(("localhost", port)).close()
    except OSError:
        return False
    return True


def start_server(name, port, args):
    if listening(port):
        sys.exit(f":{port} is already in use")
    LOGS.mkdir(exist_ok=True)
    with open(LOGS / f"{name}.log", "a") as out:
        proc = subprocess.Popen(
            [N6KD, *args, "--addr", f"localhost:{port}"], stdout=out, stderr=out
        )
    while not listening(port):
        if proc.poll() is not None:
            sys.exit(f"server on :{port} exited; see compat/logs/{name}.log")
        time.sleep(0.05)
    return proc


def fixture_sql():
    tables = "".join(
        f"CREATE TABLE t{n} (i BIGINT PRIMARY KEY, v VARCHAR); INSERT INTO t{n} VALUES (1,'a'),(2,'b'),(3,'c');\n"
        for n in range(len(CASES))
    )
    types = "".join(
        f"CREATE TABLE ty{n} AS SELECT {expr} AS v;\n"
        for n, expr in enumerate(TYPES.values())
    )
    return BASE_SQL + tables + types


def connect(kind):
    con = duckdb.connect(config={"allow_unsigned_extensions": True})
    con.execute("SET threads = 4")
    if kind == "n6k":
        con.execute(f"LOAD '{N6K_CLIENT}'")
        con.execute(
            f"ATTACH 'n6k://localhost:{PORTS[kind]}' AS r (TYPE n6k, CATALOG 'app')"
        )
    else:
        con.execute("LOAD quack")
        con.execute("CREATE SECRET (TYPE quack, TOKEN 'compat')")
        con.execute(f"ATTACH 'quack:localhost:{PORTS[kind]}' AS r")
    return con


@dataclass
class Ctx:
    kind: str
    table: str
    tmp: str = field(default_factory=tempfile.mkdtemp)
    native: object = None

    def __post_init__(self):
        self.con = self.client()

    def client(self):
        if self.kind != "native":
            return connect(self.kind)
        if self.native:
            return self.native.cursor()
        self.native = duckdb.connect()
        self.native.execute("SET threads = 4; ATTACH ':memory:' AS r; USE r;")
        self.native.execute(fixture_sql())
        self.native.execute("USE memory")
        return self.native

    def sql(self, statement):
        return statement.format(T=self.table, tmp=self.tmp)

    def q(self, statement, con=None):
        return (con or self.con).execute(self.sql(statement)).fetchall()

    def on_server(self, statement):
        """Runs statement in the server's database, or directly on native."""
        if self.kind == "native":
            return self.q(f"USE r; {statement}")
        quoted = self.sql(statement).replace("'", "''")
        if self.kind == "n6k":
            return self.con.execute(
                f"SELECT * FROM n6k_catalog_query('r', '{quoted}')"
            ).fetchall()
        return self.con.execute(
            f"SELECT * FROM quack_query('quack:localhost:{PORTS['quack']}', '{quoted}')"
        ).fetchall()


def exception_class(ctx, statement, con=None):
    try:
        ctx.q(statement, con)
    except duckdb.Error as e:
        return type(e).__name__
    return "no error"


def add_sql_case(section, name, statement):
    case(section, name)(lambda ctx: ctx.q(statement))


for index, (type_name, _) in enumerate(TYPES.items()):

    def check_type(ctx, index=index):
        cursor = ctx.con.execute(f"SELECT v FROM r.ty{index}")
        return [str(cursor.description[0][1]), cursor.fetchall()]

    case("Types", type_name)(check_type)

SQL = "SQL coverage"
add_sql_case(SQL, "UPDATE RETURNING", "UPDATE r.{T} SET v='z' WHERE i=1 RETURNING i, v")
add_sql_case(SQL, "DELETE RETURNING", "DELETE FROM r.{T} WHERE i=1 RETURNING i")
add_sql_case(
    SQL,
    "ON CONFLICT",
    "INSERT INTO r.{T} VALUES (1,'q') ON CONFLICT (i) DO UPDATE SET v=excluded.v; SELECT v FROM r.{T} WHERE i=1",
)
add_sql_case(
    SQL,
    "MERGE",
    "MERGE INTO r.{T} t USING (SELECT 1 i, 'm' v) s ON t.i=s.i WHEN MATCHED THEN UPDATE SET v=s.v;"
    " SELECT v FROM r.{T} WHERE i=1",
)
add_sql_case(
    SQL,
    "CREATE VIEW",
    "CREATE VIEW r.{T}_v AS SELECT * FROM r.{T}; SELECT count(*) FROM r.{T}_v",
)
add_sql_case(
    SQL,
    "CREATE SCHEMA",
    "CREATE SCHEMA r.{T}_s; CREATE TABLE r.{T}_s.x (i INT); SELECT count(*) FROM r.{T}_s.x",
)
add_sql_case(
    SQL, "CREATE INDEX", "CREATE INDEX {T}_ix ON r.{T}(v); SELECT count(*) FROM r.{T}"
)
add_sql_case(SQL, "CREATE SEQUENCE", "CREATE SEQUENCE r.{T}_q; SELECT 1")
add_sql_case(
    SQL,
    "ALTER add column",
    "ALTER TABLE r.{T} ADD COLUMN n INT; SELECT i, n FROM r.{T} WHERE i=1",
)
add_sql_case(
    SQL,
    "ALTER add column with default",
    "ALTER TABLE r.{T} ADD COLUMN n INT DEFAULT 7; SELECT i, n FROM r.{T} WHERE i=1",
)
add_sql_case(
    SQL,
    "ALTER drop column",
    "ALTER TABLE r.{T} DROP COLUMN v; SELECT * FROM r.{T} WHERE i=1",
)
add_sql_case(
    SQL,
    "ALTER change type",
    "ALTER TABLE r.{T} ALTER v TYPE BLOB; SELECT typeof(v) FROM r.{T} LIMIT 1",
)
add_sql_case(
    SQL,
    "ALTER rename",
    "ALTER TABLE r.{T} RENAME TO {T}_r; SELECT count(*) FROM r.{T}_r",
)
add_sql_case(
    SQL,
    "DROP TABLE",
    "DROP TABLE r.{T}; SELECT count(*) FROM duckdb_tables() WHERE database_name='r' AND table_name='{T}'",
)
add_sql_case(
    SQL,
    "CREATE TABLE AS",
    "CREATE TABLE r.{T}_c AS SELECT * FROM r.{T}; SELECT count(*) FROM r.{T}_c",
)
add_sql_case(
    SQL,
    "COPY out",
    "COPY (SELECT * FROM r.{T}) TO '{tmp}/out.parquet'; SELECT count(*) FROM '{tmp}/out.parquet'",
)


@case(SQL, "COPY in")
def copy_in(ctx):
    duckdb.connect().execute(
        ctx.sql("COPY (SELECT 9::BIGINT i, 'p' v) TO '{tmp}/in.parquet'")
    )
    return ctx.q("COPY r.{T} FROM '{tmp}/in.parquet'; SELECT count(*) FROM r.{T}")


add_sql_case(SQL, "USE", "USE r; SELECT count(*) FROM {T}")


def describe_without_default(ctx, table):
    return [row[:4] + row[5:] for row in ctx.q(f"DESCRIBE {table}")]


@case(SQL, "DESCRIBE")
def describe(ctx):
    return describe_without_default(ctx, "r.{T}")


add_sql_case(SQL, "rowid", "SELECT count(rowid) FROM r.{T}")


@case(SQL, "SAMPLE")
def sample(ctx):
    ctx.q("SELECT * FROM r.{T} USING SAMPLE 1")
    return "ran"


add_sql_case(
    SQL,
    "recursive CTE",
    "WITH RECURSIVE c(n) AS (SELECT min(i) FROM r.{T} UNION ALL SELECT n+1 FROM c WHERE n<3) SELECT count(*) FROM c",
)
add_sql_case(SQL, "PIVOT", "SELECT count(*) FROM (PIVOT r.{T} ON v USING count(*))")
add_sql_case(SQL, "LATERAL", "SELECT count(*) FROM r.{T} t, LATERAL (SELECT t.i + 1)")


@case(SQL, "EXPLAIN")
def explain(ctx):
    ctx.q("EXPLAIN SELECT count(*) FROM r.{T}")
    return "ran"


@case(SQL, "EXPLAIN ANALYZE")
def explain_analyze(ctx):
    ctx.q("EXPLAIN ANALYZE SELECT count(*) FROM r.{T}")
    return "ran"


@case(SQL, "Prepared parameters")
def prepared(ctx):
    return ctx.con.execute(
        ctx.sql("SELECT count(*) FROM r.{T} WHERE i < ?"), [3]
    ).fetchall()


add_sql_case(SQL, "Multi-statement", "SELECT 1; SELECT count(*) FROM r.{T}")

REFS = "Client-side references"


@case(REFS, "getvariable in a filter")
def getvariable(ctx):
    return ctx.q(
        "SET VARIABLE x = 5; SELECT count(*) FROM r.a WHERE i < getvariable('x')"
    )


@case(REFS, "Macro in a filter")
def macro(ctx):
    return ctx.q(
        "CREATE TEMP MACRO under(v) AS v < 1000; SELECT count(*) FROM r.a WHERE under(i)"
    )


@case(REFS, "Python UDF in a filter")
def python_udf(ctx):
    ctx.con.create_function(
        "under", lambda v: v < 1000, ["BIGINT"], "BOOLEAN", type="native"
    )
    return ctx.q("SELECT count(*) FROM r.a WHERE under(i)")


add_sql_case(
    REFS,
    "random() in a filter",
    "SELECT count(*) FROM r.a WHERE i < 1000 AND random() < 2",
)


@case(REFS, "Client setting in the select list")
def client_setting(ctx):
    return ctx.q(
        "SET threads = 2; SELECT current_setting('threads')::INT, count(*) FROM r.a GROUP BY 1"
    )


@case(REFS, "Python UDF in the select list")
def python_udf_select(ctx):
    ctx.con.create_function(
        "under", lambda v: v < 1000, ["BIGINT"], "BOOLEAN", type="native"
    )
    return ctx.q("SELECT sum(under(i)::INT) FROM r.a")


add_sql_case(
    REFS,
    "Macro in the select list",
    "CREATE TEMP MACRO double(v) AS v * 2; SELECT sum(double(i)) FROM r.{T}",
)


@case(REFS, "Prepared parameter in a mixed join")
def prepared_mixed(ctx):
    ctx.q("CREATE TABLE l AS SELECT 1 AS k")
    return ctx.con.execute(
        ctx.sql("SELECT count(*) FROM r.{T} t JOIN l ON t.i = l.k WHERE t.i < ?"), [3]
    ).fetchall()


@case(REFS, "Prepared parameters in INSERT")
def prepared_insert(ctx):
    ctx.con.execute(ctx.sql("INSERT INTO r.{T} VALUES (?, ?)"), [9, "p"])
    return ctx.q("SELECT count(*) FROM r.{T}")


@case(REFS, "Prepared parameters in UPDATE")
def prepared_update(ctx):
    ctx.con.execute(ctx.sql("UPDATE r.{T} SET v = ? WHERE i = ?"), ["p", 1])
    return ctx.q("SELECT v FROM r.{T} WHERE i = 1")


LOCAL = "Client-side data"
MAKE_LOCAL = "CREATE TABLE l AS SELECT 1 AS k, 'x' AS v; "


@case(LOCAL, "Dataframe join")
def dataframe_join(ctx):
    df = pandas.DataFrame({"i": [1, 2]})  # noqa: F841
    return ctx.con.execute(
        ctx.sql("SELECT count(*) FROM r.{T} JOIN df USING (i)")
    ).fetchall()


@case(LOCAL, "Dataframe in a filter")
def dataframe_filter(ctx):
    df = pandas.DataFrame({"i": [1, 2]})  # noqa: F841
    return ctx.con.execute(
        ctx.sql("SELECT count(*) FROM r.{T} WHERE i IN (SELECT i FROM df)")
    ).fetchall()


@case(LOCAL, "Client file in a filter")
def client_file(ctx):
    Path(ctx.tmp, "keys.csv").write_text("i\n1\n2\n")
    return ctx.q(
        "SELECT count(*) FROM r.{T} WHERE i IN (SELECT i FROM read_csv('{tmp}/keys.csv'))"
    )


@case(LOCAL, "Client file join")
def client_file_join(ctx):
    Path(ctx.tmp, "keys.csv").write_text("i\n1\n2\n")
    return ctx.q("SELECT count(*) FROM r.{T} JOIN read_csv('{tmp}/keys.csv') USING (i)")


add_sql_case(
    LOCAL,
    "Client table macro",
    "CREATE TEMP MACRO keys() AS TABLE SELECT 1 AS i; SELECT count(*) FROM r.{T} WHERE i IN (SELECT i FROM keys())",
)
add_sql_case(
    LOCAL,
    "Client secret in a join",
    "CREATE SECRET {T}_s (TYPE http, BEARER_TOKEN 'x');"
    " SELECT count(*) FROM r.{T}, (SELECT * FROM duckdb_secrets() WHERE name = '{T}_s')",
)
add_sql_case(
    LOCAL,
    "Database attached on the client",
    "ATTACH ':memory:' AS c; CREATE TABLE c.t AS SELECT 1 AS i; SELECT count(*) FROM r.{T} JOIN c.t USING (i)",
)
add_sql_case(
    LOCAL,
    "Local table in a filter",
    MAKE_LOCAL + "SELECT count(*) FROM r.{T} WHERE i IN (SELECT k FROM l)",
)
add_sql_case(
    LOCAL,
    "CTE with client data",
    "WITH l AS (SELECT 1 AS i) SELECT count(*) FROM r.{T} JOIN l USING (i)",
)
add_sql_case(
    LOCAL,
    "Inline VALUES join",
    "SELECT count(*) FROM r.{T} JOIN (VALUES (1), (2)) x(i) USING (i)",
)
add_sql_case(
    LOCAL,
    "Correlated subquery on a remote table",
    MAKE_LOCAL
    + "SELECT count(*) FROM l WHERE EXISTS (SELECT 1 FROM r.{T} t WHERE t.i = l.k)",
)
add_sql_case(
    LOCAL,
    "Window over a mixed join",
    MAKE_LOCAL
    + "SELECT sum(rn) FROM (SELECT row_number() OVER (ORDER BY t.i) rn FROM r.{T} t, l)",
)

WRITES = "Writes with RETURNING and client data"
add_sql_case(
    WRITES, "INSERT RETURNING", "INSERT INTO r.{T} VALUES (9, 'p') RETURNING i, v"
)
add_sql_case(
    WRITES,
    "UPDATE RETURNING an expression",
    "UPDATE r.{T} SET v = 'z' WHERE i = 1 RETURNING i + 1, upper(v)",
)
add_sql_case(
    WRITES,
    "ON CONFLICT DO NOTHING",
    "INSERT INTO r.{T} VALUES (1, 'q') ON CONFLICT DO NOTHING; SELECT v FROM r.{T} WHERE i = 1",
)
add_sql_case(
    WRITES,
    "INSERT OR REPLACE",
    "INSERT OR REPLACE INTO r.{T} VALUES (1, 'q'); SELECT v FROM r.{T} WHERE i = 1",
)
add_sql_case(
    WRITES,
    "INSERT from a local table",
    MAKE_LOCAL
    + "INSERT INTO r.{T} SELECT k + 10, v FROM l; SELECT count(*) FROM r.{T}",
)
add_sql_case(
    WRITES,
    "INSERT RETURNING from a local table",
    MAKE_LOCAL + "INSERT INTO r.{T} SELECT k + 10, v FROM l RETURNING i, v",
)


@case(WRITES, "INSERT from a dataframe")
def insert_dataframe(ctx):
    df = pandas.DataFrame({"i": [10], "v": ["p"]})  # noqa: F841
    ctx.con.execute(ctx.sql("INSERT INTO r.{T} SELECT * FROM df"))
    return ctx.q("SELECT count(*) FROM r.{T}")


add_sql_case(
    WRITES,
    "UPDATE FROM a local table",
    MAKE_LOCAL
    + "UPDATE r.{T} t SET v = l.v FROM l WHERE t.i = l.k; SELECT v FROM r.{T} WHERE i = 1",
)
add_sql_case(
    WRITES,
    "UPDATE with a remote subquery",
    "UPDATE r.{T} SET v = 'z' WHERE i IN (SELECT min(i) FROM r.{T}); SELECT v FROM r.{T} WHERE i = 1",
)
add_sql_case(
    WRITES,
    "DELETE USING a local table",
    MAKE_LOCAL
    + "DELETE FROM r.{T} t USING l WHERE t.i = l.k; SELECT count(*) FROM r.{T}",
)
add_sql_case(
    WRITES,
    "MERGE from a local table",
    MAKE_LOCAL
    + "MERGE INTO r.{T} t USING l ON t.i = l.k WHEN MATCHED THEN UPDATE SET v = l.v; SELECT v FROM r.{T} WHERE i = 1",
)
add_sql_case(
    WRITES,
    "MERGE with insert and delete",
    "MERGE INTO r.{T} t USING (SELECT * FROM (VALUES (1, 'u'), (9, 'n')) x(i, v)) s ON t.i = s.i"
    " WHEN MATCHED THEN DELETE WHEN NOT MATCHED THEN INSERT VALUES (s.i, s.v); SELECT * FROM r.{T} ORDER BY i",
)
add_sql_case(
    WRITES,
    "MERGE RETURNING",
    "MERGE INTO r.{T} t USING (SELECT 1 i, 'm' v) s ON t.i = s.i WHEN MATCHED THEN UPDATE SET v = s.v RETURNING i, v",
)
add_sql_case(
    WRITES,
    "CREATE TABLE AS from a mixed join",
    MAKE_LOCAL
    + "CREATE TABLE r.{T}_c AS SELECT t.i FROM r.{T} t JOIN l ON t.i = l.k; SELECT count(*) FROM r.{T}_c",
)

CONSTRAINTS = "Constraints"


@case(CONSTRAINTS, "PRIMARY KEY over attach: duplicate rejected")
def primary_key_duplicate(ctx):
    ctx.q(
        "CREATE TABLE r.{T}_k (i BIGINT PRIMARY KEY, v VARCHAR); INSERT INTO r.{T}_k VALUES (1,'a')"
    )
    return exception_class(ctx, "INSERT INTO r.{T}_k VALUES (1,'dup')"), ctx.q(
        "SELECT count(*) FROM r.{T}_k"
    )


@case(CONSTRAINTS, "PRIMARY KEY over attach: UPDATE, MERGE, DELETE work")
def primary_key_writes(ctx):
    ctx.q(
        "CREATE TABLE r.{T}_k (i BIGINT PRIMARY KEY, v VARCHAR); INSERT INTO r.{T}_k VALUES (1,'a'),(2,'b')"
    )
    ctx.q("UPDATE r.{T}_k SET v='z' WHERE i=1")
    ctx.q(
        "MERGE INTO r.{T}_k t USING (SELECT 1 i, 'm' v) s ON t.i=s.i WHEN MATCHED THEN UPDATE SET v=s.v"
    )
    ctx.q("DELETE FROM r.{T}_k WHERE i=2")
    return ctx.q("SELECT * FROM r.{T}_k")


@case(CONSTRAINTS, "DESCRIBE keeps a key created over attach")
def describe_key(ctx):
    ctx.q("CREATE TABLE r.{T}_k (i BIGINT PRIMARY KEY, v VARCHAR DEFAULT 'd')")
    ctx.q("INSERT INTO r.{T}_k (i) VALUES (1)")
    return describe_without_default(ctx, "r.{T}_k"), ctx.q("SELECT * FROM r.{T}_k")


add_sql_case(
    CONSTRAINTS,
    "UPDATE on a table without a key",
    "CREATE TABLE r.{T}_n (i BIGINT, v VARCHAR); INSERT INTO r.{T}_n VALUES (1, 'a');"
    " UPDATE r.{T}_n SET v = 'z' WHERE i = 1; SELECT * FROM r.{T}_n",
)
add_sql_case(
    CONSTRAINTS,
    "DELETE on a table without a key",
    "CREATE TABLE r.{T}_n (i BIGINT, v VARCHAR); INSERT INTO r.{T}_n VALUES (1, 'a'), (2, 'b');"
    " DELETE FROM r.{T}_n WHERE i = 1; SELECT * FROM r.{T}_n",
)


@case(CONSTRAINTS, "CHECK over attach")
def check_constraint(ctx):
    ctx.q("CREATE TABLE r.{T}_c (i INT CHECK (i > 0))")
    return exception_class(ctx, "INSERT INTO r.{T}_c VALUES (-1)"), ctx.q(
        "SELECT count(*) FROM r.{T}_c"
    )


@case(CONSTRAINTS, "Foreign key over attach")
def foreign_key(ctx):
    ctx.q("CREATE TABLE r.{T}_p (i INT PRIMARY KEY)")
    ctx.q("CREATE TABLE r.{T}_f (p INT REFERENCES r.{T}_p (i))")
    return exception_class(ctx, "INSERT INTO r.{T}_f VALUES (1)"), ctx.q(
        "SELECT count(*) FROM r.{T}_f"
    )


add_sql_case(
    CONSTRAINTS,
    "Generated column over attach",
    "CREATE TABLE r.{T}_g (i INT, d INT GENERATED ALWAYS AS (i * 2)); INSERT INTO r.{T}_g (i) VALUES (2);"
    " SELECT i, d FROM r.{T}_g",
)

TXN = "Transactions"


@case(TXN, "Read your own write")
def read_own_write(ctx):
    ctx.q("BEGIN; UPDATE r.{T} SET v='z' WHERE i=1")
    return ctx.q("SELECT v FROM r.{T} WHERE i=1")


@case(TXN, "Other client doesn't see uncommitted write")
def isolation(ctx):
    other = ctx.client()
    ctx.q("BEGIN; UPDATE r.{T} SET v='z' WHERE i=1")
    return ctx.q("SELECT v FROM r.{T} WHERE i=1", other)


@case(TXN, "Write conflict")
def write_conflict(ctx):
    other = ctx.client()
    ctx.q("BEGIN; UPDATE r.{T} SET v='z' WHERE i=1")
    return exception_class(ctx, "UPDATE r.{T} SET v='y' WHERE i=1", other)


@case(TXN, "Rollback")
def rollback(ctx):
    ctx.q("BEGIN; UPDATE r.{T} SET v='z' WHERE i=1; ROLLBACK")
    return ctx.q("SELECT v FROM r.{T} WHERE i=1", ctx.client())


@case(TXN, "Commit")
def commit(ctx):
    ctx.q("BEGIN; UPDATE r.{T} SET v='z' WHERE i=1; COMMIT")
    return ctx.q("SELECT v FROM r.{T} WHERE i=1", ctx.client())


@case(TXN, "Own write in a mixed join")
def own_write_mixed(ctx):
    ctx.q("BEGIN; UPDATE r.{T} SET v='z' WHERE i=2")
    return ctx.q("SELECT t.v FROM r.{T} t JOIN (SELECT 2 i) l USING (i)")


@case(TXN, "DDL visible to its own transaction")
def ddl_self(ctx):
    ctx.q("BEGIN; CREATE TABLE r.{T}_n (i INT)")
    return ctx.q("SELECT count(*) FROM r.{T}_n")


@case(TXN, "Uncommitted DDL hidden from other clients")
def ddl_hidden(ctx):
    ctx.q("BEGIN; CREATE TABLE r.{T}_n (i INT)")
    return exception_class(ctx, "SELECT count(*) FROM r.{T}_n", ctx.client())


@case(TXN, "Rolled-back DDL gone")
def ddl_rollback(ctx):
    ctx.q("BEGIN; CREATE TABLE r.{T}_n (i INT); ROLLBACK")
    return exception_class(ctx, "SELECT count(*) FROM r.{T}_n", ctx.client())


@case(TXN, "Committed DDL persists")
def ddl_commit(ctx):
    ctx.q("BEGIN; CREATE TABLE r.{T}_n (i INT); COMMIT")
    return ctx.q("SELECT count(*) FROM r.{T}_n", ctx.client())


ERRORS = "Errors"


@case(ERRORS, "Primary key violation")
def err_pk(ctx):
    return exception_class(ctx, "INSERT INTO r.{T} VALUES (1,'dup')")


@case(ERRORS, "Missing table")
def err_missing(ctx):
    return exception_class(ctx, "SELECT * FROM r.nope")


@case(ERRORS, "Conversion failure")
def err_conversion(ctx):
    return exception_class(ctx, "SELECT CAST(v AS INT) FROM r.{T}")


@case(ERRORS, "Missing function")
def err_function(ctx):
    return exception_class(ctx, "SELECT nofunc(i) FROM r.{T}")


def run_case(index, kind):
    try:
        result = {"value": repr(CASES[index].run(Ctx(kind, f"t{index}")))}
    except duckdb.Error as e:
        result = {"error": type(e).__name__, "message": first_line(e)}
    print(json.dumps(result), flush=True)
    os._exit(0)


def launch(index, kind):
    proc = subprocess.Popen(
        [sys.executable, "-I", __file__, "--case", str(index), kind],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        start_new_session=True,
    )
    try:
        out, err = proc.communicate(timeout=TIMEOUT)
        code = proc.returncode
    except subprocess.TimeoutExpired:
        return {"failure": f"over {TIMEOUT}s"}
    finally:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    if code < 0:
        return {"failure": f"crash: signal {-code}"}
    lines = [line for line in out.splitlines() if line.startswith("{")]
    if not lines:
        return {"failure": f"exit {code}: {err.strip()[-120:]}"}
    return json.loads(lines[-1])


def matches(result, reference):
    if "failure" in result or "failure" in reference:
        return False
    return result.get("value") == reference.get("value") and result.get(
        "error"
    ) == reference.get("error")


def detail(result):
    text = result.get("failure") or result.get("message") or result["value"]
    return " ".join(str(text).split())


def status(result, reference):
    if matches(result, reference):
        return PASS
    if "failure" in result:
        return ERROR
    if "error" not in result:
        return WRONG
    message = result["message"].lower()
    return UNSUPPORTED if any(phrase in message for phrase in REFUSALS) else ERROR


def main():
    if not os.environ.get("N6K_EXTENSION_DIR"):
        log("using installed extensions; set N6K_EXTENSION_DIR for local builds")
    tmp = Path(tempfile.mkdtemp())
    (tmp / "n6k.sql").write_text("ATTACH ':memory:' AS app; USE app;\n" + fixture_sql())
    (tmp / "quack.sql").write_text(fixture_sql())
    servers = []
    try:
        servers.append(
            start_server("n6k", PORTS["n6k"], ["-f", str(tmp / "n6k.sql"), *SERVER])
        )
        args = ["quack", "-f", str(tmp / "quack.sql"), *SERVER, "--token", "compat"]
        servers.append(start_server("quack", PORTS["quack"], args))
        counts = {kind: dict.fromkeys(LEGEND, 0) for kind in PORTS}
        section = None
        rows = []
        details = []
        for index, item in enumerate(CASES):
            log(f"{index + 1}/{len(CASES)} {item.section}: {item.name}")
            reference = launch(index, "native")
            cells = {"native": PASS if "failure" not in reference else ERROR}
            if cells["native"] != PASS:
                details.append(
                    f"- {ERROR} **native, {item.name}:** `{detail(reference)}`"
                )
            for kind in PORTS:
                result = launch(index, kind)
                cells[kind] = status(result, reference)
                counts[kind][cells[kind]] += 1
                if cells[kind] == WRONG:
                    details.append(
                        f"- {WRONG} **{kind}, {item.name}:** got `{detail(result)}`, native `{detail(reference)}`"
                    )
                elif cells[kind] != PASS:
                    details.append(
                        f"- {cells[kind]} **{kind}, {item.name}:** `{detail(result)}`"
                    )
            if item.section != section:
                section = item.section
                rows.append(
                    f"\n## {section}\n\n| test | native | n6k | quack |\n| --- | --- | --- | --- |"
                )
            rows.append(
                f"| {item.name} | {cells['native']} | {cells['n6k']} | {cells['quack']} |"
            )
    finally:
        for proc in servers:
            proc.kill()
            proc.wait()
    print("# Compat results\n")
    print(
        f"Run on {time.strftime('%Y-%m-%d')}. Matching native means the same result or the same exception class.\n"
    )
    print("| | n6k | quack |\n| --- | --- | --- |")
    for mark, meaning in LEGEND.items():
        print(f"| {mark} {meaning} | {counts['n6k'][mark]} | {counts['quack'][mark]} |")
    print("\n".join(rows))
    print("\n## Details\n")
    print("\n".join(details) if details else "Everything matches native.")


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--case":
        run_case(int(sys.argv[2]), sys.argv[3])
    else:
        main()
