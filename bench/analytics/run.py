"""Analytical queries on the same generated data in basalt, SQLite and DuckDB.

    python3 bench/analytics/run.py --basalt bin/basalt [--scale 1] [--repeat 3]

Requires psycopg (to talk to basalt) and, optionally, the duckdb CLI. The
data is a small TPC-H-like schema generated with a fixed seed:
customers (15,000 x scale), orders (150,000 x scale) and lineitem (about
600,000 x scale). Every engine gets the same rows and primary keys and no
other indexes. Each query runs --repeat times; the median is reported.
basalt is measured through its wire protocol; SQLite in-process through
Python's sqlite3 module; DuckDB through its CLI's own timer.
"""

import argparse
import csv
import datetime
import os
import random
import re
import shutil
import sqlite3
import statistics
import subprocess
import sys
import tempfile
import time

import psycopg

QUERIES = [
    ("Q1 scan + group by",
     """SELECT l_flag, count(*), sum(l_qty), sum(l_price * (1 - l_disc)), avg(l_disc)
        FROM lineitem WHERE l_ship <= '1998-09-01' GROUP BY l_flag ORDER BY l_flag"""),
    ("Q2 join + group by",
     """SELECT c_nation, sum(o_total) FROM customers JOIN orders ON o_cust = c_id
        WHERE o_date >= '1995-01-01' AND o_date < '1996-01-01' GROUP BY c_nation ORDER BY 2 DESC"""),
    ("Q3 3-way join + top 10",
     """SELECT o_id, sum(l_price * (1 - l_disc)) AS rev
        FROM customers JOIN orders ON o_cust = c_id JOIN lineitem ON l_order = o_id
        WHERE c_segment = 'BUILDING' AND o_date < '1995-03-15' AND l_ship > '1995-03-15'
        GROUP BY o_id ORDER BY rev DESC, o_id LIMIT 10"""),
    ("Q4 correlated EXISTS",
     """SELECT o_status, count(*) FROM orders
        WHERE o_date >= '1993-07-01' AND o_date < '1993-10-01'
          AND EXISTS (SELECT 1 FROM lineitem WHERE l_order = o_id AND l_qty > 45)
        GROUP BY o_status ORDER BY o_status"""),
    ("Q5 IN (subquery)",
     """SELECT count(*) FROM orders WHERE o_cust IN (SELECT c_id FROM customers WHERE c_nation = 3)"""),
    ("Q6 count(DISTINCT)", "SELECT count(DISTINCT l_part) FROM lineitem"),
    ("Q7 primary key lookup", "SELECT o_total, o_date FROM orders WHERE o_id = 77777"),
    # The same EXISTS on a column without an index, first as written (basalt
    # turns it into a semi join), then hidden under OR so that it stays a
    # subquery executed once per customer.
    ("Q8 EXISTS, no index",
     """SELECT count(*) FROM customers WHERE c_id <= 300
          AND EXISTS (SELECT 1 FROM orders WHERE o_cust = c_id AND o_total > 40000)"""),
    ("Q9 Q8 as a subplan",
     """SELECT count(*) FROM customers WHERE c_id <= 300
          AND (c_id < 0 OR EXISTS (SELECT 1 FROM orders WHERE o_cust = c_id AND o_total > 40000))"""),
]


def generate(dirname, scale, seed=42):
    r = random.Random(seed)
    nc, no = 15000 * scale, 150000 * scale
    segs = ["AUTOMOBILE", "BUILDING", "FURNITURE", "HOUSEHOLD", "MACHINERY"]
    start = datetime.date(1992, 1, 1)
    with open(os.path.join(dirname, "customers.csv"), "w", newline="") as f:
        w = csv.writer(f)
        for c in range(1, nc + 1):
            w.writerow([c, r.randrange(25), r.choice(segs)])
    lines = 0
    with open(os.path.join(dirname, "orders.csv"), "w", newline="") as fo, \
            open(os.path.join(dirname, "lineitem.csv"), "w", newline="") as fl:
        wo, wl = csv.writer(fo), csv.writer(fl)
        for o in range(1, no + 1):
            od = start + datetime.timedelta(days=r.randrange(2400))
            total = 0
            for ln in range(1, r.randint(1, 7) + 1):
                qty = r.randint(1, 50)
                price = round(r.uniform(900, 100000) / 100, 2)
                disc = r.randint(0, 10) / 100
                ship = od + datetime.timedelta(days=r.randint(1, 120))
                flag = r.choice("ANR")
                wl.writerow([o, ln, r.randrange(20000 * scale), qty, f"{price:.2f}", f"{disc:.2f}", ship.isoformat(), flag])
                total += price * qty
                lines += 1
            wo.writerow([o, r.randint(1, nc), od.isoformat(), r.choice("FOP"), f"{total:.2f}"])
    return nc, no, lines


SCHEMA = [
    "CREATE TABLE customers (c_id int PRIMARY KEY, c_nation int, c_segment text)",
    "CREATE TABLE orders (o_id int PRIMARY KEY, o_cust int, o_date date, o_status char(1), o_total numeric(12,2))",
    "CREATE TABLE lineitem (l_order int, l_line int, l_part int, l_qty int, l_price numeric(12,2), l_disc numeric(4,2),"
    " l_ship date, l_flag char(1), PRIMARY KEY (l_order, l_line))",
]


def median_time(fn, repeat):
    times = []
    result = None
    for _ in range(repeat):
        t0 = time.perf_counter()
        result = fn()
        times.append(time.perf_counter() - t0)
    return statistics.median(times), result


