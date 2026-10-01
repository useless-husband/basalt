package executor

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/planner"
	"github.com/useless-husband/basalt/internal/storage"
	"github.com/useless-husband/basalt/internal/types"
)

// values builds a ValuesP producing rows of (key, payload) with the given
// column ids. A key of -1 means NULL.
func valuesPlan(ids []expr.ColumnID, rows [][2]int) *planner.ValuesP {
	v := &planner.ValuesP{ColIDs: ids}
	for _, r := range rows {
		k := types.Value(types.NewInt(int64(r[0])))
		if r[0] < 0 {
			k = types.Null
		}
		v.Rows = append(v.Rows, []expr.Expr{&expr.Const{V: k, T: types.Int4}, &expr.Const{V: types.NewInt(int64(r[1])), T: types.Int4}})
	}
	return v
}

func render(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		parts := make([]string, len(r))
		for j, v := range r {
			if v.IsNull() {
				parts[j] = "N"
			} else {
				parts[j] = fmt.Sprint(v.I)
			}
		}
		out[i] = strings.Join(parts, ",")
	}
	sort.Strings(out)
	return out
}

func randomRows(r *rand.Rand, n int) [][2]int {
	rows := make([][2]int, n)
	for i := range rows {
		k := r.Intn(6)
		if r.Intn(7) == 0 {
			k = -1
		}
		rows[i] = [2]int{k, i}
	}
	return rows
}

// TestJoinAlgorithmsAgree checks that hash join, merge join and nested
// loop produce the same multiset of rows for every join kind they
// support, on random inputs with duplicate and NULL keys.
func TestJoinAlgorithmsAgree(t *testing.T) {
	seed := int64(20261002)
	r := rand.New(rand.NewSource(seed))
	lk, lv, rk, rv := expr.ColumnID(1), expr.ColumnID(2), expr.ColumnID(3), expr.ColumnID(4)
	lkCol := &expr.Col{ID: lk, T: types.Int4, Name: "l.k"}
	rkCol := &expr.Col{ID: rk, T: types.Int4, Name: "r.k"}
	eq := &expr.Call{Fn: expr.CompareFunc("=", types.Int4), Args: []expr.Expr{lkCol, rkCol}, T: types.Bool}
	sortKeys := func(c *expr.Col) []expr.SortKey { return []expr.SortKey{{E: c}} }
	c := &Ctx{Eval: &expr.Ctx{}, subIters: map[int]Iter{}}
	c.Eval.Sub = c
	for round := 0; round < 200; round++ {
		left, right := randomRows(r, r.Intn(12)), randomRows(r, r.Intn(12))
		for _, kind := range []planner.JoinKind{planner.JoinInner, planner.JoinLeft, planner.JoinFull, planner.JoinSemi, planner.JoinAnti} {
			mkL := func() planner.Plan { return valuesPlan([]expr.ColumnID{lk, lv}, left) }
			mkR := func() planner.Plan { return valuesPlan([]expr.ColumnID{rk, rv}, right) }
			plans := map[string]planner.Plan{
				"nestloop": &planner.NestLoopP{Kind: kind, Outer: mkL(), Inner: mkR(), Cond: eq},
				"hash":     &planner.HashJoinP{Kind: kind, Outer: mkL(), Inner: mkR(), OuterKeys: []expr.Expr{lkCol}, InnerKeys: []expr.Expr{rkCol}},
			}
			if kind == planner.JoinInner || kind == planner.JoinLeft {
				plans["merge"] = &planner.MergeJoinP{Kind: kind,
					Outer: &planner.SortP{Input: mkL(), Keys: sortKeys(lkCol)}, Inner: &planner.SortP{Input: mkR(), Keys: sortKeys(rkCol)},
					OuterKeys: []expr.Expr{lkCol}, InnerKeys: []expr.Expr{rkCol}}
			}
			var want []string
			for _, name := range []string{"nestloop", "hash", "merge"} {
				p, ok := plans[name]
				if !ok {
					continue
				}
				it, err := c.Build(p)
				if err != nil {
					t.Fatal(err)
				}
				rows, err := Drain(it, c.Eval)
				if err != nil {
					t.Fatal(err)
				}
				got := render(rows)
				if want == nil {
					want = got
					continue
				}
				if strings.Join(got, " ") != strings.Join(want, " ") {
					t.Fatalf("seed %d round %d %v join: %s gives %v, nested loop gives %v\nleft %v\nright %v",
						seed, round, kind, name, got, want, left, right)
				}
			}
		}
	}
}

func TestTIDKeyRoundTrip(t *testing.T) {
	for _, tid := range []uint64{0, 1, 1<<16 | 7, 1<<47 | 65535} {
		k := appendTID([]byte("prefix"), storageTID(tid))
		if got := tidFromKey(k); uint64(got) != tid {
			t.Fatalf("tid %d -> %d", tid, got)
		}
	}
}

func TestSortKeyComparison(t *testing.T) {
	keys := []sortKey{{desc: true}, {nullsFirst: true}}
	rows := [][]types.Value{
		{types.NewInt(1), types.NewInt(5)},
		{types.NewInt(2), types.Null},
		{types.NewInt(2), types.NewInt(1)},
		{types.Null, types.NewInt(0)},
	}
	sort.SliceStable(rows, func(i, j int) bool { return compareKeyVals(keys, rows[i], rows[j]) < 0 })
	// DESC puts NULL last (keys[0].nullsFirst is false); within 2, NULLS FIRST.
	got := ""
	for _, r := range rows {
		for _, v := range r {
			if v.IsNull() {
				got += "N"
			} else {
				got += fmt.Sprint(v.I)
			}
		}
		got += " "
	}
	if got != "2N 21 15 N0 " {
		t.Fatalf("order %q", got)
	}
}

func storageTID(v uint64) storage.TID { return storage.TID(v) }
