// Package difftest compares basalt with sqlite3 on randomly generated
// tables and queries. The generator stays inside the part of SQL where the
// two engines agree (integer arithmetic without division by zero, no
// boolean-valued output columns, case-sensitive data), and results are
// compared as multisets of rows.
//
//	BASALT_DIFFTEST=1 go test -run TestDifferential -v ./test/difftest/
//	BASALT_DIFFTEST_SEED=7 BASALT_DIFFTEST_QUERIES=5000 ...
package difftest

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/useless-husband/basalt/internal/engine"
	"github.com/useless-husband/basalt/internal/types"
	"github.com/useless-husband/basalt/internal/vfs"
)

type gen struct {
	r      *rand.Rand
	tables []table
}

type table struct {
	name string
	ints []string
	strs []string
}

func (g *gen) pick(xs []string) string { return xs[g.r.Intn(len(xs))] }

func (g *gen) schema(nrows int) []string {
	var stmts []string
	words := []string{"ant", "bee", "cat", "dog", "eel", "fox", "gnu", ""}
	for i := 0; i < 4; i++ {
		t := table{name: fmt.Sprintf("t%d", i), ints: []string{"a", "b", "c"}, strs: []string{"s"}}
		g.tables = append(g.tables, t)
		stmts = append(stmts, fmt.Sprintf("CREATE TABLE %s (id INTEGER PRIMARY KEY, a INTEGER, b INTEGER, c INTEGER NOT NULL, s TEXT)", t.name))
		if i%2 == 1 {
			stmts = append(stmts, fmt.Sprintf("CREATE INDEX %s_a ON %s (a)", t.name, t.name))
		}
		for r := 1; r <= nrows; r++ {
			v := func(null bool) string {
				if null && g.r.Intn(8) == 0 {
					return "NULL"
				}
				return strconv.Itoa(g.r.Intn(21) - 5)
			}
			s := "NULL"
			if g.r.Intn(6) != 0 {
				s = "'" + g.pick(words) + "'"
			}
			stmts = append(stmts, fmt.Sprintf("INSERT INTO %s VALUES (%d, %s, %s, %s, %s)", t.name, r, v(true), v(true), v(false), s))
		}
	}
	return stmts
}

// scope is the set of columns visible to an expression.
type scope struct {
	ints, strs []string
}

func (g *gen) intExpr(sc scope, depth int) string {
	if depth <= 0 || g.r.Intn(3) == 0 {
		switch g.r.Intn(6) {
		case 0:
			return strconv.Itoa(g.r.Intn(11) - 3)
		default:
			return g.pick(sc.ints)
		}
	}
	switch g.r.Intn(9) {
	case 0, 1:
		return "(" + g.intExpr(sc, depth-1) + " + " + g.intExpr(sc, depth-1) + ")"
	case 2:
		return "(" + g.intExpr(sc, depth-1) + " - " + g.intExpr(sc, depth-1) + ")"
	case 3:
		return "(" + g.intExpr(sc, depth-1) + " * " + g.intExpr(sc, depth-1) + ")"
	case 4:
		return "(" + g.intExpr(sc, depth-1) + " / NULLIF(" + g.intExpr(sc, depth-1) + ", 0))"
	case 5:
		return "CASE WHEN " + g.pred(sc, depth-1) + " THEN " + g.intExpr(sc, depth-1) + " ELSE " + g.intExpr(sc, depth-1) + " END"
	case 6:
		return "COALESCE(" + g.intExpr(sc, depth-1) + ", " + g.intExpr(sc, depth-1) + ")"
	case 7:
		return "ABS(" + g.intExpr(sc, depth-1) + ")"
	}
	return "(- " + g.intExpr(sc, depth-1) + ")"
}

func (g *gen) strExpr(sc scope) string {
	switch g.r.Intn(4) {
	case 0:
		return "'" + g.pick([]string{"cat", "dog", "x", ""}) + "'"
	case 1:
		return "COALESCE(" + g.pick(sc.strs) + ", 'none')"
	}
	return g.pick(sc.strs)
}

func (g *gen) pred(sc scope, depth int) string {
	if depth <= 0 {
		depth = 0
	}
	switch g.r.Intn(10) {
	case 0:
		return g.intExpr(sc, depth) + " IS NULL"
	case 1:
		return g.intExpr(sc, depth) + " IS NOT NULL"
	case 2:
		return g.intExpr(sc, depth) + " BETWEEN " + g.intExpr(sc, 0) + " AND " + g.intExpr(sc, 0)
	case 3:
		return g.intExpr(sc, depth) + " IN (" + strconv.Itoa(g.r.Intn(5)) + ", " + strconv.Itoa(g.r.Intn(5)) + ", NULL)"
	case 4:
		if depth > 0 {
			return "(" + g.pred(sc, depth-1) + " AND " + g.pred(sc, depth-1) + ")"
		}
	case 5:
		if depth > 0 {
			return "(" + g.pred(sc, depth-1) + " OR " + g.pred(sc, depth-1) + ")"
		}
	case 6:
		if depth > 0 {
			return "NOT (" + g.pred(sc, depth-1) + ")"
		}
	case 7:
		if len(sc.strs) > 0 {
			return g.strExpr(sc) + " " + g.pick([]string{"=", "<>", "<", ">="}) + " " + g.strExpr(sc)
		}
	case 8:
		if depth > 0 {
			return g.subPred(sc)
		}
	}
	return g.intExpr(sc, depth) + " " + g.pick([]string{"=", "<>", "<", "<=", ">", ">="}) + " " + g.intExpr(sc, depth)
}