def run_basalt(binary, data, repeat):
    work = tempfile.mkdtemp(prefix="basalt-analytics-")
    proc = subprocess.Popen([binary, "-D", os.path.join(work, "db"), "-listen", "127.0.0.1:0", "-pool-mb", "1024",
                             "-autovacuum-interval", "0"], stdout=subprocess.PIPE, text=True)
    try:
        port = int(re.search(r":(\d+) ", proc.stdout.readline()).group(1))
        conn = psycopg.connect(f"host=127.0.0.1 port={port} user=bench dbname=bench", autocommit=True)
        t0 = time.perf_counter()
        for s in SCHEMA:
            conn.execute(s)
        with conn.cursor() as cur:
            for table in ("customers", "orders", "lineitem"):
                with open(os.path.join(data, table + ".csv")) as f, cur.copy(f"COPY {table} FROM STDIN WITH (FORMAT csv)") as cp:
                    while chunk := f.read(1 << 20):
                        cp.write(chunk)
        conn.execute("ANALYZE")
        load = time.perf_counter() - t0
        out = {}
        for name, q in QUERIES:
            out[name] = median_time(lambda: conn.execute(q).fetchall(), repeat)
        plans = {name: "\n".join(r[0] for r in conn.execute("EXPLAIN " + q).fetchall()) for name, q in QUERIES}
        conn.close()
        return load, out, plans
    finally:
        proc.terminate()
        proc.wait(timeout=60)
        shutil.rmtree(work, ignore_errors=True)


def run_sqlite(data, repeat):
    work = tempfile.mkdtemp(prefix="sqlite-analytics-")
    try:
        db = sqlite3.connect(os.path.join(work, "a.db"))
        t0 = time.perf_counter()
        for s in SCHEMA:
            db.execute(s)
        for table, n in (("customers", 3), ("orders", 5), ("lineitem", 8)):
            with open(os.path.join(data, table + ".csv")) as f:
                db.executemany(f"INSERT INTO {table} VALUES ({','.join('?' * n)})", csv.reader(f))
        db.commit()
        db.execute("ANALYZE")
        load = time.perf_counter() - t0
        out = {name: median_time(lambda: db.execute(q).fetchall(), repeat) for name, q in QUERIES}
        db.close()
        return load, out
    finally:
        shutil.rmtree(work, ignore_errors=True)


def run_duckdb(data, repeat):
    duck = shutil.which("duckdb")
    if not duck:
        return None
    work = tempfile.mkdtemp(prefix="duckdb-analytics-")
    try:
        path = os.path.join(work, "a.duckdb")
        load_sql = ";\n".join(SCHEMA) + ";\n" + "".join(
            f"COPY {t} FROM '{os.path.join(data, t + '.csv')}' (FORMAT csv);\n" for t in ("customers", "orders", "lineitem"))
        t0 = time.perf_counter()
        subprocess.run([duck, path], input=load_sql, text=True, check=True, capture_output=True)
        load = time.perf_counter() - t0
        out = {}
        for name, q in QUERIES:
            script = ".timer on\n" + (q + ";\n") * repeat
            res = subprocess.run([duck, path], input=script, text=True, check=True, capture_output=True)
            times = [float(x) for x in re.findall(r"Run Time \(s\): real ([0-9.]+)", res.stdout)]
            out[name] = (statistics.median(times), None)
        return load, out
    finally:
        shutil.rmtree(work, ignore_errors=True)


def same(a, b):
    """Compare result rows across engines (numbers with a tolerance)."""
    if a is None or b is None or len(a) != len(b):
        return a is None or b is None
    for ra, rb in zip(a, b):
        for x, y in zip(ra, rb):
            try:
                if abs(float(x) - float(y)) > 1e-6 * max(1, abs(float(y))):
                    return False
            except (TypeError, ValueError):
                if str(x) != str(y):
                    return False
    return True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--basalt", default="bin/basalt")
    ap.add_argument("--scale", type=int, default=1)
    ap.add_argument("--repeat", type=int, default=3)
    ap.add_argument("--plans", action="store_true", help="print basalt's EXPLAIN for each query")
    args = ap.parse_args()
    data = tempfile.mkdtemp(prefix="analytics-data-")
    try:
        nc, no, nl = generate(data, args.scale)
        print(f"# {nc} customers, {no} orders, {nl} line items (seed 42)")
        bl, b, plans = run_basalt(args.basalt, data, args.repeat)
        sl, s = run_sqlite(data, args.repeat)
        dk = run_duckdb(data, args.repeat)
        print(f"# load: basalt {bl:.1f}s (COPY over the wire), SQLite {sl:.1f}s (executemany), "
              + (f"DuckDB {dk[0]:.1f}s (COPY)" if dk else "DuckDB not installed"))
        print(f"{'query':<26} {'basalt ms':>10} {'SQLite ms':>10} {'DuckDB ms':>10} {'basalt/SQLite':>14}  results")
        for name, _ in QUERIES:
            bt, br = b[name]
            st, sr = s[name]
            dt = dk[1][name][0] if dk else float('nan')
            agree = "match" if same(br, sr) else "DIFFER"
            print(f"{name:<26} {bt*1000:>10.1f} {st*1000:>10.1f} {dt*1000:>10.1f} {bt/st:>13.1f}x  {agree}")
        if args.plans:
            for name, _ in QUERIES:
                print(f"\n-- {name}\n{plans[name]}")
    finally:
        shutil.rmtree(data, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
