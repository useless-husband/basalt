package planner

import (
	"strings"
	"testing"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/types"
)

func testCatalog() *catalog.Catalog {
	c := catalog.New()
	c.Tables[1] = &catalog.Table{Oid: 1, Name: "t", Columns: []*catalog.Column{{Name: "a", Type: types.Int4}, {Name: "b", Type: types.Text}, {Name: "c", Type: types.Numeric}}, Indexes: []uint32{10}}
	c.Indexes[10] = &catalog.Index{Oid: 10, Name: "t_pkey", Table: 1, Columns: []int{0}, Unique: true, Primary: true}
	c.Tables[2] = &catalog.Table{Oid: 2, Name: "u", Columns: []*catalog.Column{{Name: "a", Type: types.Int4}, {Name: "d", Type: types.Date}}}
	return c
}

func bind(t *testing.T, q string, params ...types.T) (*Query, *Binder, error) {
	t.Helper()
	st, err := sql.ParseOne(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	b := NewBinder(testCatalog(), nil, params)
	qu, err := b.BindQuery(st.(*sql.SelectStmt))
	return qu, b, err
}

func TestBinderErrors(t *testing.T) {
	cases := map[string]string{
		`SELECT nope FROM t`:                       pgerr.UndefinedColumn,
		`SELECT a FROM t, u`:                       pgerr.AmbiguousColumn,
		`SELECT b, count(*) FROM t GROUP BY a`:     pgerr.GroupingError,
		`SELECT a FROM t WHERE count(*) > 1`:       pgerr.GroupingError,
		`SELECT sum(count(*)) FROM t`:              pgerr.GroupingError,
		`SELECT a + b FROM t`:                      pgerr.UndefinedFunction,
		`SELECT * FROM missing`:                    pgerr.UndefinedTable,
		`SELECT x.a FROM t`:                        pgerr.UndefinedTable,
		`SELECT (SELECT a, b FROM t)`:              pgerr.SyntaxError,
		`SELECT a FROM t UNION SELECT a, b FROM t`: pgerr.SyntaxError,
		`SELECT d + 'x' FROM u`:                    pgerr.InvalidTextRepresentation,
	}
	for q, code := range cases {
		_, _, err := bind(t, q)
		if pgerr.Code(err) != code {
			t.Errorf("%s: got %v (%s), want %s", q, err, pgerr.Code(err), code)
		}
	}
}

func TestParameterTypeInference(t *testing.T) {
	_, b, err := bind(t, `SELECT a FROM t WHERE a = $1 AND b LIKE $2 AND c > $3 LIMIT $4`)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{types.OidInt4, types.OidText, types.OidNumeric, types.OidInt8}
	for i, w := range want {
		if b.Params[i].Oid != w {
			t.Errorf("$%d inferred as %s", i+1, b.Params[i])
		}
	}
	// A client-specified type wins and an unconstrained parameter is text.
	_, b, _ = bind(t, `SELECT $1, a FROM t WHERE a = $2`, types.Unknown, types.Int8)
	if b.Params[0].Oid != types.OidText || b.Params[1].Oid != types.OidInt8 {
		t.Errorf("got %v", b.Params)
	}
}

func optimize(t *testing.T, q string) (Plan, string) {
	t.Helper()
	qu, b, err := bind(t, q)
	if err != nil {
		t.Fatal(err)
	}
	o := NewOptimizer(b.Cat, b, &Rewriter{Ctx: &expr.Ctx{}}, func(*catalog.Table) int { return 100 }, nil)
	p, err := o.Optimize(qu.Root)
	if err != nil {
		t.Fatal(err)
	}
	return p, strings.Join(Explain(p, ExplainOptions{}), "\n")
}

func TestConstantFoldingAndPushdown(t *testing.T) {
	_, plan := optimize(t, `SELECT a FROM t WHERE a = 1 + 2 AND (b = 'x' OR false) AND true`)
	if !strings.Contains(plan, "(t.a = 3)") || strings.Contains(plan, "false") || strings.Contains(plan, "true") {
		t.Errorf("not folded:\n%s", plan)
	}
	// Single-table predicates move below the join into the scans.
	_, plan = optimize(t, `SELECT * FROM t JOIN u ON t.a = u.a WHERE t.b = 'x' AND u.d > '2024-01-01'`)
	lines := strings.Split(plan, "\n")
	for i, l := range lines {
		if strings.Contains(l, "Filter: (t.b = ") || strings.Contains(l, "Filter: (u.d > ") {
			if !strings.Contains(lines[i-1], "Scan on") {
				t.Errorf("filter not attached to a scan:\n%s", plan)
			}
		}
	}
	// A WHERE condition on the nullable side turns LEFT JOIN into a join.
	_, plan = optimize(t, `SELECT * FROM t LEFT JOIN u ON t.a = u.a WHERE u.d = '2024-01-01'`)
	if strings.Contains(plan, "Left") {
		t.Errorf("LEFT JOIN should become an inner join:\n%s", plan)
	}
	// ... but IS NULL on it does not (the anti-join idiom).
	_, plan = optimize(t, `SELECT * FROM t LEFT JOIN u ON t.a = u.a WHERE u.d IS NULL`)
	if !strings.Contains(plan, "Left") {
		t.Errorf("IS NULL must keep the outer join:\n%s", plan)
	}
}

func TestIndexChoiceWithoutStatistics(t *testing.T) {
	_, plan := optimize(t, `SELECT * FROM t WHERE a = 42`)
	if !strings.Contains(plan, "Index Scan using t_pkey") {
		t.Errorf("equality on the primary key should use it:\n%s", plan)
	}
	_, plan = optimize(t, `SELECT * FROM t WHERE b = 'x'`)
	if !strings.Contains(plan, "Seq Scan") {
		t.Errorf("no index on b:\n%s", plan)
	}
}
