# basalt design

This document explains how basalt is put together, the problems that took
the most care, and the alternatives that were considered and rejected. It
assumes familiarity with the vocabulary of database internals; the
Traditional Chinese walkthrough (`導讀.zh-TW.md`) explains the same ideas
from the ground up.

## Layers

```
 client (psql, pgx, psycopg)
   │ PostgreSQL frontend/backend protocol v3
 internal/pgwire     startup, simple + extended query, COPY, cancel, errors
 internal/engine     sessions, transactions, DDL, ANALYZE, VACUUM, pg_catalog
 internal/sql        lexer, AST, recursive-descent parser
 internal/planner    binder → logical plan → rewrite → physical plan, EXPLAIN
 internal/expr       bound expressions, functions, aggregates, closure compiler
 internal/executor   Volcano iterators, joins, aggregation, DML, constraints
 internal/txn        transaction ids, snapshots, visibility, lock manager
 internal/catalog    schema objects and statistics (immutable versions)
 internal/storage    buffer pool, WAL, mini-transactions, heap, B+tree, clog,
                     checkpoints, recovery
 internal/vfs        file abstraction: OS (F_FULLFSYNC) and fault-injecting
 internal/types      values, OIDs, casts, text/binary formats, key encoding
```

Each package depends only on the ones below it. The executor never sees
SQL text and the storage engine never sees SQL types: it stores bytes.

## Storage

### Files

A database directory holds `basalt.db` (all tables, indexes, the commit
log, sequences and the catalog, in 8 KiB pages), `wal/*.wal` (log segments
named by the LSN of their first byte) and `basalt.control` (the redo start
point of the last checkpoint, written to one of two checksummed 512-byte
slots alternately so that a torn write can never destroy both).

Every page starts with a 16-byte header: the LSN of the last log record
that changed it, a CRC-32C of the page, and the page type. Checksums are
verified on every read from disk.

### Buffer pool

A fixed array of frames, a map from page id to frame, and clock (second
chance) eviction. A frame is pinned while in use and has a reader/writer
latch for its contents. Before a dirty page is written, the WAL is flushed
up to the page's LSN (the WAL rule). Writes always go from a private copy
of the page, with the checksum computed on the copy.

### Mini-transactions and the log

Every change to pages goes through a *mini-transaction* (mtr), the idea
InnoDB uses: the mtr latches the pages it changes exclusively, remembers
their before-images, and at commit produces **one** log record that
contains, for each changed page, either the byte ranges that changed or a
full page image. A B+tree split that touches five pages is therefore one
record: after a crash it is either entirely replayed or not at all, so the
tree is never half split. This also means the B+tree and heap code contain
no log-record types of their own; they simply edit bytes inside an mtr.

