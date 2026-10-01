package engine

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/types"
	"github.com/useless-husband/basalt/internal/vfs"
)

func openMem(t *testing.T) (*DB, vfs.FS) {
	t.Helper()
	fs := vfs.NewFaultFS()
	db, err := Open(Options{Dir: "/db", FS: fs, PoolPages: 256, CheckpointInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	return db, fs
}

// q runs a statement and renders its rows as "a|b" lines.
func q(t *testing.T, s *Session, query string) string {
	t.Helper()
	res, err := s.Exec(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return render(res[len(res)-1])
}

func render(r *Result) string {
	var lines []string
	for _, row := range r.Rows {
		var parts []string
		for i, v := range row {
			if v.IsNull() {
				parts = append(parts, "NULL")
			} else {
				parts = append(parts, types.ToText(v, r.Columns[i].Type))
			}
		}
		lines = append(lines, strings.Join(parts, "|"))
	}
	return strings.Join(lines, "\n")
}

func qerr(t *testing.T, s *Session, query, code string) {
	t.Helper()
	_, err := s.Exec(query)
	if err == nil {
		t.Fatalf("%s: expected error %s", query, code)
	}
	if pgerr.Code(err) != code {
		t.Fatalf("%s: got %s (%v), want %s", query, pgerr.Code(err), err, code)
	}
}

func expect(t *testing.T, s *Session, query, want string) {
	t.Helper()
	if got := q(t, s, query); got != want {
		t.Fatalf("%s:\n got: %q\nwant: %q", query, got, want)
	}
}

func TestBasicSQL(t *testing.T) {
	db, _ := openMem(t)
	defer db.Close()
	s := db.NewSession("test", "test")
	q(t, s, `CREATE TABLE t (id int PRIMARY KEY, name text NOT NULL, score numeric(6,2), created date DEFAULT '2024-01-01')`)
	q(t, s, `INSERT INTO t VALUES (1, 'alice', 90.5), (2, 'bob', 72), (3, 'carol', NULL)`)
	expect(t, s, `SELECT id, name, score, created FROM t ORDER BY id`, "1|alice|90.50|2024-01-01\n2|bob|72.00|2024-01-01\n3|carol|NULL|2024-01-01")
	expect(t, s, `SELECT count(*), sum(score), avg(score) FROM t`, "3|162.50|81.2500000000000000")
	expect(t, s, `SELECT name FROM t WHERE score > 80 OR score IS NULL ORDER BY name`, "alice\ncarol")
	q(t, s, `UPDATE t SET score = score + 1 WHERE id = 2`)
	expect(t, s, `SELECT score FROM t WHERE id = 2`, "73.00")
	q(t, s, `DELETE FROM t WHERE id = 3`)
	expect(t, s, `SELECT count(*) FROM t`, "2")
	qerr(t, s, `INSERT INTO t VALUES (1, 'dup', 1)`, pgerr.UniqueViolation)
	qerr(t, s, `INSERT INTO t VALUES (4, NULL, 1)`, pgerr.NotNullViolation)
	qerr(t, s, `SELECT nope FROM t`, pgerr.UndefinedColumn)
	expect(t, s, `INSERT INTO t VALUES (5, 'eve', 1) RETURNING id, upper(name)`, "5|EVE")
	expect(t, s, `SELECT 1 + 2 * 3, 7 / 2, 7.0 / 2, 'a' || 'b', NULL IS NULL`, "7|3|3.5000000000000000|ab|t")
}

func TestJoinsAndSubqueries(t *testing.T) {
	db, _ := openMem(t)
	defer db.Close()
	s := db.NewSession("test", "test")
	q(t, s, `CREATE TABLE a (id int PRIMARY KEY, v text)`)
	q(t, s, `CREATE TABLE b (id int PRIMARY KEY, a_id int REFERENCES a(id), w int)`)
	q(t, s, `INSERT INTO a VALUES (1,'x'),(2,'y'),(3,'z')`)
	q(t, s, `INSERT INTO b VALUES (10,1,5),(11,1,6),(12,2,7)`)
	expect(t, s, `SELECT a.v, b.w FROM a JOIN b ON a.id = b.a_id ORDER BY b.w`, "x|5\nx|6\ny|7")
	expect(t, s, `SELECT a.v, count(b.id) FROM a LEFT JOIN b ON a.id = b.a_id GROUP BY a.v ORDER BY a.v`, "x|2\ny|1\nz|0")
	expect(t, s, `SELECT v FROM a WHERE id IN (SELECT a_id FROM b) ORDER BY v`, "x\ny")
	expect(t, s, `SELECT v FROM a WHERE NOT EXISTS (SELECT 1 FROM b WHERE b.a_id = a.id)`, "z")
	expect(t, s, `SELECT v, (SELECT max(w) FROM b WHERE b.a_id = a.id) FROM a ORDER BY v`, "x|6\ny|7\nz|NULL")
	expect(t, s, `WITH c AS (SELECT a_id, sum(w) AS s FROM b GROUP BY a_id) SELECT a.v, c.s FROM a JOIN c ON c.a_id = a.id ORDER BY 1`, "x|11\ny|7")
	expect(t, s, `SELECT id FROM a UNION SELECT a_id FROM b ORDER BY 1`, "1\n2\n3")
	expect(t, s, `SELECT id FROM a EXCEPT SELECT a_id FROM b`, "3")
	expect(t, s, `SELECT id FROM a INTERSECT SELECT a_id FROM b ORDER BY 1`, "1\n2")
	qerr(t, s, `INSERT INTO b VALUES (13, 9, 1)`, pgerr.ForeignKeyViolation)
	qerr(t, s, `DELETE FROM a WHERE id = 1`, pgerr.ForeignKeyViolation)
	expect(t, s, `SELECT CASE WHEN w > 5 THEN 'big' ELSE 'small' END, count(*) FROM b GROUP BY 1 ORDER BY 1`, "big|2\nsmall|1")
}

func TestTransactionsAndRecovery(t *testing.T) {
	fs := vfs.NewFaultFS()
	db, err := Open(Options{Dir: "/db", FS: fs, PoolPages: 128, CheckpointInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	s := db.NewSession("test", "test")
	q(t, s, `CREATE TABLE acct (id int PRIMARY KEY, bal int)`)
	q(t, s, `INSERT INTO acct SELECT g, 100 FROM generate_series(1, 500) g`)
	q(t, s, `BEGIN`)
	q(t, s, `UPDATE acct SET bal = bal - 10 WHERE id = 1`)
	q(t, s, `UPDATE acct SET bal = bal + 10 WHERE id = 2`)
	q(t, s, `COMMIT`)
	q(t, s, `BEGIN`)
	q(t, s, `UPDATE acct SET bal = 0`)
	q(t, s, `ROLLBACK`)
	expect(t, s, `SELECT sum(bal), min(bal), max(bal) FROM acct`, "50000|90|110")
	// Crash: unsynced writes are lost.
	db.Abandon()
	db2, err := Open(Options{Dir: "/db", FS: fs.Crash(nil2rand(), vfs.CrashOptions{KeepProbability: 0.3, TornProbability: 0.5}), PoolPages: 128, CheckpointInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	s2 := db2.NewSession("test", "test")
	expect(t, s2, `SELECT sum(bal), count(*) FROM acct`, "50000|500")
	expect(t, s2, `SELECT bal FROM acct WHERE id IN (1, 2) ORDER BY id`, "90\n110")
}

func nil2rand() *rand.Rand { return rand.New(rand.NewSource(1)) }

func TestTransactionalDDL(t *testing.T) {
	db, _ := openMem(t)
	defer db.Close()
	s := db.NewSession("test", "test")
	other := db.NewSession("test", "test")
	pagesBefore := db.Store().PageCount()

	// Rolled-back CREATE TABLE leaves nothing behind.
	q(t, s, `BEGIN`)
	q(t, s, `CREATE TABLE t (id int PRIMARY KEY, v text UNIQUE)`)
	q(t, s, `INSERT INTO t VALUES (1, 'a')`)
	expect(t, s, `SELECT count(*) FROM t`, "1")
	qerr(t, other, `SELECT * FROM t`, pgerr.UndefinedTable) // not visible before commit
	q(t, s, `ROLLBACK`)
	qerr(t, s, `SELECT * FROM t`, pgerr.UndefinedTable)
	free, err := db.Store().FreePages()
	if err != nil {
		t.Fatal(err)
	}
	if grown := int(db.Store().PageCount()-pagesBefore) - free; grown > 2 {
		t.Fatalf("rollback leaked %d pages", grown)
	}

	// Committed DDL and data appear together.
	q(t, s, `BEGIN`)
	q(t, s, `CREATE TABLE t (id serial PRIMARY KEY, v text)`)
	q(t, s, `INSERT INTO t (v) VALUES ('x'), ('y')`)
	q(t, s, `CREATE INDEX t_v ON t (v)`)
	q(t, s, `COMMIT`)
	expect(t, other, `SELECT id, v FROM t ORDER BY id`, "1|x\n2|y")

	// DROP and TRUNCATE inside a rolled-back transaction keep the data.
	q(t, s, `BEGIN`)
	q(t, s, `TRUNCATE t`)
	expect(t, s, `SELECT count(*) FROM t`, "0")
	q(t, s, `DROP TABLE t`)
	q(t, s, `ROLLBACK`)
	expect(t, other, `SELECT count(*) FROM t WHERE v = 'y'`, "1")

	// A failed statement inside the block rolls back the DDL with it.
	q(t, s, `BEGIN`)
	q(t, s, `ALTER TABLE t ADD COLUMN extra int DEFAULT 7`)
	qerr(t, s, `SELECT nosuch FROM t`, pgerr.UndefinedColumn)
	q(t, s, `COMMIT`)
	qerr(t, other, `SELECT extra FROM t`, pgerr.UndefinedColumn)
}

// TestAddColumnDefault checks that rows written before ADD COLUMN ... DEFAULT
// show the default on every path that reads them.
func TestAddColumnDefault(t *testing.T) {
	db, _ := openMem(t)
	defer db.Close()
	s := db.NewSession("t", "t")
	q(t, s, `CREATE TABLE p (id int PRIMARY KEY)`)
	q(t, s, `CREATE TABLE c (id int PRIMARY KEY, pid int REFERENCES p ON UPDATE CASCADE)`)
	q(t, s, `INSERT INTO p VALUES (1), (2)`)
	q(t, s, `INSERT INTO c VALUES (10, 1), (20, 2)`)
	q(t, s, `ALTER TABLE c ADD COLUMN flag text DEFAULT 'old'`)
	q(t, s, `INSERT INTO c VALUES (30, 1, 'new')`)
	expect(t, s, `SELECT id, flag FROM c ORDER BY id`, "10|old\n20|old\n30|new")
	// The cascade rewrites child rows; the default must survive.
	q(t, s, `UPDATE p SET id = 5 WHERE id = 1`)
	expect(t, s, `SELECT id, pid, flag FROM c ORDER BY id`, "10|5|old\n20|2|old\n30|5|new")
	q(t, s, `CREATE INDEX c_flag ON c (flag)`)
	q(t, s, `SET enable_seqscan = off`)
	expect(t, s, `SELECT count(*) FROM c WHERE flag = 'old'`, "2")
	q(t, s, `ANALYZE c`)
	q(t, s, `VACUUM c`)
	expect(t, s, `SELECT count(*) FROM c WHERE flag = 'old'`, "2")
	cs := db.Catalog().TableByName("c").Stats.Columns[2]
	if cs.NullFrac != 0 {
		t.Fatalf("statistics see NULLs in a defaulted column: %+v", cs)
	}
}

func TestForeignKeyActions(t *testing.T) {
	db, _ := openMem(t)
	defer db.Close()
	s := db.NewSession("t", "t")
	q(t, s, `CREATE TABLE country (code char(2) PRIMARY KEY)`)
	q(t, s, `CREATE TABLE city (id int PRIMARY KEY, country char(2) REFERENCES country ON DELETE CASCADE ON UPDATE CASCADE)`)
	q(t, s, `CREATE TABLE street (id int PRIMARY KEY, city int REFERENCES city ON DELETE CASCADE)`)
	q(t, s, `CREATE TABLE note (id int PRIMARY KEY, city int REFERENCES city ON DELETE SET NULL)`)
	q(t, s, `CREATE TABLE pin (id int PRIMARY KEY, city int REFERENCES city)`) // NO ACTION
	q(t, s, `INSERT INTO country VALUES ('TW'), ('JP')`)
	q(t, s, `INSERT INTO city VALUES (1, 'TW'), (2, 'TW'), (3, 'JP')`)
	q(t, s, `INSERT INTO street VALUES (100, 1), (101, 1), (102, 3)`)
	q(t, s, `INSERT INTO note VALUES (7, 2)`)
	q(t, s, `INSERT INTO pin VALUES (9, 3)`)
	// Cascading delete two levels deep; SET NULL on a third table.
	q(t, s, `DELETE FROM city WHERE id = 1 OR id = 2`)
	expect(t, s, `SELECT count(*) FROM street`, "1")
	expect(t, s, `SELECT city IS NULL FROM note`, "t")
	// NO ACTION blocks deleting a referenced row, also through a cascade.
	qerr(t, s, `DELETE FROM city WHERE id = 3`, pgerr.ForeignKeyViolation)
	qerr(t, s, `DELETE FROM country WHERE code = 'JP'`, pgerr.ForeignKeyViolation)
	expect(t, s, `SELECT count(*) FROM city`, "1") // the failed statement changed nothing
	// ON UPDATE CASCADE rewrites the referencing column.
	q(t, s, `UPDATE country SET code = 'NP' WHERE code = 'JP'`)
	expect(t, s, `SELECT country FROM city WHERE id = 3`, "NP")
	// Updating a key that is referenced without ON UPDATE fails.
	qerr(t, s, `UPDATE city SET id = 30 WHERE id = 3`, pgerr.ForeignKeyViolation)
	// Self-reference and composite keys.
	q(t, s, `CREATE TABLE emp (id int PRIMARY KEY, boss int REFERENCES emp)`)
	q(t, s, `INSERT INTO emp VALUES (1, NULL), (2, 1), (3, 2)`)
	qerr(t, s, `INSERT INTO emp VALUES (4, 99)`, pgerr.ForeignKeyViolation)
	qerr(t, s, `DELETE FROM emp WHERE id = 2`, pgerr.ForeignKeyViolation)
	q(t, s, `CREATE TABLE pair (a int, b int, PRIMARY KEY (a, b))`)
	q(t, s, `CREATE TABLE ref2 (x int, y int, FOREIGN KEY (x, y) REFERENCES pair (a, b))`)
	q(t, s, `INSERT INTO pair VALUES (1, 2)`)
	q(t, s, `INSERT INTO ref2 VALUES (1, 2), (NULL, 5)`) // a NULL column skips the check (MATCH SIMPLE)
	qerr(t, s, `INSERT INTO ref2 VALUES (2, 1)`, pgerr.ForeignKeyViolation)
	// A referenced table cannot be dropped or truncated without CASCADE.
	qerr(t, s, `DROP TABLE pair`, "2BP01")
	q(t, s, `DROP TABLE pair CASCADE`)
	q(t, s, `INSERT INTO ref2 VALUES (8, 8)`) // the constraint went with it
}
