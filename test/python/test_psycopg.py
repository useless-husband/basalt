"""Drive basalt with Python's psycopg 3 driver.

Run:  python -m pip install "psycopg[binary]"  &&  python test/python/test_psycopg.py

The test builds basalt, starts it on an ephemeral port in a temporary
directory, and stops it (by PID) when done.
"""

import datetime
import decimal
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest

import psycopg
from psycopg import errors, sql

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))


class Server:
    def __init__(self):
        self.dir = tempfile.mkdtemp(prefix="basalt-py-")
        binary = os.path.join(self.dir, "basalt")
        subprocess.run(["go", "build", "-o", binary, "./cmd/basalt"], cwd=ROOT, check=True)
        self.log = open(os.path.join(self.dir, "server.log"), "w+")
        self.proc = subprocess.Popen(
            [binary, "-D", os.path.join(self.dir, "data"), "-listen", "127.0.0.1:0"],
            stdout=subprocess.PIPE, stderr=self.log, text=True)
        line = self.proc.stdout.readline()
        m = re.search(r"listening on 127\.0\.0\.1:(\d+)", line)
        if not m:
            raise RuntimeError("server did not start: " + line)
        self.port = int(m.group(1))

    def dsn(self):
        return f"host=127.0.0.1 port={self.port} user=py dbname=py"

    def stop(self):
        self.proc.terminate()
        self.proc.wait(timeout=30)
        self.log.close()
        shutil.rmtree(self.dir, ignore_errors=True)


SERVER = None


