# Changelog

## 0.1.0 (unreleased)

First version.

- PostgreSQL wire protocol v3 server: simple and extended query, binary and text formats, COPY (text, CSV), cancellation, SQLSTATE errors; catalog emulation for psql's `\d` family.
- SQL: DDL (transactional), constraints including foreign keys with cascades, INSERT/UPDATE/DELETE with RETURNING and ON CONFLICT, joins of all kinds, aggregation, subqueries, non-recursive CTEs, set operations, EXPLAIN [ANALYZE].
- Planner: binder with parameter type inference, constant folding, decorrelation of EXISTS/NOT EXISTS/IN into semi and anti joins, predicate pushdown, cost model using ANALYZE statistics (combined range bounds, cache-aware repeated index probes, `random_page_cost` and `effective_cache_size` settings), dynamic-programming join ordering, hash/merge/nested-loop/index nested-loop joins, index-ordered LIMIT and top-N sort.
- Executor: compiled expression closures, scans that decode only the columns read, an int64 fast path for `numeric`.
- Storage: 8 KiB checksummed pages, buffer pool, write-ahead log with mini-transactions, full-page images and group commit, sharp checkpoints, redo recovery, B+trees with splits and merges, F_FULLFSYNC on macOS.
- Transactions: MVCC with READ COMMITTED (statement restart on conflict) and REPEATABLE READ (snapshot isolation), deadlock detection, VACUUM and autovacuum.
- Tests: psql golden output, pgx, psycopg, sqllogictest subset, differential testing against sqlite3, SIGKILL crash test, fault-injecting file system, isolation anomaly tests. Benchmarks: TPC-B and analytical queries against SQLite.
