#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["cryptography", "duckdb==2.0.0"]
# ///
"""Usage: [N6KD=] [N6K_CLIENT=] [N6K_EXTENSION_DIR=] security/run.py > security/results.md

Each check has a written expected result and runs in its own process group.
Relative paths resolve from security/.
"""

import base64
import http.server
import json
import os
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
from dataclasses import dataclass
from pathlib import Path

import duckdb
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

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
TOKEN = "security-token"
PORTS = {"n6k": 8151, "quack": 8152}
JWT_PORT = 8153
ISSUER_PORT = 8154
ISSUER = f"http://localhost:{ISSUER_PORT}"
AUDIENCE = "security"
KEY_FILE_ENV = "SECURITY_KEY_FILE"
TIMEOUT = 30
PASS = "✅"
BROKEN = "❌"
ERROR = "💥"
NOT_APPLICABLE = "➖"
LEGEND = {PASS: "holds", BROKEN: "does not hold", ERROR: "check could not run"}
PERMISSION = "PermissionException"


@dataclass
class Check:
    section: str
    name: str
    run: object
    expect: object
    targets: tuple = ("n6k", "quack")


CHECKS = []


def check(section, name, expect, **kw):
    def add(fn):
        CHECKS.append(Check(section, name, fn, expect, **kw))
        return fn

    return add


def log(msg):
    print(f"{time.strftime('%T')} {msg}", file=sys.stderr, flush=True)


def first_line(e):
    return str(e).splitlines()[0] if str(e) else type(e).__name__


def connect(kind, token=TOKEN):
    con = duckdb.connect(config={"allow_unsigned_extensions": True})
    if kind == "n6k":
        con.execute(f"LOAD '{N6K_CLIENT}'")
        option = f", TOKEN '{token}'" if token else ""
        con.execute(f"ATTACH 'n6k://localhost:{PORTS[kind]}' AS r (TYPE n6k, CATALOG 'app'{option})")
    else:
        con.execute("LOAD quack")
        if token:
            con.execute(f"CREATE SECRET (TYPE quack, TOKEN '{token}')")
        con.execute(f"ATTACH 'quack:localhost:{PORTS[kind]}' AS r")
    return con


@dataclass
class Ctx:
    kind: str
    name: str
    tmp: str

    def __post_init__(self):
        self.con = connect(self.kind)

    def client(self):
        return connect(self.kind)

    def table(self, name):
        return f"app.{name}" if self.kind == "n6k" else name

    def raw(self, sql, con=None):
        quoted = sql.replace("'", "''")
        if self.kind == "n6k":
            statement = f"SELECT * FROM n6k_catalog_query('r', '{quoted}')"
        else:
            statement = f"SELECT * FROM quack_query('quack:localhost:{PORTS[self.kind]}', '{quoted}')"
        return (con or self.con).execute(statement).fetchall()

    def q(self, sql, con=None):
        return (con or self.con).execute(sql).fetchall()


def error_class(fn):
    try:
        fn()
    except duckdb.Error as e:
        return type(e).__name__
    return "no error"


PERMS = "Perms (vcatbridge)"


@check(PERMS, "Row rule holds over attach", [(50,)])
def row_rule_attach(ctx):
    return ctx.q("SELECT count(*) FROM r.a")


@check(PERMS, "Row rule holds through raw SQL", [(50,)])
def row_rule_raw(ctx):
    return ctx.raw(f"SELECT count(*) FROM {ctx.table('a')}")


@check(PERMS, "A filter can't reach hidden rows", ([(0,)], [(0,)]))
def filter_hidden(ctx):
    return ctx.q("SELECT count(*) FROM r.a WHERE k >= 5"), ctx.raw(f"SELECT count(*) FROM {ctx.table('a')} WHERE k >= 5")


@check(PERMS, "Row rule holds in a join and an aggregate", ([(50, 4)], [(50, 4)]))
def rule_in_join(ctx):
    a, b = ctx.table("a"), ctx.table("b")
    raw = ctx.raw(f"SELECT count(*), max(a.k) FROM {a} a JOIN {b} b USING (k)")
    return ctx.q("SELECT count(*), max(a.k) FROM r.a a JOIN r.b b USING (k)"), raw


WRITES = (
    "INSERT INTO {} VALUES (1000, 'x')",
    "UPDATE {} SET v = 'x' WHERE k = 1",
    "DELETE FROM {} WHERE k = 1",
)
UNCHANGED = ([PERMISSION, PERMISSION, PERMISSION], [(100, 1)])


def table_state(ctx):
    return ctx.q("SELECT count(*), count(*) FILTER (k = 1 AND v = 'v1') FROM r.b")


@check(PERMS, "Writes denied on a select-only table, over attach", UNCHANGED)
def writes_attach(ctx):
    classes = [error_class(lambda sql=sql: ctx.q(sql.format("r.b"))) for sql in WRITES]
    return classes, table_state(ctx)