The first change to a page after a checkpoint is logged as a full page
image (PostgreSQL's `full_page_writes`). That is what makes torn page
writes harmless: recovery starts at the checkpoint, and the first record
it meets for any page that might have been torn is a complete copy of it.

Log records carry a CRC over their position and contents; recovery stops
at the first record that is incomplete or does not check out, and the
segment is truncated there.

**Group commit.** Appends go to an in-memory buffer. `Flush(lsn)` takes a
flush mutex, and if no earlier flush already covered `lsn` it writes the
whole buffer and syncs once. Committers that arrive while a sync is in
progress queue on the mutex and usually find their records already durable
when they get it. On macOS the sync is `fcntl(F_FULLFSYNC)`, because plain
`fsync(2)` there does not flush the drive's cache.

### Checkpoints

Checkpoints are *sharp*: they take the checkpoint lock exclusively (every
mtr holds it shared), start a new log segment, write all dirty pages,
sync the data file, record the new redo point in the control file and
delete older segments. Writers pause for the duration. A fuzzy checkpoint
(tracking each dirty page's first-dirtying LSN and letting writers
continue) was rejected for now: it adds a dirty-page table and a more
subtle recovery start point, and the sharp version made correctness easy
to argue while everything else was being built.

A checkpoint runs every `-checkpoint-interval` (default 30 s) or after
512 MiB of WAL. Frequent checkpoints cost throughput beyond the pause:
the next change to every page after a checkpoint logs a full 8 KiB image.
The size threshold was raised from 64 MiB to 512 MiB after TPC-B runs
showed the in-process load (about 150 MiB of WAL per second at 8
clients) checkpointing several times a second.

### Recovery: redo only

Recovery reads the control file, replays every log record from the redo
point, applying a page diff only if the page's LSN is older than the
record (so replay is idempotent), and then takes a checkpoint.

There is no undo pass. A transaction's status lives in the commit log
(two bits per transaction id, in ordinary pages, so the log protects it
too). A transaction whose commit record did not reach the log is still "in
progress" in the recovered commit log, and every id below the startup
high-water mark that is not committed is treated as aborted. Its row
versions remain on disk but are invisible under MVCC, exactly as in
PostgreSQL, and VACUUM reclaims them later. ARIES-style undo with
compensation records was considered and rejected: with MVCC it buys
nothing for row data, and structural changes are already atomic through
mini-transactions.

Transaction ids are 64-bit, so there is no wraparound and no freezing.
They are reserved in blocks of 1,024 with a logged update of the meta page
so that an id is never reused after a crash.

### Heap

A table is a chain of slotted pages: a slot array growing from the front,
tuples from the back. A tuple is a 26-byte MVCC header (`xmin`, `xmax`,
`cmin`, `cmax`, flags) followed by a self-describing row encoding. Row
identifiers (TIDs) are page and slot. Insertion goes to the last page or a
page VACUUM freed space in; extension is serialized per table, and the
latch order (last page, then first page, then the meta page) is fixed so
extension cannot deadlock with inserts.

Rows must fit in a page (about 8 KB); there is no TOAST.

### B+tree

Keys are byte strings compared with `bytes.Compare`. Each value is
encoded order-preservingly ("memcomparable": sign-flipped big-endian
integers, bit-twiddled floats, a sign/exponent/digits form for decimals,
escaped and terminated strings, NULLs sorting last), and the row's TID is
appended, so every key is unique even in a non-unique index and a delete
removes exactly one entry. The root page never moves: a root split copies
its contents into two new children. Deletion merges a node with a sibling
when the two fit in one page and collapses a root with a single child.

Concurrency is deliberately simple: one reader/writer lock per tree.
Writers hold it for one insert or delete; cursors hold it only while
copying a batch of keys and re-descend for the next batch, so a long scan
never blocks writers for long and needs no latch coupling. Concurrent
writers to the *same* index serialize, which is the main scalability limit
of this design (Lehman–Yao B-link trees would remove it).

`Check()` verifies the invariants (sorted keys, separator bounds, equal
leaf depth, an intact leaf chain) and is used by the randomized tests.

## Transactions

### Visibility

basalt is a multi-version store with PostgreSQL's tuple layout ideas.
A snapshot is `(xmin, xmax, active)`. A tuple is visible if its inserter
committed before the snapshot (or is the current transaction and an
earlier command) and its deleter did not. Commands within a transaction
are numbered; a statement sees the effects of earlier statements of its
own transaction but not its own, which is what prevents the Halloween
problem in `UPDATE t SET x = x + 1` without any special casing.

Commit statuses are looked up in an in-memory, lock-free mirror of the
commit log (atomic 32-bit words, 16 statuses each), loaded at startup and
updated at commit after the log is durable.

### Isolation levels

* **REPEATABLE READ** is snapshot isolation: one snapshot for the whole
  transaction. Updating or deleting a row that a transaction committed
  *after* our snapshot changed fails with SQLSTATE 40001 (first updater
  wins). If the other transaction is still running we wait for it; if it
  aborts we proceed.
* **READ COMMITTED** (the default, as in PostgreSQL) takes a new snapshot
  for every statement. When a statement runs into a row changed by a
  transaction that committed after the statement's snapshot, basalt undoes
  the statement's own changes so far (its new versions are marked dead,
  versions it locked are released) and runs the statement again on a new
  snapshot. PostgreSQL instead re-evaluates only the conflicting row
  (EvalPlanQual); re-running the statement gives the same guarantees and
  needs no row-version chain, at the price of repeating work under heavy
  contention.
* **SERIALIZABLE** is rejected with SQLSTATE 0A000 rather than silently
  weakened. Snapshot isolation allows write skew, and a test demonstrates
  it. Serializable snapshot isolation (SSI) is the obvious next step.

Two-phase locking for serializability was considered and rejected: it
makes readers block writers, which is a poor fit for an MVCC store whose
whole point is that readers never block.

### Locks and deadlocks

