# basalt

**A relational SQL database written from scratch in Go that speaks the PostgreSQL wire protocol.** `psql`, the Go driver `pgx` and Python's `psycopg` connect to it unchanged.

Everything below the protocol is basalt's own code: the page-based storage engine (buffer pool, write-ahead log, B+trees, crash recovery), MVCC transactions with snapshot isolation, the SQL parser, a cost-based planner and the executor. There is no embedded SQLite or PostgreSQL and no third-party SQL parser; the only dependencies are the client drivers used by the tests.

It exists to show, at a size one person can read (about 29,000 lines of Go), how the parts of a real database fit together, and to check them against real clients and against other engines rather than against its own assumptions. It is a single-node research and teaching system, not a place to keep data you care about.

[繁體中文說明](README.zh-TW.md) · [Design](docs/DESIGN.md) · [導讀（給初學者）](docs/導讀.zh-TW.md)

## A session with psql

The real output of `psql -a -f demo.sql` against basalt, with the script from [`docs/demo.sql`](docs/demo.sql) (nothing edited except trailing spaces):

```text
CREATE TABLE accounts (id serial PRIMARY KEY, owner text NOT NULL, balance numeric(12,2) NOT NULL CHECK (balance >= 0));
CREATE TABLE
CREATE TABLE transfers (id bigserial PRIMARY KEY, src int REFERENCES accounts, dst int REFERENCES accounts, amount numeric(12,2), at timestamptz DEFAULT now());
CREATE TABLE
INSERT INTO accounts (owner, balance) SELECT 'user' || g, 100 FROM generate_series(1, 10000) g;
INSERT 0 10000
\d accounts
                                Table "public.accounts"
 Column  |     Type      | Collation | Nullable |               Default
---------+---------------+-----------+----------+--------------------------------------
 id      | integer       |           | not null | nextval('accounts_id_seq'::regclass)
 owner   | text          |           | not null |
 balance | numeric(12,2) |           | not null |
Indexes:
    "accounts_pkey" PRIMARY KEY, btree (id)
Check constraints:
    "accounts_balance_check" CHECK (balance >= 0)
Referenced by:
    TABLE "transfers" CONSTRAINT "transfers_dst_fkey" FOREIGN KEY (dst) REFERENCES accounts(id)
    TABLE "transfers" CONSTRAINT "transfers_src_fkey" FOREIGN KEY (src) REFERENCES accounts(id)

BEGIN;
BEGIN
UPDATE accounts SET balance = balance - 25 WHERE id = 1;
UPDATE 1
UPDATE accounts SET balance = balance + 25 WHERE id = 2;
UPDATE 1
INSERT INTO transfers (src, dst, amount) VALUES (1, 2, 25) RETURNING id, src, dst, amount;
 id | src | dst | amount
----+-----+-----+--------
  1 |   1 |   2 |  25.00
(1 row)

INSERT 0 1
COMMIT;
COMMIT
UPDATE accounts SET balance = balance - 500 WHERE id = 1;
psql:demo.sql:10: ERROR:  new row for relation "accounts" violates check constraint "accounts_balance_check"
ANALYZE;
ANALYZE
EXPLAIN ANALYZE SELECT a.owner, sum(t.amount) FROM accounts a JOIN transfers t ON t.src = a.id WHERE a.id < 100 GROUP BY a.owner;
                                                              QUERY PLAN
---------------------------------------------------------------------------------------------------------------------------------------
 HashAggregate  (cost=5.04..5.05 rows=1 width=15) (actual time=0.015..0.015 rows=1 loops=1)
   Group Key: a.owner
   ->  Nested Loop  (cost=2.00..5.04 rows=1 width=63) (actual time=0.013..0.013 rows=1 loops=1)
         ->  Seq Scan on transfers t  (cost=0.00..1.01 rows=1 width=40) (actual time=0.002..0.002 rows=1 loops=1)
         ->  Index Scan using accounts_pkey on accounts a  (cost=2.00..4.02 rows=1 width=23) (actual time=0.007..0.007 rows=1 loops=1)
               Index Cond: (a.id = t.src)
               Filter: (a.id < 100)
 Execution Time: 0.022 ms
(8 rows)

SELECT count(*), sum(balance) FROM accounts;
 count |    sum
-------+------------
 10000 | 1000000.00
(1 row)
```

## What it implements