// subPred is a correlated subquery predicate: EXISTS, NOT EXISTS or IN.
// As a top-level WHERE conjunct the planner turns it into a semi or anti
// join; nested under OR or NOT it runs as a subplan.
func (g *gen) subPred(sc scope) string {
	t := g.tables[g.r.Intn(len(g.tables))]
	inner := scope{ints: append(append([]string{}, sc.ints...), "z.a", "z.b", "z.c")}
	where := "z.c = " + g.pick(sc.ints)
	if g.r.Intn(4) == 0 {
		where += " AND z.a " + g.pick([]string{"<", ">=", "<>"}) + " " + g.pick(sc.ints)
	}
	where += " AND " + g.pred(inner, 0)
	switch g.r.Intn(4) {
	case 0:
		return "NOT EXISTS (SELECT 1 FROM " + t.name + " z WHERE " + where + ")"
	case 1:
		return g.pick(sc.ints) + " IN (SELECT z.b FROM " + t.name + " z WHERE " + where + ")"
	}
	return "EXISTS (SELECT 1 FROM " + t.name + " z WHERE " + where + ")"
}

func colsOf(alias string, t table) scope {
	var sc scope
	for _, c := range t.ints {
		sc.ints = append(sc.ints, alias+"."+c)
	}
	for _, c := range t.strs {
		sc.strs = append(sc.strs, alias+"."+c)
	}
	return sc
}

func (g *gen) query() string {
	t1 := g.tables[g.r.Intn(len(g.tables))]
	sc := colsOf("x", t1)
	from := t1.name + " x"
	switch g.r.Intn(4) {
	case 1:
		t2 := g.tables[g.r.Intn(len(g.tables))]
		jt := g.pick([]string{"JOIN", "LEFT JOIN"})
		s2 := colsOf("y", t2)
		on := "x." + g.pick(t1.ints) + " = y." + g.pick(t2.ints)
		if g.r.Intn(3) == 0 {
			on += " AND " + g.pred(scope{ints: append(append([]string{}, sc.ints...), s2.ints...)}, 1)
		}
		from += " " + jt + " " + t2.name + " y ON " + on
		sc.ints = append(sc.ints, s2.ints...)
		sc.strs = append(sc.strs, s2.strs...)
	case 2:
		t2 := g.tables[g.r.Intn(len(g.tables))]
		from += ", " + t2.name + " y"
		s2 := colsOf("y", t2)
		sc.ints = append(sc.ints, s2.ints...)
		sc.strs = append(sc.strs, s2.strs...)
	}
	where := ""
	if g.r.Intn(4) != 0 {
		where = " WHERE " + g.pred(sc, 2)
		if g.r.Intn(3) == 0 {
			where += " AND " + g.subPred(sc)
		}
	}
	switch g.r.Intn(6) {
	case 0, 1: // aggregate with GROUP BY
		key := g.pick(sc.ints)
		aggs := []string{"count(*)", "count(" + g.pick(sc.ints) + ")", "sum(" + g.intExpr(sc, 1) + ")",
			"min(" + g.intExpr(sc, 1) + ")", "max(" + g.pick(sc.ints) + ")", "max(" + g.pick(sc.strs) + ")"}
		q := "SELECT " + key + ", " + g.pick(aggs) + ", " + g.pick(aggs) + " FROM " + from + where + " GROUP BY " + key
		if g.r.Intn(3) == 0 {
			q += " HAVING count(*) > " + strconv.Itoa(g.r.Intn(3))
		}
		return q
	case 2: // aggregate without GROUP BY
		return "SELECT count(*), sum(" + g.intExpr(sc, 2) + "), min(" + g.pick(sc.ints) + ") FROM " + from + where
	case 3: // set operation
		other := g.tables[g.r.Intn(len(g.tables))]
		op := g.pick([]string{"UNION", "UNION ALL", "INTERSECT", "EXCEPT"})
		return "SELECT " + g.intExpr(sc, 1) + " FROM " + from + where + " " + op + " SELECT " + g.pick([]string{"a", "b", "c"}) + " FROM " + other.name
	case 4: // scalar subquery in the select list
		t2 := g.tables[g.r.Intn(len(g.tables))]
		sub := "(SELECT max(z.a) FROM " + t2.name + " z WHERE z.b = " + g.pick(sc.ints) + ")"
		return "SELECT " + g.intExpr(sc, 1) + ", " + sub + " FROM " + from + where
	}
	distinct := ""
	if g.r.Intn(4) == 0 {
		distinct = "DISTINCT "
	}
	return "SELECT " + distinct + g.intExpr(sc, 2) + ", " + g.strExpr(sc) + ", " + g.intExpr(sc, 1) + " FROM " + from + where
}