class PsycopgTest(unittest.TestCase):
    def connect(self, **kw):
        return psycopg.connect(SERVER.dsn(), **kw)

    def test_types_and_parameters(self):
        with self.connect() as conn:
            conn.execute("DROP TABLE IF EXISTS items")
            conn.execute("""CREATE TABLE items (
                id serial PRIMARY KEY, name text NOT NULL, price numeric(8,2), qty int,
                ratio double precision, active bool, added date, seen timestamptz, blob bytea)""")
            now = datetime.datetime(2024, 5, 6, 7, 8, 9, 123000, tzinfo=datetime.timezone.utc)
            with conn.cursor() as cur:
                cur.executemany(
                    "INSERT INTO items (name, price, qty, ratio, active, added, seen, blob) "
                    "VALUES (%s, %s, %s, %s, %s, %s, %s, %s)",
                    [("apple", decimal.Decimal("1.25"), 10, 0.5, True, datetime.date(2024, 1, 2), now, b"\x00\x01"),
                     ("pear", decimal.Decimal("2.50"), None, 1.0, False, datetime.date(2024, 1, 3), now, None),
                     ("plum", None, 3, None, None, None, None, b"")])
                cur.execute("SELECT name, price, qty, ratio, active, added, seen, blob FROM items ORDER BY id")
                rows = cur.fetchall()
            self.assertEqual(rows[0], ("apple", decimal.Decimal("1.25"), 10, 0.5, True,
                                       datetime.date(2024, 1, 2), now, b"\x00\x01"))
            self.assertIsNone(rows[1][2])
            self.assertEqual(rows[2][7], b"")
            # Binary result format.
            with conn.cursor(binary=True) as cur:
                cur.execute("SELECT sum(price), max(added), count(*) FROM items WHERE qty > %s", (1,))
                self.assertEqual(cur.fetchone(), (decimal.Decimal("1.25"), datetime.date(2024, 1, 2), 2))

    def test_errors_map_to_exception_classes(self):
        with self.connect(autocommit=True) as conn:
            conn.execute("DROP TABLE IF EXISTS u")
            conn.execute("CREATE TABLE u (k int PRIMARY KEY)")
            conn.execute("INSERT INTO u VALUES (1)")
            with self.assertRaises(errors.UniqueViolation):
                conn.execute("INSERT INTO u VALUES (1)")
            with self.assertRaises(errors.UndefinedTable):
                conn.execute("SELECT * FROM nope")
            with self.assertRaises(errors.SyntaxError):
                conn.execute("SELEC 1")
            with self.assertRaises(errors.DivisionByZero):
                conn.execute("SELECT 1 / 0")
            self.assertEqual(conn.execute("SELECT count(*) FROM u").fetchone()[0], 1)

    def test_transactions(self):
        with self.connect() as conn:
            conn.execute("DROP TABLE IF EXISTS acct")
            conn.execute("CREATE TABLE acct (id int PRIMARY KEY, bal int)")
            conn.commit()
            with conn.transaction():
                conn.execute("INSERT INTO acct VALUES (1, 100), (2, 100)")
            try:
                with conn.transaction():
                    conn.execute("UPDATE acct SET bal = bal - 500 WHERE id = 1")
                    raise RuntimeError("abort")
            except RuntimeError:
                pass
            self.assertEqual(conn.execute("SELECT sum(bal) FROM acct").fetchone()[0], 200)
            conn.rollback()

    def test_copy(self):
        with self.connect() as conn:
            conn.execute("DROP TABLE IF EXISTS points")
            conn.execute("CREATE TABLE points (x int, label text)")
            with conn.cursor() as cur:
                with cur.copy("COPY points (x, label) FROM STDIN") as cp:
                    for i in range(1000):
                        cp.write_row((i, f"p{i}" if i % 10 else None))
                cur.execute("SELECT count(*), count(label), sum(x) FROM points")
                self.assertEqual(cur.fetchone(), (1000, 900, 499500))
                out = []
                with cur.copy("COPY points TO STDOUT") as cp:
                    for row in cp.rows():
                        out.append(row)
                self.assertEqual(len(out), 1000)
            conn.commit()

    def test_snapshot_isolation_between_connections(self):
        with self.connect(autocommit=True) as setup:
            setup.execute("DROP TABLE IF EXISTS si")
            setup.execute("CREATE TABLE si (id int PRIMARY KEY, v int)")
            setup.execute("INSERT INTO si VALUES (1, 1)")
        with self.connect() as a, self.connect() as b:
            self.assertEqual(a.execute("SELECT v FROM si WHERE id = 1").fetchone()[0], 1)
            b.execute("UPDATE si SET v = 2 WHERE id = 1")
            b.commit()
            # a's snapshot was taken by its first statement: still 1.
            self.assertEqual(a.execute("SELECT v FROM si WHERE id = 1").fetchone()[0], 1)
            # Updating a row changed after the snapshot fails (first updater wins).
            with self.assertRaises(errors.SerializationFailure):
                a.execute("UPDATE si SET v = 3 WHERE id = 1")
            a.rollback()
            self.assertEqual(a.execute("SELECT v FROM si WHERE id = 1").fetchone()[0], 2)

    def test_server_side_identifiers_and_sql_composition(self):
        with self.connect(autocommit=True) as conn:
            conn.execute(sql.SQL("DROP TABLE IF EXISTS {}").format(sql.Identifier("Mixed Case")))
            conn.execute(sql.SQL("CREATE TABLE {} ({} int)").format(sql.Identifier("Mixed Case"), sql.Identifier("Col")))
            conn.execute(sql.SQL("INSERT INTO {} VALUES (%s)").format(sql.Identifier("Mixed Case")), (5,))
            row = conn.execute('SELECT "Col" FROM "Mixed Case"').fetchone()
            self.assertEqual(row, (5,))

    def test_cancel(self):
        with self.connect(autocommit=True) as conn:
            timer = threading.Timer(0.3, conn.cancel)
            timer.start()
            start = time.time()
            with self.assertRaises(errors.QueryCanceled):
                conn.execute("SELECT count(*) FROM generate_series(1, 2000000000)")
            self.assertLess(time.time() - start, 10)
            self.assertEqual(conn.execute("SELECT 1").fetchone(), (1,))


if __name__ == "__main__":
    SERVER = Server()
    try:
        result = unittest.main(exit=False, verbosity=2).result
    finally:
        SERVER.stop()
    sys.exit(0 if result.wasSuccessful() else 1)