@check(PERMS, "Writes denied on a select-only table, through raw SQL", UNCHANGED)
def writes_raw(ctx):
    classes = [error_class(lambda sql=sql: ctx.raw(sql.format(ctx.table("b")))) for sql in WRITES]
    return classes, table_state(ctx)


@check(PERMS, "Unlisted table unreachable", ("CatalogException", "CatalogException", [(0,)]))
def unlisted(ctx):
    attach = error_class(lambda: ctx.q("SELECT * FROM r.secret"))
    raw = error_class(lambda: ctx.raw(f"SELECT * FROM {ctx.table('secret')}"))
    return attach, raw, ctx.q("SELECT count(*) FROM information_schema.tables WHERE table_name = 'secret'")


LOCKDOWN = "Lockdown"


@check(LOCKDOWN, "File read denied", PERMISSION)
def file_read(ctx):
    probe = Path(ctx.tmp) / "probe.csv"
    probe.write_text("a\n1\n")
    return error_class(lambda: ctx.raw(f"SELECT * FROM read_csv('{probe}')"))


@check(LOCKDOWN, "File write denied", (PERMISSION, False))
def file_write(ctx):
    target = Path(ctx.tmp) / "written.csv"
    denied = error_class(lambda: ctx.raw(f"COPY (SELECT 1) TO '{target}'"))
    return denied, target.exists()


@check(LOCKDOWN, "LOAD and INSTALL have no effect", "CatalogException")
def extensions(ctx):
    error_class(lambda: ctx.raw("INSTALL inet"))
    error_class(lambda: ctx.raw("LOAD inet"))
    return error_class(lambda: ctx.raw("SELECT '127.0.0.1'::INET"))


@check(LOCKDOWN, "Changing a locked setting has no effect", True)
def locked_setting(ctx):
    read = "SELECT current_setting('memory_limit')"
    before = ctx.raw(read)
    error_class(lambda: ctx.raw("SET memory_limit = '1GB'"))
    return before == ctx.raw(read)


ISOLATION = "Session isolation"


@check(ISOLATION, "Table created by one client is hidden from others", ([(1,)], [(0,)]))
def table_isolated(ctx):
    other = ctx.client()
    ctx.raw(f"CREATE TABLE {ctx.name} (i INT)")
    count = f"SELECT count(*) FROM duckdb_tables() WHERE table_name = '{ctx.name}'"
    return ctx.raw(count), ctx.raw(count, other)


@check(ISOLATION, "Database attached by one client is hidden from others", ([(1,)], [(0,)]))
def attach_isolated(ctx):
    other = ctx.client()
    ctx.raw(f"ATTACH ':memory:' AS {ctx.name}")
    count = f"SELECT count(*) FROM duckdb_databases() WHERE database_name = '{ctx.name}'"
    return ctx.raw(count), ctx.raw(count, other)


@check(ISOLATION, "SET can't change another client's limits", True)
def set_isolated(ctx):
    other = ctx.client()
    read = "SELECT current_setting('memory_limit')"
    before = ctx.raw(read, other)
    error_class(lambda: ctx.raw("SET memory_limit = '1GB'"))
    return before == ctx.raw(read, other)


IDENTITY = "Identity"


def refused_then_allowed(ctx, token):
    def attempt():
        connect(ctx.kind, token).execute("SELECT count(*) FROM r.b").fetchall()

    return error_class(attempt) != "no error", ctx.q("SELECT count(*) FROM r.b")


@check(IDENTITY, "Missing credential rejected", (True, [(100,)]))
def missing_credential(ctx):
    return refused_then_allowed(ctx, None)


@check(IDENTITY, "Wrong credential rejected", (True, [(100,)]))
def wrong_credential(ctx):
    return refused_then_allowed(ctx, "wrong-token")


def b64(data):
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def sign_jwt(subject):
    key = serialization.load_pem_private_key(Path(os.environ[KEY_FILE_ENV]).read_bytes(), password=None)
    now = int(time.time())
    header = b64(json.dumps({"alg": "EdDSA", "typ": "JWT", "kid": "security"}).encode())
    claims = {"iss": ISSUER, "aud": AUDIENCE, "sub": subject, "iat": now, "exp": now + 300}
    payload = b64(json.dumps(claims).encode())
    signature = b64(key.sign(f"{header}.{payload}".encode()))
    return f"{header}.{payload}.{signature}"


@check(
    IDENTITY,
    "Rows differ by JWT claim",
    ([("alice",)], [("alice",)], [("bob",)], [("bob",)]),
    targets=("n6k",),
)
def jwt_rows(ctx):
    results = []
    for user in ("alice", "bob"):
        con = duckdb.connect(config={"allow_unsigned_extensions": True})
        con.execute(f"LOAD '{N6K_CLIENT}'")
        con.execute(f"ATTACH 'n6k://localhost:{JWT_PORT}' AS r (TYPE n6k, CATALOG 'app', TOKEN '{sign_jwt(user)}')")
        results.append(con.execute("SELECT owner FROM r.owned").fetchall())
        results.append(con.execute("SELECT * FROM n6k_catalog_query('r', 'SELECT owner FROM app.owned')").fetchall())
    return tuple(results)


