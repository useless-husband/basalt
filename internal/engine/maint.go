package engine

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/executor"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/storage"
	"github.com/useless-husband/basalt/internal/txn"
	"github.com/useless-husband/basalt/internal/types"
)

func (s *Session) tablesFor(name *sql.TableName) ([]*catalog.Table, error) {
	cat := s.catalog()
	if name == nil {
		return cat.SortedTables(), nil
	}
	t := cat.TableByName(name.Name)
	if t == nil {
		return nil, pgerr.New(pgerr.UndefinedTable, "relation \"%s\" does not exist", name.Name)
	}
	return []*catalog.Table{t}, nil
}

func (s *Session) vacuum(ctx context.Context, x *sql.VacuumStmt) (*Result, error) {
	tables, err := s.tablesFor(x.Table)
	if err != nil {
		return nil, err
	}
	if x.Full {
		s.Notices = append(s.Notices, "VACUUM FULL is performed as a plain VACUUM")
	}
	for _, t := range tables {
		if _, err := s.db.vacuumTable(ctx, s, t); err != nil {
			return nil, err
		}
	}
	if x.Analyze {
		if _, err := s.analyzeTables(ctx, tables); err != nil {
			return nil, err
		}
	}
	return &Result{Tag: "VACUUM"}, nil
}

// vacuumTable runs VACUUM on one table.
func (db *DB) vacuumTable(ctx context.Context, env expr.Env, t *catalog.Table) (executor.VacuumStats, error) {
	db.vacMu.Lock()
	defer db.vacMu.Unlock()
	tx := db.txns.Begin()
	tx.Ctx = ctx
	defer tx.Abort()
	// Keep the table from being dropped meanwhile.
	if err := tx.Lock(txn.Tag{Kind: txn.TagRelation, ID: uint64(t.Oid)}, txn.AccessShare); err != nil {
		return executor.VacuumStats{}, err
	}
	cat := db.Catalog()
	t = cat.Tables[t.Oid]
	if t == nil {
		return executor.VacuumStats{}, nil
	}
	horizon := db.txns.OldestXmin()
	xc := executor.New(tx, db.store, cat, &expr.Ctx{Env: env, Check: checkFn(ctx)})
	st, err := xc.Vacuum(t, horizon)
	if err == nil {
		if v, ok := db.changes.Load(t.Oid); ok {
			v.(*tableChanges).deleted.Store(0)
		}
	}
	return st, err
}

func (s *Session) analyze(ctx context.Context, x *sql.AnalyzeStmt) (*Result, error) {
	tables, err := s.tablesFor(x.Table)
	if err != nil {
		return nil, err
	}
	return s.analyzeTables(ctx, tables)
}

const (
	analyzeSample = 30000
	mcvCount      = 10
	histBuckets   = 20
)

func (s *Session) analyzeTables(ctx context.Context, tables []*catalog.Table) (*Result, error) {
	stats := map[uint32]*catalog.TableStats{}
	for _, t := range tables {
		st, err := s.db.gatherStats(ctx, s.txn, t)
		if err != nil {
			return nil, err
		}
		stats[t.Oid] = st
	}
	cat, err := s.lockCatalog()
	if err != nil {
		return nil, err
	}
	for oid, st := range stats {
		if t := cat.Tables[oid]; t != nil {
			t.Stats = st
		}
	}
	// Re-decode to fill in the parsed forms of the new statistics.
	b, err := cat.Encode()
	if err != nil {
		return nil, err
	}
	if cat, err = catalog.Decode(b); err != nil {
		return nil, err
	}
	if err := s.stageCatalog(nil, cat, nil); err != nil {
		return nil, err
	}
	return &Result{Tag: "ANALYZE"}, nil
}

