package sql

import (
	"strings"
	"testing"

	"github.com/useless-husband/basalt/internal/pgerr"
)

func TestParseAccepts(t *testing.T) {
	queries := []string{
		`SELECT 1`,
		`select a, b as "B", c d from t where a = 1 and not b or c is null`,
		`SELECT DISTINCT ON (a) a, b FROM t ORDER BY a, b DESC NULLS LAST LIMIT 10 OFFSET 5`,
		`SELECT * FROM a JOIN b ON a.id = b.aid LEFT OUTER JOIN c USING (x) CROSS JOIN d`,
		`SELECT count(*), sum(DISTINCT x) FILTER (WHERE x > 0) FROM t GROUP BY y HAVING count(*) > 1`,
		`WITH w AS (SELECT 1 AS x), v(y) AS (VALUES (2)) SELECT * FROM w, v`,
		`SELECT 1 UNION ALL SELECT 2 INTERSECT SELECT 3 EXCEPT SELECT 4 ORDER BY 1`,
		`(SELECT 1 LIMIT 1) UNION (SELECT 2)`,
		`SELECT CASE WHEN a THEN 1 WHEN b THEN 2 ELSE 3 END, CASE x WHEN 1 THEN 'a' END FROM t`,
		`SELECT x::int, CAST(y AS varchar(10)), '1 day'::interval, DATE '2020-01-01', int4 '5'`,
		`SELECT a FROM t WHERE b IN (1,2,3) AND c NOT IN (SELECT d FROM u) AND EXISTS (SELECT 1)`,
		`SELECT a BETWEEN 1 AND 10, a NOT BETWEEN SYMMETRIC 10 AND 1, s LIKE 'a%' ESCAPE '\', s NOT ILIKE 'b'`,
		`SELECT x = ANY(ARRAY[1,2]), y > ALL (SELECT z FROM w), arr[1]`,
		`SELECT (SELECT max(x) FROM t2 WHERE t2.a = t1.a) FROM t1`,
		`SELECT extract(year FROM ts), position('a' IN s), substring(s FROM 2 FOR 3), trim(both 'x' from s)`,
		`SELECT -2147483648, 1.5e10, .5, $1, E'a\nb', $$dollar$$`,
		`SELECT c.relname FROM pg_catalog.pg_class c WHERE c.relname OPERATOR(pg_catalog.~) '^(t)$' COLLATE pg_catalog.default`,
		`INSERT INTO t (a, b) VALUES (1, DEFAULT), (2, 'x') RETURNING *`,
		`INSERT INTO t SELECT * FROM u ON CONFLICT (a) DO UPDATE SET b = excluded.b`,
		`INSERT INTO t DEFAULT VALUES`,
		`UPDATE t AS x SET a = a + 1, (b, c) = (1, 2) FROM u WHERE x.id = u.id RETURNING a`,
		`DELETE FROM t USING u WHERE t.id = u.id RETURNING t.*`,
		`CREATE TABLE IF NOT EXISTS t (id serial PRIMARY KEY, name varchar(20) NOT NULL DEFAULT 'x' UNIQUE,
		   price numeric(10,2) CHECK (price > 0), owner int REFERENCES u(id) ON DELETE CASCADE,
		   ts timestamp with time zone, d double precision, CONSTRAINT c1 UNIQUE (name, price),
		   FOREIGN KEY (owner) REFERENCES u (id))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS i ON t (a, b DESC)`,
		`DROP TABLE IF EXISTS a, b CASCADE`,
		`BEGIN ISOLATION LEVEL REPEATABLE READ`,
		`START TRANSACTION READ ONLY`,
		`COMMIT`, `ROLLBACK`, `ABORT`, `END`,
		`EXPLAIN ANALYZE SELECT 1`, `EXPLAIN (ANALYZE, COSTS OFF) SELECT 1`,
		`ANALYZE`, `ANALYZE t`, `VACUUM`, `VACUUM FULL t`,
		`SET search_path TO public, pg_catalog`, `SET TIME ZONE 'UTC'`, `SET extra_float_digits = 3`,
		`SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED`,
		`SHOW server_version`, `RESET ALL`,
		`SELECT * FROM generate_series(1, 10) AS g(i)`,
		`SELECT * FROM (SELECT 1) AS s(x) WHERE x IS NOT DISTINCT FROM 1`,
		`ALTER TABLE t ADD COLUMN c int DEFAULT 0`,
		`COPY t (a, b) FROM STDIN WITH (FORMAT csv, HEADER true)`,
		`SELECT 1; SELECT 2;`,
		`select left('abc', 2), right('abc', 1)`,
		`SELECT a FROM t FOR UPDATE`,
		`SELECT now() AT TIME ZONE 'UTC'`,
		`SELECT 'a' || 'b' || 'c', 2 ^ 3 ^ 2, -x, +y, NOT NOT z`,
		`SELECT FROM t`,
		`SELECT t.* FROM t`,
		`TABLE t`,
	}
	for _, q := range queries {
		if _, err := Parse(q); err != nil {
			t.Errorf("Parse(%q): %v", q, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		`SELECT FROM WHERE`,
		`SELECT 1 +`,
		`SELECT 'unterminated`,
		`SELECT * FROM t WHERE (a = 1`,
		`INSERT t VALUES (1)`,
		`CREATE TABLE t (a int,)`,
		`SELECT select`,
	}
	for _, q := range bad {
		_, err := Parse(q)
		if err == nil {
			t.Errorf("Parse(%q): expected error", q)
			continue
		}
		if pgerr.Code(err) != pgerr.SyntaxError && pgerr.Code(err) != pgerr.FeatureNotSupported {
			t.Errorf("Parse(%q): code %s", q, pgerr.Code(err))
		}
	}
}

func TestPrecedence(t *testing.T) {
	e, err := ParseExpr(`a OR b AND NOT c = d + e * f`)
	if err != nil {
		t.Fatal(err)
	}
	or := e.(*BinaryExpr)
	if or.Op != "OR" {
		t.Fatalf("top should be OR, got %s", or.Op)
	}
	and := or.Right.(*BinaryExpr)
	if and.Op != "AND" {
		t.Fatalf("expected AND")
	}
	not := and.Right.(*UnaryExpr)
	eq := not.Expr.(*BinaryExpr)
	if eq.Op != "=" {
		t.Fatalf("NOT should apply to the comparison")
	}
	plus := eq.Right.(*BinaryExpr)
	if plus.Op != "+" || plus.Right.(*BinaryExpr).Op != "*" {
		t.Fatalf("arithmetic precedence wrong")
	}
	// IS binds looser than comparison.
	e, _ = ParseExpr(`a = b IS NULL`)
	if _, ok := e.(*IsExpr); !ok {
		t.Fatalf("a = b IS NULL should be (a = b) IS NULL, got %T", e)
	}
	// Casts bind tightest.
	e, _ = ParseExpr(`-a::int`)
	if u, ok := e.(*UnaryExpr); !ok || u.Op != "-" {
		t.Fatalf("-a::int should be -(a::int), got %T", e)
	}
}

func TestNegativeLiteralFolding(t *testing.T) {
	e, err := ParseExpr(`-9223372036854775808`)
	if err != nil {
		t.Fatal(err)
	}
	lit := e.(*Literal)
	if lit.Kind != LitInt || lit.Val != "-9223372036854775808" {
		t.Fatalf("got %+v", lit)
	}
}

func TestStatementSplitting(t *testing.T) {
	stmts, err := Parse(`select 1; ; insert into t values (';'); `)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 2 || !strings.Contains(stmts[1].SQL, "';'") {
		t.Fatalf("got %+v", stmts)
	}
}

func TestCheckSQLCaptured(t *testing.T) {
	st, err := ParseOne(`CREATE TABLE t (a int CHECK (a > 0 AND a < 10))`)
	if err != nil {
		t.Fatal(err)
	}
	ct := st.(*CreateTableStmt)
	c := ct.Columns[0].Constraints[0]
	if c.CheckSQL != "a > 0 AND a < 10" {
		t.Fatalf("CheckSQL = %q", c.CheckSQL)
	}
}

func TestSyntaxErrorPosition(t *testing.T) {
	_, err := Parse(`SELECT * FROM t WHERE`)
	pe := err.(*pgerr.Error)
	if pe.Message != "syntax error at end of input" || pe.Position == 0 {
		t.Fatalf("got %+v", pe)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{`SELECT 1`, `SELECT a FROM t WHERE b = 'x' ORDER BY 1`, `INSERT INTO t VALUES (1, $1)`, `CREATE TABLE t (a int primary key)`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		// The parser must never panic, whatever the input.
		_, _ = Parse(s)
	})
}