def run_check(index, kind):
    item = CHECKS[index]
    try:
        result = {"value": repr(item.run(Ctx(kind, f"iso{index}_{os.getpid()}", tempfile.mkdtemp())))}
    except duckdb.Error as e:
        result = {"error": f"{type(e).__name__}: {first_line(e)}"}
    print(json.dumps(result), flush=True)
    os._exit(0)


def launch(index, kind):
    proc = subprocess.Popen(
        [sys.executable, "-I", __file__, "--check", str(index), kind],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        start_new_session=True,
    )
    try:
        out, err = proc.communicate(timeout=TIMEOUT)
        code = proc.returncode
    except subprocess.TimeoutExpired:
        return {"error": f"over {TIMEOUT}s"}
    finally:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    if code < 0:
        return {"error": f"crash: signal {-code}"}
    lines = [line for line in out.splitlines() if line.startswith("{")]
    return json.loads(lines[-1]) if lines else {"error": f"exit {code}: {err.strip()[-160:]}"}


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
        proc = subprocess.Popen([N6KD, *args, "--addr", f"localhost:{port}"], stdout=out, stderr=out)
    while not listening(port):
        if proc.poll() is not None:
            sys.exit(f"server on :{port} exited; see security/logs/{name}.log")
        time.sleep(0.05)
    return proc


def start_issuer(key):
    public = key.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    documents = {
        "/.well-known/openid-configuration": {
            "issuer": ISSUER,
            "jwks_uri": f"{ISSUER}/jwks",
            "id_token_signing_alg_values_supported": ["EdDSA"],
        },
        "/jwks": {
            "keys": [{"kty": "OKP", "crv": "Ed25519", "x": b64(public), "kid": "security", "alg": "EdDSA", "use": "sig"}]
        },
    }

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            body = json.dumps(documents.get(self.path, {})).encode()
            self.send_response(200 if self.path in documents else 404)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    server = http.server.ThreadingHTTPServer(("localhost", ISSUER_PORT), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()


def main():
    key = Ed25519PrivateKey.generate()
    pem = key.private_bytes(
        serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()
    )
    key_file = Path(tempfile.mkdtemp()) / "key.pem"
    key_file.write_bytes(pem)
    os.environ[KEY_FILE_ENV] = str(key_file)
    start_issuer(key)
    servers = []
    try:
        boot = ["-f", "schema.sql", *SERVER, "--perms", "perms.yml"]
        servers.append(start_server("n6k", PORTS["n6k"], [*boot, "--api-key", TOKEN]))
        servers.append(start_server("quack", PORTS["quack"], ["quack", *boot, "--token", TOKEN]))
        oidc = ["--oidc-issuer-url", ISSUER, "--oidc-audience", AUDIENCE]
        jwt_args = ["-f", "jwt_schema.sql", *SERVER, "--perms", "jwt_perms.yml", *oidc]
        servers.append(start_server("n6k_jwt", JWT_PORT, jwt_args))
        counts = {kind: dict.fromkeys(LEGEND, 0) for kind in PORTS}
        section = None
        rows = []
        details = []
        for index, item in enumerate(CHECKS):
            log(f"{index + 1}/{len(CHECKS)} {item.section}: {item.name}")
            cells = {}
            for kind in PORTS:
                if kind not in item.targets:
                    cells[kind] = NOT_APPLICABLE
                    continue
                result = launch(index, kind)
                if "error" in result:
                    cells[kind] = ERROR
                    details.append(f"- {ERROR} **{kind}, {item.name}:** `{result['error']}`")
                elif result["value"] == repr(item.expect):
                    cells[kind] = PASS
                else:
                    cells[kind] = BROKEN
                    details.append(
                        f"- {BROKEN} **{kind}, {item.name}:** got `{result['value']}`, expected `{item.expect!r}`"
                    )
                counts[kind][cells[kind]] += 1
            if item.section != section:
                section = item.section
                rows.append(f"\n## {section}\n\n| check | n6k | quack |\n| --- | --- | --- |")
            rows.append(f"| {item.name} | {cells['n6k']} | {cells['quack']} |")
    finally:
        for proc in servers:
            proc.kill()
            proc.wait()
    print("# Security results\n")
    print(f"Run on {time.strftime('%Y-%m-%d')}, with `--perms` and `--lockdown`. {NOT_APPLICABLE} means not applicable.\n")
    print("| | n6k | quack |\n| --- | --- | --- |")
    for mark, meaning in LEGEND.items():
        print(f"| {mark} {meaning} | {counts['n6k'][mark]} | {counts['quack'][mark]} |")
    print("\n".join(rows))
    print("\n## Details\n")
    print("\n".join(details) if details else "Every check holds.")


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--check":
        run_check(int(sys.argv[2]), sys.argv[3])
    else:
        main()