// gatherStats reads a table with the snapshot of transaction tx and
// computes planner statistics from a reservoir sample of its live rows.
func (db *DB) gatherStats(ctx context.Context, tx *txn.Txn, t *catalog.Table) (*catalog.TableStats, error) {
	if err := tx.Lock(txn.Tag{Kind: txn.TagRelation, ID: uint64(t.Oid)}, txn.AccessShare); err != nil {
		return nil, err
	}
	heap := db.store.Heap(t.Heap)
	cur := heap.Scan()
	r := rand.New(rand.NewPCG(uint64(t.Oid), 42))
	var sample [][]types.Value
	total := 0
	widths := make([]int64, len(t.Columns))
	check := checkFn(ctx)
	for {
		_, tuple, ok, err := cur.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if !tx.Visible(storage.DecodeHeader(tuple)) {
			continue
		}
		total++
		if total%4096 == 0 {
			if err := check(); err != nil {
				return nil, err
			}
		}
		if len(sample) < analyzeSample {
			vals, err := executor.DecodeTuple(t, tuple)
			if err != nil {
				return nil, err
			}
			sample = append(sample, vals)
		} else if j := r.IntN(total); j < analyzeSample {
			vals, err := executor.DecodeTuple(t, tuple)
			if err != nil {
				return nil, err
			}
			sample[j] = vals
		}
	}
	st := &catalog.TableStats{Rows: float64(total), Pages: heap.PageCount()}
	n := len(sample)
	for ci, col := range t.Columns {
		cs := catalog.ColumnStats{}
		var nonNull []types.Value
		var width int64
		for _, row := range sample {
			v := row[ci]
			if v.IsNull() {
				continue
			}
			nonNull = append(nonNull, v)
			switch v.K {
			case types.KText, types.KBytea:
				width += int64(len(v.S))
			default:
				width += 8
			}
		}
		_ = widths
		if n > 0 {
			cs.NullFrac = float64(n-len(nonNull)) / float64(n)
		}
		if len(nonNull) > 0 {
			cs.AvgWidth = int(width / int64(len(nonNull)))
		}
		sort.SliceStable(nonNull, func(i, j int) bool { return types.Compare(nonNull[i], nonNull[j]) < 0 })
		// Count runs of equal values.
		type run struct {
			v     types.Value
			count int
		}
		var runs []run
		for _, v := range nonNull {
			if len(runs) > 0 && types.Compare(runs[len(runs)-1].v, v) == 0 {
				runs[len(runs)-1].count++
			} else {
				runs = append(runs, run{v, 1})
			}
		}
		d := float64(len(runs))
		if n < total && len(nonNull) > 0 {
			// Haas and Stokes' estimator, as in PostgreSQL.
			f1 := 0.0
			for _, r := range runs {
				if r.count == 1 {
					f1++
				}
			}
			nn := float64(len(nonNull))
			N := float64(total) * (1 - cs.NullFrac)
			denom := nn - f1 + f1*nn/N
			if denom > 0 {
				d = nn * d / denom
			}
			d = math.Min(math.Max(d, float64(len(runs))), N)
		}
		cs.NDistinct = math.Max(d, 1)
		// Most common values: those that appear more than once and more
		// often than average.
		byCount := append([]run(nil), runs...)
		sort.SliceStable(byCount, func(i, j int) bool { return byCount[i].count > byCount[j].count })
		isMCV := map[int]bool{}
		avg := float64(len(nonNull)) / math.Max(1, float64(len(runs)))
		for i := 0; i < len(byCount) && i < mcvCount; i++ {
			rc := byCount[i]
			if rc.count < 2 || (float64(rc.count) < 1.25*avg && len(runs) > mcvCount) {
				break
			}
			cs.MCV = append(cs.MCV, types.ToText(rc.v, col.Type))
			cs.MCVFreq = append(cs.MCVFreq, float64(rc.count)/float64(n))
			isMCV[i] = true
		}
		// Equi-depth histogram over the values not in the MCV list.
		mcvSet := map[string]bool{}
		for _, m := range cs.MCV {
			mcvSet[m] = true
		}
		var rest []types.Value
		for _, v := range nonNull {
			if !mcvSet[types.ToText(v, col.Type)] {
				rest = append(rest, v)
			}
		}
		if len(rest) >= 2 && col.Type.Kind() != types.KArray && col.Type.Kind() != types.KBool {
			buckets := min(histBuckets, len(rest)-1)
			for b := 0; b <= buckets; b++ {
				idx := b * (len(rest) - 1) / buckets
				cs.Histogram = append(cs.Histogram, types.ToText(rest[idx], col.Type))
			}
		}
		st.Columns = append(st.Columns, cs)
	}
	return st, nil
}

// autovacuum vacuums tables that accumulated enough dead rows.
func (db *DB) autovacuum(every time.Duration) {
	defer db.wg.Done()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-db.stop:
			return
		case <-tick.C:
		}
		cat := db.Catalog()
		for _, t := range cat.SortedTables() {
			v, ok := db.changes.Load(t.Oid)
			if !ok {
				continue
			}
			c := v.(*tableChanges)
			dead := c.deleted.Load()
			rows := 1000.0
			if t.Stats != nil {
				rows = t.Stats.Rows
			}
			if float64(dead) < 50+0.2*rows {
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				select {
				case <-db.stop:
					cancel()
				case <-ctx.Done():
				}
			}()
			if _, err := db.vacuumTable(ctx, nil, t); err != nil && db.closed.Load() {
				cancel()
				return
			}
			cancel()
		}
	}
}

// VacuumAll vacuums every table (tests and tools).
func (db *DB) VacuumAll() (executor.VacuumStats, error) {
	var total executor.VacuumStats
	for _, t := range db.Catalog().SortedTables() {
		st, err := db.vacuumTable(context.Background(), nil, t)
		if err != nil {
			return total, err
		}
		total.Pages += st.Pages
		total.Removed += st.Removed
		total.Kept += st.Kept
	}
	return total, nil
}

var _ = fmt.Sprintf