**Wire protocol v3.** Startup with protocol negotiation, trust or cleartext-password authentication, the simple and extended query protocols (Parse/Bind/Describe/Execute/Sync, parameter type inference, text and binary formats for the common types, portal suspension), COPY FROM STDIN / TO STDOUT in text and CSV, CancelRequest, notices, and errors with SQLSTATE codes, detail, hint, position, table and constraint fields. ReadyForQuery reports idle, in-transaction and failed-transaction states. Enough of `pg_catalog` and `information_schema` is emulated that psql's `\d`, `\dt`, `\d table`, `\d+`, `\di`, `\ds`, `\dn`, `\l` and `\du` produce PostgreSQL's output.

**SQL.** CREATE/DROP/TRUNCATE TABLE, CREATE/DROP INDEX, CREATE SEQUENCE, ALTER TABLE (ADD COLUMN, RENAME, ADD CONSTRAINT); PRIMARY KEY, NOT NULL, UNIQUE, CHECK and FOREIGN KEY (with ON DELETE/UPDATE CASCADE and SET NULL); serial and identity columns; INSERT with RETURNING and ON CONFLICT (DO NOTHING / DO UPDATE); UPDATE ... FROM; DELETE ... USING; SELECT with inner, left, right, full and cross joins, WHERE, GROUP BY/HAVING, aggregates (count, sum, avg, min, max, bool_and/or, string_agg, array_agg, stddev/variance, with DISTINCT, FILTER and ORDER BY), ORDER BY with NULLS FIRST/LAST, LIMIT/OFFSET, DISTINCT and DISTINCT ON, scalar, IN, EXISTS, ANY/ALL subqueries (correlated or not), non-recursive CTEs, UNION/INTERSECT/EXCEPT [ALL], CASE, COALESCE/NULLIF/GREATEST/LEAST, three-valued NULL logic; types smallint, integer, bigint, real, double precision, numeric(p,s) (arbitrary precision), text, varchar(n), char(n), boolean, date, timestamp, timestamptz, interval, bytea and one-dimensional arrays; about 120 built-in functions; EXPLAIN and EXPLAIN ANALYZE. DDL is transactional.

**Planner.** A binder that resolves names and infers parameter types, a logical plan, rewrite rules (constant folding, predicate pushdown into joins, scans and below GROUP BY, outer-to-inner join conversion), and a cost-based physical planner using ANALYZE statistics (row counts, null fractions, distinct counts, most common values, histograms). It chooses between sequential and index scans, orders joins by dynamic programming over subsets (greedy above eight tables), and picks hash, merge, nested-loop or index nested-loop joins; ORDER BY ... LIMIT can read an index in order or use a top-N sort.

**Storage.** One data file of 8 KiB checksummed pages, a buffer pool with clock eviction, slotted heap pages, B+tree indexes with splits and merges, a segmented write-ahead log with group commit, mini-transactions that log page diffs or full-page images atomically (so a crash never leaves a B+tree half split and torn page writes are repaired), sharp checkpoints, redo recovery, and `F_FULLFSYNC` on macOS.

**Transactions.** MVCC with 64-bit transaction ids. READ COMMITTED (the default, as in PostgreSQL: a snapshot per statement; a statement that meets a row changed by a later-committed transaction undoes itself and restarts on a new snapshot) and REPEATABLE READ (snapshot isolation: one snapshot per transaction, first updater wins with SQLSTATE 40001). SERIALIZABLE is refused rather than silently weakened. A lock manager with wait-for-graph deadlock detection (40P01), concurrent sessions, VACUUM and autovacuum.

## How it works

```text
  psql · pgx · psycopg
          │  PostgreSQL protocol v3
  ┌───────▼──────────────────────────────────────────────────────────┐
  │ pgwire    startup · simple/extended query · COPY · cancel        │
  │ engine    sessions · transactions · DDL · ANALYZE · VACUUM ·     │
  │           pg_catalog emulation                                   │
  │ sql ──► planner: binder → logical plan → rewrite → cost-based    │
  │         physical plan ──► executor: Volcano iterators, DML,      │
  │         constraints                                              │
  ├──────────────────────────────────────────────────────────────────┤
  │ txn       snapshots · visibility · commit log · lock manager     │
  ├──────────────────────────────────────────────────────────────────┤
  │ storage   buffer pool · heap pages · B+trees · mini-transactions │
  │           → WAL (group commit) · checkpoints · redo recovery     │
  └───────▲─────────────────────────────▲────────────────────────────┘
     basalt.db (8 KiB pages)       wal/*.wal segments
```