The lock manager grants table locks (a subset of PostgreSQL's modes),
key locks (for foreign keys), transaction-id locks (waiting for a
transaction to finish means acquiring a share lock on its id) and the
catalog lock (for DDL). Row "locks" are the `xmax` field of the row
itself, so they cost nothing until there is a conflict.

Whenever a request has to wait, the lock manager searches the wait-for
graph from the requester (edges to holders and to earlier queued requests
that conflict). If the search returns to the requester, that request fails
with 40P01. Choosing the requester as the victim is cheap and
deterministic. Lock waits observe statement cancellation and timeouts.

### Constraints under concurrency

* **Unique.** Inserting a key looks at every existing entry with the same
  key prefix while holding the index's write lock, and classifies each
  pointed-to version by its latest committed state: live means violation,
  dead is ignored, and a version whose inserter or deleter is still running
  means "wait for it and try again". An UPDATE that leaves a unique key
  unchanged skips the check: the locked old version owns the key.
* **Foreign keys.** A child insert takes a share lock on the referenced
  key (in the lock manager) and then checks that a live parent exists; a
  parent delete or key update takes an exclusive lock on the same key and
  then looks for live children. Because the two locks conflict, a
  concurrent insert and delete cannot both succeed. ON DELETE/UPDATE
  CASCADE and SET NULL are supported.

### Reclaiming space

VACUUM computes the oldest snapshot horizon, finds versions that no
current or future snapshot can see, deletes their index entries first and
only then frees the heap slots (so an index never points at a reused
slot), and compacts the page. Autovacuum runs it for tables whose deleted
row count crossed a threshold.

Because every UPDATE adds an index entry for the new version (there are no
HOT updates), a frequently updated row would otherwise drag a growing list
of dead entries through every lookup until VACUUM. Index scans and unique
checks therefore delete entries whose versions are dead to every snapshot
as soon as they meet them (similar in spirit to PostgreSQL's
`kill_prior_tuple`).

### Transactional DDL

DDL runs inside the session's transaction against a private copy of the
catalog. The new catalog is written in the *same mini-transaction as the
commit status*, so it becomes durable atomically with the commit, and it
is published to other sessions before the transaction's locks are
released. A rollback frees storage the DDL allocated; storage of dropped
or truncated relations is freed only after commit. Catalog changes are
serialized by a catalog lock held until commit, which takes part in
deadlock detection.

## SQL processing

### Parser

Hand-written recursive descent with precedence climbing for expressions,
following PostgreSQL's grammar and operator precedence (including `::`,
`IS`, `BETWEEN`, `OPERATOR(pg_catalog.~)` and `COLLATE`, which psql uses),
typed literals (`DATE '...'`), and the special forms (`EXTRACT`,
`SUBSTRING ... FROM ... FOR`, `TRIM`, `POSITION`). Errors carry the
character position so psql can draw its caret.

### Binder

The binder resolves names and types and produces a logical plan. Columns
are identified by **column ids** unique within the query rather than by
position. That decision made the rest of the planner much simpler: join
reordering and predicate pushdown change row layouts freely, physical
operators compute their layouts at the end, and a column that is not in an
operator's input is, by definition, an outer (correlated) reference
supplied by the enclosing operator.

Parameter types are inferred from context (`$1 = int_col`, `INSERT ...
VALUES ($1)`, casts), which pgx needs because it sends `Parse` without
types and asks for `ParameterDescription`. String literals stay untyped
until context fixes their type, as in PostgreSQL, so `'2024-01-01'`
becomes a date when compared with a date column.

Aggregation follows the SQL rules: grouped expressions are matched
structurally (top-down) and every other column reference outside an
aggregate is a grouping error.

### Rewrite

* Constant folding, including three-valued simplification of AND/OR and
  CASE arms with constant conditions; an expression that errors (`1/0`)
  is left unfolded so the error only happens if it is evaluated.
* Subquery decorrelation: a WHERE conjunct `EXISTS (...)`, `NOT EXISTS
  (...)` or `x IN (...)` whose subquery is correlated only through its
  top-level WHERE becomes a semi or anti join whose condition is the
  correlated conjuncts (plus `x = column` for IN). Forms whose semantics
  differ from a join are left as per-row subplans: `NOT IN` (a NULL in
  the subquery makes it unknown rather than true), subqueries under OR or
  NOT, and subqueries with LIMIT, aggregates or set operations.
* Predicate pushdown through projections (by substitution), into join
  inputs and join conditions, below GROUP BY for predicates on grouping
  columns, and into scans. A WHERE predicate that rejects NULLs from the
  nullable side turns a LEFT JOIN into an inner join.

### Physical planning and the cost model

Costs use PostgreSQL's units and default constants (sequential page = 1,
CPU per tuple 0.01, per operator 0.0025), except that `random_page_cost`
defaults to 1.1, the value usually recommended for SSDs, instead of 4.0:
basalt reads pages from its buffer pool or an SSD, and with 4.0 it chose
hash joins over index probes that were measured to be twice as fast.
Repeated index probes on the inner side of a nested loop are charged the
Mackert–Lohman estimate of distinct pages read over all probes, given
`effective_cache_size` (by default the buffer pool size), as PostgreSQL's
`cost_index` does. Both are settings.

