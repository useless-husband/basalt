"""The same pgbench-style TPC-B transaction against SQLite, for comparison.

    python3 bench/tpcb/sqlite_tpcb.py --scale 1 --clients 1,4 --duration 20

Each client is a thread with its own connection. SQLite allows one writer
at a time, so transactions start with BEGIN IMMEDIATE and wait (busy
timeout) for the write lock. Two durability settings are measured:

  full     journal_mode=WAL, synchronous=FULL, fullfsync=ON  (F_FULLFSYNC per
           commit on macOS, comparable to basalt's default)
  normal   journal_mode=WAL, synchronous=NORMAL, fullfsync=OFF (SQLite's usual
           WAL setting; not durable across power loss)
"""

import argparse
import os
import random
import sqlite3
import statistics
import tempfile
import threading
import time


def setup(path, scale):
    db = sqlite3.connect(path)
    db.execute("PRAGMA journal_mode=WAL")
    db.executescript("""
        CREATE TABLE pgbench_branches (bid INTEGER PRIMARY KEY, bbalance INTEGER, filler CHAR(88));
        CREATE TABLE pgbench_tellers (tid INTEGER PRIMARY KEY, bid INTEGER, tbalance INTEGER, filler CHAR(84));
        CREATE TABLE pgbench_accounts (aid INTEGER PRIMARY KEY, bid INTEGER, abalance INTEGER, filler CHAR(84));
        CREATE TABLE pgbench_history (tid INTEGER, bid INTEGER, aid INTEGER, delta INTEGER, mtime TIMESTAMP, filler CHAR(22));
    """)
    db.executemany("INSERT INTO pgbench_branches VALUES (?, 0, '')", [(b,) for b in range(1, scale + 1)])
    db.executemany("INSERT INTO pgbench_tellers VALUES (?, ?, 0, '')", [(t, (t - 1) // 10 + 1) for t in range(1, 10 * scale + 1)])
    db.executemany("INSERT INTO pgbench_accounts VALUES (?, ?, 0, '')",
                   [(a, (a - 1) // 100000 + 1) for a in range(1, 100000 * scale + 1)])
    db.commit()
    db.close()


def client(path, mode, scale, seed, deadline, out):
    db = sqlite3.connect(path, timeout=60, isolation_level=None, check_same_thread=False)
    if mode == "full":
        db.execute("PRAGMA synchronous=FULL")
        db.execute("PRAGMA fullfsync=ON")
    else:
        db.execute("PRAGMA synchronous=NORMAL")
        db.execute("PRAGMA fullfsync=OFF")
    r = random.Random(seed)
    lat = []
    while time.time() < deadline:
        aid = r.randint(1, 100000 * scale)
        bid = r.randint(1, scale)
        tid = r.randint(1, 10 * scale)
        delta = r.randint(-5000, 5000)
        t0 = time.perf_counter()
        db.execute("BEGIN IMMEDIATE")
        db.execute("UPDATE pgbench_accounts SET abalance = abalance + ? WHERE aid = ?", (delta, aid))
        db.execute("SELECT abalance FROM pgbench_accounts WHERE aid = ?", (aid,)).fetchone()
        db.execute("UPDATE pgbench_tellers SET tbalance = tbalance + ? WHERE tid = ?", (delta, tid))
        db.execute("UPDATE pgbench_branches SET bbalance = bbalance + ? WHERE bid = ?", (delta, bid))
        db.execute("INSERT INTO pgbench_history (tid, bid, aid, delta, mtime) VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)",
                   (tid, bid, aid, delta))
        db.execute("COMMIT")
        lat.append(time.perf_counter() - t0)
    db.close()
    out.extend(lat)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--scale", type=int, default=1)
    ap.add_argument("--clients", default="1,4")
    ap.add_argument("--duration", type=float, default=20)
    ap.add_argument("--modes", default="full,normal")
    args = ap.parse_args()
    print(f"# SQLite {sqlite3.sqlite_version}, scale {args.scale} ({100000 * args.scale} accounts)")
    print(f"{'mode':<8} {'clients':>7} {'tps':>10} {'p50 ms':>10} {'p99 ms':>10}")
    for mode in args.modes.split(","):
        for c in [int(x) for x in args.clients.split(",")]:
            with tempfile.TemporaryDirectory() as d:
                path = os.path.join(d, "bench.db")
                setup(path, args.scale)
                deadline = time.time() + args.duration
                lat = []
                threads = [threading.Thread(target=client, args=(path, mode, args.scale, i + 1, deadline, lat)) for i in range(c)]
                for t in threads:
                    t.start()
                for t in threads:
                    t.join()
                lat.sort()
                tps = len(lat) / args.duration
                p50 = lat[len(lat) // 2] * 1000 if lat else 0
                p99 = lat[int(0.99 * (len(lat) - 1))] * 1000 if lat else 0
                print(f"{mode:<8} {c:>7} {tps:>10.0f} {p50:>10.2f} {p99:>10.2f}")


if __name__ == "__main__":
    main()
