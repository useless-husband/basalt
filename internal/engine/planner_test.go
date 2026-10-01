package engine

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// estimate returns the row estimate of the top plan node.
func estimate(t *testing.T, s *Session, query string) (int, string) {
	t.Helper()
	plan := q(t, s, "EXPLAIN "+query)
	m := regexp.MustCompile(`rows=(\d+)`).FindStringSubmatch(plan)
	if m == nil {
		t.Fatalf("no estimate in %s", plan)
	}
	n, _ := strconv.Atoi(m[1])
	return n, plan
}

// TestPlannerUsesStatistics checks that ANALYZE statistics drive the
// estimates (histograms for ranges, MCVs for skewed equality, distinct
// counts for joins) and, through them, the choice of access path and join.
func TestPlannerUsesStatistics(t *testing.T) {
	db, _ := openMem(t)
	defer db.Close()
	s := db.NewSession("t", "t")
	q(t, s, `CREATE TABLE big (id int PRIMARY KEY, grp int, tag text)`)
	q(t, s, `CREATE TABLE small (gid int PRIMARY KEY, label text)`)
	q(t, s, `INSERT INTO big SELECT g, g % 50, CASE WHEN g % 10 = 0 THEN 'rare' ELSE 'common' END FROM generate_series(1, 20000) g`)
	q(t, s, `INSERT INTO small SELECT g, 'g' || g FROM generate_series(0, 49) g`)
	q(t, s, `ANALYZE`)
	within := func(got, want int) bool {
		return float64(got) >= 0.7*float64(want) && float64(got) <= 1.3*float64(want)
	}
	for _, c := range []struct {
		query string
		want  int
	}{
		{`SELECT * FROM big WHERE id < 2000`, 2000},
		{`SELECT * FROM big WHERE id >= 15000`, 5000},
		{`SELECT * FROM big WHERE tag = 'rare'`, 2000},
		{`SELECT * FROM big WHERE tag = 'common'`, 18000},
		{`SELECT * FROM big b JOIN small s ON s.gid = b.grp`, 20000},
	} {
		got, plan := estimate(t, s, c.query)
		if !within(got, c.want) {
			t.Errorf("%s: estimated %d rows, actual %d\n%s", c.query, got, c.want, plan)
		}
	}
	// A selective range uses the primary key index; an unselective one
	// scans the table.
	if _, plan := estimate(t, s, `SELECT * FROM big WHERE id BETWEEN 100 AND 120`); !strings.Contains(plan, "Index Scan using big_pkey") {
		t.Errorf("selective range should use the index:\n%s", plan)
	}
	if _, plan := estimate(t, s, `SELECT * FROM big WHERE id > 10`); !strings.Contains(plan, "Seq Scan on big") {
		t.Errorf("unselective range should scan:\n%s", plan)
	}
	// The join hashes the small side.
	if _, plan := estimate(t, s, `SELECT * FROM big b JOIN small s ON s.gid = b.grp`); !strings.Contains(plan, "Hash Join") ||
		!regexp.MustCompile(`(?s)Hash Join.*Seq Scan on big.*Seq Scan on small`).MatchString(plan) {
		t.Errorf("expected a hash join building on small:\n%s", plan)
	}
	// ORDER BY ... LIMIT on the key reads the index in order.
	if _, plan := estimate(t, s, `SELECT * FROM big ORDER BY id LIMIT 5`); !strings.Contains(plan, "Index Scan using big_pkey") || strings.Contains(plan, "Sort") {
		t.Errorf("ORDER BY id LIMIT should use the index order:\n%s", plan)
	}
	// Planner switches work like PostgreSQL's enable_* settings.
	q(t, s, `SET enable_hashjoin = off`)
	q(t, s, `SET enable_nestloop = off`)
	if _, plan := estimate(t, s, `SELECT * FROM big b JOIN small s ON s.gid = b.grp`); !strings.Contains(plan, "Merge Join") {
		t.Errorf("with hash and nested loop joins disabled, expected a merge join:\n%s", plan)
	}
	expect(t, s, `SELECT count(*), sum(b.id) FROM big b JOIN small s ON s.gid = b.grp`, "20000|200010000")
}