A few ideas carry most of the weight (details and the alternatives considered are in [docs/DESIGN.md](docs/DESIGN.md)):

- **Mini-transactions.** Every page change is made inside a mini-transaction that becomes one log record holding the changed byte ranges (or a full image on the first change after a checkpoint). Multi-page operations are atomic in the log without any per-structure redo code.
- **Redo only, thanks to MVCC.** Uncommitted row versions are left on disk after a crash; the recovered commit log marks their transactions aborted, so they are invisible, and VACUUM removes them later. This is PostgreSQL's approach, and it removes the need for an undo pass.
- **Column ids instead of positions.** The binder gives every column of a query a unique id. Join reordering and predicate pushdown change row layouts freely, and a column missing from an operator's input is by definition an outer reference of a correlated subquery or a parameterized index scan.
- **No special case for psql.** The catalog queries psql sends go through the same parser, planner and executor as user queries, against virtual `pg_catalog` tables generated from basalt's catalog.

## Evidence

Every claim above is backed by something you can run. Results below were measured on the machine described under Benchmarks.

| What | Command | Result |
|---|---|---|
| Unit and integration tests (all packages) | `make test`, `make race` | pass; CI runs them with `-race` on Linux and macOS |
| Real psql client | `make psql-test` | 265 lines of psql output (DDL, `\d`, `\copy`, joins, errors with SQLSTATEs, transactions, EXPLAIN) identical to [`test/psql/basic.expected`](test/psql/basic.expected) |
| Go driver pgx | `go test ./internal/pgwire` | binary-format type round trips (numeric, timestamps, bytea, interval), batches, error fields, transactions, COPY, cancellation, concurrent clients |
| Python driver psycopg 3 | `make python-test` | 7 tests: types, error classes, transactions, COPY in and out, isolation, identifiers, cancellation |
| SQLite's sqllogictest corpus | `make slt` | **382,491 of 382,574 records pass (99.98%)** over 57 files: `select1`–`select5` 10,706/10,706, `random/*` (first 10 files of each family) 371,516/371,544, `evidence/*` 269/324; 144,203 records the corpus marks `skipif postgresql` are skipped |
| Differential testing against sqlite3 | `make difftest` | 35,000 random queries (seeds 1–8) over random tables with NULLs: identical results |
| Crash safety (process kill) | `make crash-test` | 30 rounds of `SIGKILL` by PID under 4 concurrent writers: every acknowledged commit present, no rolled-back or never-committed row visible, money conserved, index and table agree |
| Crash safety (power loss model) | `go test ./internal/storage -run Recovery` | 30 randomized crashes of the fault-injecting file system (unsynced writes lost, one torn): every flushed record recovered, B+tree invariants hold |
| Isolation | `go test ./internal/engine -run 'No\|Skew\|Deadlock\|ReadCommitted\|Bank'` | REPEATABLE READ prevents dirty and non-repeatable reads, phantoms, lost updates and read skew; write skew is shown to be possible; deadlocks are detected; 16 clients transfer money while auditors see a constant total |
| Planner uses statistics | `go test ./internal/engine -run Planner` | estimates within 30% of actual row counts; index vs sequential scan, hash build side, index-ordered LIMIT and merge join chosen as expected |

The sqllogictest failures are of two kinds: places where PostgreSQL's semantics differ from SQLite's and the corpus does not mark them (integer overflow and division by zero raise errors; `NULLIF(int, numeric)` is numeric), and features basalt does not have (triggers, views, `REPLACE`, `REINDEX`). The runner prints every failure reason; the corpus is downloaded at test time at a pinned commit and is not stored in this repository.

## Benchmarks

All numbers were measured on one machine: Apple M5 (10 cores), 16 GB RAM, internal SSD (APFS), macOS 27.0, Go 1.27.1, SQLite 3.45.3 (Python's `sqlite3` module), DuckDB 1.5.5. The machine was shared with other build jobs while measuring, so repeated runs vary by roughly 10 to 20 percent; each TPC-B figure is one 20-second run and each query time is the median of five runs. The raw output of every run is reproduced by the commands shown.

### TPC-B (pgbench's transaction)

`bench/tpcb` implements pgbench's built-in transaction: inside BEGIN/COMMIT, update an account, read it back, update a teller and a branch, insert a history row. Scale 8 (800,000 accounts, 8 branches), one connection per client, statements prepared (pgx's statement cache). `bench/tpcb/sqlite_tpcb.py` runs the same transaction against SQLite with `BEGIN IMMEDIATE`. The embedded mode calls basalt's engine directly and checks TPC-B consistency (account, teller, branch and history totals agree) after every run.