// normalize renders numbers so that 3 and 3.0 compare equal.
func normalize(s string) string {
	if s == "NULL" {
		return s
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && strings.ContainsAny(s, ".eE") {
		if f == math.Trunc(f) && math.Abs(f) < 1e15 {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', 6, 64)
	}
	return s
}

func sortedRows(rows []string) []string {
	out := append([]string(nil), rows...)
	sort.Strings(out)
	return out
}

func TestDifferential(t *testing.T) {
	if os.Getenv("BASALT_DIFFTEST") == "" {
		t.Skip("set BASALT_DIFFTEST=1 to compare with sqlite3")
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not installed")
	}
	seed := int64(1)
	if v, err := strconv.ParseInt(os.Getenv("BASALT_DIFFTEST_SEED"), 10, 64); err == nil {
		seed = v
	}
	nq := 3000
	if v, err := strconv.Atoi(os.Getenv("BASALT_DIFFTEST_QUERIES")); err == nil {
		nq = v
	}
	t.Logf("seed %d, %d queries", seed, nq)
	g := &gen{r: rand.New(rand.NewSource(seed))}
	schema := g.schema(40)
	queries := make([]string, nq)
	for i := range queries {
		queries[i] = g.query()
	}

	// sqlite3: one process, queries separated by markers.
	var script bytes.Buffer
	script.WriteString(".bail off\n.mode list\n.separator |\n.nullvalue NULL\n")
	for _, s := range schema {
		script.WriteString(s + ";\n")
	}
	for i, q := range queries {
		fmt.Fprintf(&script, "SELECT '#Q%d';\n%s;\n", i, q)
	}
	cmd := exec.Command(sqlite, ":memory:")
	cmd.Stdin = &script
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Logf("sqlite3 exited with %v", err)
	}
	sqliteRows := make([][]string, nq)
	cur := -1
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "#Q") {
			cur, _ = strconv.Atoi(line[2:])
			continue
		}
		if cur >= 0 && line != "" {
			parts := strings.Split(line, "|")
			for i := range parts {
				parts[i] = normalize(parts[i])
			}
			sqliteRows[cur] = append(sqliteRows[cur], strings.Join(parts, "|"))
		}
	}
	sqliteErr := map[int]bool{}
	// sqlite3 reports errors on stderr as "... near line N" without the
	// query number; detect them by a parse error message per query later.
	_ = stderr

	db, err := engine.Open(engine.Options{Dir: t.TempDir(), FS: vfs.NoSync{FS: vfs.OS{}}, PoolPages: 2048, CheckpointInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := db.NewSession("diff", "diff")
	for _, st := range schema {
		if _, err := s.Exec(st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
	match, mismatch, basaltErrors := 0, 0, 0
	shown := 0
	for i, q := range queries {
		res, err := s.Exec(q)
		if err != nil {
			basaltErrors++
			if shown < 10 {
				t.Logf("basalt error on query %d: %v\n  %s", i, err, q)
				shown++
			}
			continue
		}
		r := res[len(res)-1]
		var rows []string
		for _, row := range r.Rows {
			parts := make([]string, len(row))
			for j, v := range row {
				if v.IsNull() {
					parts[j] = "NULL"
				} else {
					parts[j] = normalize(types.ToText(v, r.Columns[j].Type))
				}
			}
			rows = append(rows, strings.Join(parts, "|"))
		}
		a, b := sortedRows(rows), sortedRows(sqliteRows[i])
		if strings.Join(a, "\n") == strings.Join(b, "\n") {
			match++
			continue
		}
		mismatch++
		if mismatch <= 10 {
			t.Errorf("query %d differs (seed %d):\n  %s\n  basalt (%d rows): %v\n  sqlite (%d rows): %v", i, seed, q, len(a), head(a), len(b), head(b))
		}
	}
	_ = sqliteErr
	t.Logf("RESULT %d queries: %d identical, %d different, %d rejected by basalt", nq, match, mismatch, basaltErrors)
	if basaltErrors > 0 {
		t.Errorf("basalt rejected %d generated queries", basaltErrors)
	}
}

func head(rows []string) []string {
	if len(rows) > 6 {
		return append(rows[:6:6], "...")
	}
	return rows
}