Estimates come from ANALYZE statistics: row counts scaled to the current
table size, null fraction, distinct counts (Haas–Stokes estimator over a
30,000-row reservoir sample), most common values with frequencies, and
20-bucket equi-depth histograms with linear interpolation (numbers,
dates and timestamps). A lower and an upper bound on the same column are
combined into the fraction between them instead of being multiplied as
if independent; other conjuncts are assumed independent.

* Access paths: sequential scan, or an index scan using equality on a
  prefix of the index columns plus a range on the next one; values may be
  constants, parameters or outer columns.
* Join order: dynamic programming over subsets for up to eight relations
  (avoiding cross products while a join predicate exists), greedy above.
* Join methods: hash join, merge join (sorting inputs as needed), nested
  loop with a parameterized index scan on the inner side, and plain
  nested loop over a materialized inner side. LEFT, FULL, semi and anti
  joins are planned without reordering.
* `ORDER BY ... LIMIT` considers reading an index in order, and otherwise
  uses a top-N heap sort.
* `enable_hashjoin`, `enable_mergejoin`, `enable_nestloop`,
  `enable_seqscan` and `enable_indexscan` work like PostgreSQL's (a large
  penalty cost), which the tests use to force each algorithm.

EXPLAIN prints plans in PostgreSQL's text format; EXPLAIN ANALYZE wraps
every iterator to count rows, loops and time.

### Executor

Pull-based iterators. Expressions are compiled once per execution into Go
closures over the operator's input layout. Correlated subqueries that were
not turned into joins are re-executed with their outer columns bound;
uncorrelated ones are evaluated once per statement, and `x IN (subquery)`
builds a hash set. Table scans decode only the columns some operator
reads.

`numeric` values are exact decimals. Values whose coefficient fits in an
int64 take a fast path for addition, subtraction, multiplication,
comparison and rounding (checked for overflow, falling back to `math/big`),
and are stored in rows as a scale plus a varint; others use `math/big`.

## Wire protocol and catalog emulation

The server implements protocol 3.0 (it answers 3.2 requests with
NegotiateProtocolVersion), both query protocols, portal suspension, COPY
in text and CSV, CancelRequest, notices and the error fields drivers look
at (code, detail, hint, position, table, constraint). Results use the
binary format when the client asks and basalt implements it for the type.

psql's `\d` family runs ordinary SQL against `pg_catalog`. basalt answers
it with virtual tables generated from its catalog on demand (`pg_class`,
`pg_attribute`, `pg_index`, `pg_constraint`, `pg_type`, ... and empty ones
for features it lacks) plus the `pg_*` functions psql calls. The queries
psql sends are not special-cased: they go through the same parser, planner
and executor as any other query.

## Testing strategy

* Unit tests per package, with randomized tests where there are
  invariants: key encoding order versus SQL order, B+tree invariants under
  random inserts and deletes, recovery after randomized crashes of the
  fault-injecting file system (unsynced writes lost or torn).
* Engine tests for SQL behaviour, isolation anomalies, deadlocks,
  transactional DDL and planner estimates.
* Real clients: psql (golden output), pgx, psycopg.
* External oracles: SQLite's sqllogictest corpus and random queries
  compared with sqlite3.
* A crash test that SIGKILLs the server process under load and checks
  durability, atomicity and index/heap agreement after every restart.

## Trade-offs, summarized

| Decision | Chosen | Rejected | Why |
|---|---|---|---|
| Undo | none (MVCC + commit log) | ARIES undo | row versions of aborted transactions are already invisible |
| Log records | page diffs and images per mini-transaction | per-operation logical records | atomic multi-page changes with no per-structure redo code |
| Checkpoints | sharp | fuzzy | simpler argument for correctness; costs a write pause |
| B+tree concurrency | tree-level RW lock | latch coupling, B-link | simple and correct; limits concurrent writers per index |
| Isolation | RC and SI | 2PL serializable | readers never block writers |
| RC conflicts | restart the statement | EvalPlanQual | no version chains needed |
| Column references | query-wide ids | positions | free reordering of joins and pushdown |
| Catalog storage | JSON blob in pages | system tables | small catalogs, atomic replacement in one record |
| Numeric | exact decimal, int64 fast path, math/big beyond | float64 | exact PostgreSQL semantics; the fast path covers typical money and quantity values |