```sh
go run ./bench/tpcb -basalt bin/basalt -scale 8 -clients 1,8,16 -duration 20s               # durable
go run ./bench/tpcb -basalt bin/basalt -scale 8 -clients 1,8,16 -duration 20s -no-fsync     # no fsync
go run ./bench/tpcb -embedded -scale 8 -clients 1,8 -duration 20s                            # in-process
python3 bench/tpcb/sqlite_tpcb.py --scale 8 --clients 1,8 --duration 20
```

Transactions per second:

| Configuration | 1 client | 8 clients | 16 clients |
|---|---:|---:|---:|
| basalt over TCP, durable (F_FULLFSYNC, group commit) | 218 | 608 | 756 |
| SQLite in-process, WAL, `synchronous=FULL`, `fullfsync=ON` | 247 | 238 | |
| basalt over TCP, `-unsafe-no-fsync` | 1,900 | 12,132 | 15,957 |
| basalt in-process, no network, no fsync | 2,617 | 33,288 | |
| SQLite in-process, WAL, `synchronous=NORMAL` (no fsync per commit) | 30,567 | 31,144 | |

What this shows, plainly:

- **Durable commits are limited by the drive flush.** `F_FULLFSYNC` takes about 4 ms here, so one client gets roughly 250 commits per second on either engine. With more clients basalt's group commit shares each flush among the transactions waiting for it (608 at 8 clients, 756 at 16), while SQLite's single writer serializes them. The remaining limit for basalt is contention on the 8 branch rows, each held until its transaction's flush completes.
- **Without the flush, SQLite is much faster per transaction.** In-process with one client, SQLite runs about 30,000 transactions per second and basalt about 2,600, roughly 12 times fewer: basalt writes a new row version and index entries for every update and logs page diffs, and it executes plans through generic iterators. Over TCP basalt also pays seven round trips per transaction (1,900 with one client). basalt scales across clients (33,288 in-process with 8) where SQLite does not, but that comparison is not like for like: SQLite does not try to run writers concurrently.
- One client in-process takes 0.35 ms per transaction while each of eight takes 0.20 ms; I have not established why a single client gets less work done per transaction (CPU scheduling on the M5's mixed cores is a plausible cause I did not verify).

### Analytical queries

`bench/analytics/run.py` generates a small TPC-H-like data set with a fixed seed (15,000 customers, 150,000 orders, 599,648 line items), loads identical rows into the three engines (primary keys only, ANALYZE run), checks that all three return the same results, and times seven queries. basalt is measured through its wire protocol with psycopg, SQLite in-process, DuckDB with its CLI's timer.

```sh
python3 bench/analytics/run.py --basalt bin/basalt --scale 1 --repeat 5 --plans
```

| Query | basalt ms | SQLite ms | DuckDB ms | basalt / SQLite |
|---|---:|---:|---:|---:|
| Q1 scan + GROUP BY over line items | 362.8 | 138.9 | 3.0 | 2.6x |
| Q2 join + GROUP BY | 44.9 | 15.6 | 1.0 | 2.9x |
| Q3 three-way join + top 10 | 239.0 | 23.7 | 2.0 | 10.1x |
| Q4 correlated EXISTS | 67.2 | 13.4 | 2.0 | 5.0x |
| Q5 IN (subquery) | 33.9 | 13.0 | 1.0 | 2.6x |
| Q6 count(DISTINCT) | 200.1 | 72.3 | 2.0 | 2.8x |
| Q7 primary key lookup | 0.1 | 0.0 | 0.0 | |

basalt is 2.6 to 10 times slower than SQLite on these queries and two orders of magnitude slower than DuckDB, a vectorized columnar engine. Loading took 3.6 s in basalt (COPY over the wire), 0.8 s in SQLite and 0.4 s in DuckDB. The main reasons are known: one row at a time through iterators and closures, `numeric` arithmetic in arbitrary precision (Q1 and Q3 compute `price * (1 - discount)` for hundreds of thousands of rows; SQLite uses floating point), and no subquery decorrelation (Q4 runs an index lookup per qualifying order).

## Limitations

- Single node, one database, one schema (`public`). No roles or privileges, no TLS; authentication is trust or a single cleartext password (`-password`). Bind it to localhost.
- No SERIALIZABLE isolation (SSI); REPEATABLE READ allows write skew, like PostgreSQL's. No savepoints, `SELECT ... FOR UPDATE`, `LOCK`, `LISTEN/NOTIFY` (accepted and ignored) or prepared transactions.
- Not implemented: views, triggers, stored procedures, window functions, recursive CTEs, LATERAL, `DROP COLUMN`, partial or expression indexes, descending index order and backward index scans, multi-dimensional arrays, binary COPY, time zones other than UTC, collations other than C.
- Rows must fit in a page (about 8 KB); there is no TOAST.
- Subqueries are not decorrelated into joins: a correlated EXISTS runs its subplan once per outer row (through an index when one exists).
- `numeric` uses arbitrary-precision arithmetic (`math/big`), which is exact but slow.
- Sharp checkpoints pause writes while dirty pages are flushed. B+tree writers serialize per index. DDL is serialized by a catalog lock held until commit.
- A crash while DROP or TRUNCATE frees storage can leak pages (never corrupt data). There is no `VACUUM FULL` (it runs a plain VACUUM).
- Results are materialized before being sent to the client.
- Crash safety was tested by killing the process and by a model of power loss (the fault-injecting file system), not by cutting power to a machine.

## Related work

- **[PostgreSQL](https://www.postgresql.org/)** is the reference: basalt reimplements its protocol, SQLSTATEs, catalog shapes and many of its mechanisms (xmin/xmax MVCC, a commit log, full-page writes, READ COMMITTED and REPEATABLE READ semantics) in a much smaller form, and differs in places documented above (statement restart instead of EvalPlanQual, sharp checkpoints, no HOT updates).
- **[SQLite](https://sqlite.org/)** is a single-file embedded database; basalt uses its sqllogictest corpus and the `sqlite3` shell as test oracles and compares against it in the benchmarks.
- **[CockroachDB](https://github.com/cockroachdb/cockroach)** is a production distributed SQL database in Go that speaks pgwire and has a cost-based optimizer; basalt is single-node and orders of magnitude smaller.
- **[toydb](https://github.com/erikgrinaker/toydb)** (Rust) is an educational distributed SQL database with Raft and MVCC. basalt has no replication; it focuses instead on a page-based storage engine with a WAL and on compatibility with unmodified PostgreSQL clients.
- **[BusTub](https://github.com/cmu-db/bustub)** (CMU 15-445) is a teaching skeleton of buffer pool, B+tree and executors that students complete; basalt is a complete system with a network protocol and SQL front end.
- **[go-mysql-server](https://github.com/dolthub/go-mysql-server)** is a MySQL-compatible SQL engine in Go with pluggable storage; **[DuckDB](https://duckdb.org/)** is a columnar, vectorized analytical engine, included in the benchmarks as a reference point for how far a row-at-a-time engine is from the state of the art.

To my knowledge, few from-scratch teaching databases run psql's own catalog queries unmodified; basalt does it by answering them with ordinary SQL execution rather than by special-casing them.

## Build and run

Requires Go 1.25 or later.

```sh
make build                                 # bin/basalt
bin/basalt -D ./data -listen 127.0.0.1:5433
psql -h 127.0.0.1 -p 5433 -U me -d demo    # any user and database name
```

Flags: `-D` data directory, `-listen` address (port 0 picks a free one and prints it), `-password`, `-pool-mb` (buffer pool, default 128), `-checkpoint-interval`, `-autovacuum-interval`, `-v`, and `-unsafe-no-fsync` (benchmarks only).

```sh
make test          # unit and integration tests
make race          # with the race detector
make lint          # gofmt, go vet, staticcheck
make psql-test     # drive the real psql (set PSQL=/path/to/psql if needed)
make python-test   # psycopg 3 (pip install "psycopg[binary]")
make crash-test    # SIGKILL the server under load, restart, verify
make slt           # sqllogictest subset (downloads about 60 MB)
make difftest      # random queries compared with sqlite3
make bench         # TPC-B and analytical benchmarks
```

## License

[MIT](LICENSE)
