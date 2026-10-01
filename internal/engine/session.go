package engine

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/executor"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/planner"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/txn"
	"github.com/useless-husband/basalt/internal/types"
)

// Column describes a result column.
type Column struct {
	Name     string
	Type     types.T
	TableOid uint32
	Attnum   int16
}

// Result is the outcome of one statement.
type Result struct {
	Columns []Column
	Rows    [][]types.Value
	Tag     string
	// HasRows is set for statements that return a row set (even empty).
	HasRows bool
	Empty   bool // empty query string
}

// TxnStatus is the transaction state reported in ReadyForQuery.
type TxnStatus byte

const (
	TxnIdle   TxnStatus = 'I'
	TxnActive TxnStatus = 'T'
	TxnFailed TxnStatus = 'E'
)

// Session is a client session.
type Session struct {
	db       *DB
	pid      int64
	secret   int32
	user     string
	database string

	txn       *txn.Txn
	explicit  bool
	failed    bool
	txnStart  int64
	stmtStart int64
	readOnly  bool

	settings map[string]string
	prepared map[string]*Prepared
	currval  map[string]int64

	cancelMu   sync.Mutex
	curCancel  context.CancelCauseFunc
	stmtCancel func()
	// Notices collected during the last statement.
	Notices []string
}

var defaultSettings = map[string]string{
	"server_version":                      ServerVersion,
	"server_version_num":                  ServerVersionNum,
	"server_encoding":                     "UTF8",
	"client_encoding":                     "UTF8",
	"datestyle":                           "ISO, MDY",
	"timezone":                            "UTC",
	"intervalstyle":                       "postgres",
	"integer_datetimes":                   "on",
	"standard_conforming_strings":         "on",
	"search_path":                         `"$user", public`,
	"default_transaction_isolation":       "repeatable read",
	"transaction_isolation":               "repeatable read",
	"transaction_read_only":               "off",
	"default_transaction_read_only":       "off",
	"max_identifier_length":               "63",
	"lc_collate":                          "C",
	"lc_ctype":                            "C",
	"lc_messages":                         "C",
	"extra_float_digits":                  "1",
	"application_name":                    "",
	"is_superuser":                        "on",
	"statement_timeout":                   "0",
	"lock_timeout":                        "0",
	"idle_in_transaction_session_timeout": "0",
	"enable_seqscan":                      "on",
	"enable_indexscan":                    "on",
	"enable_hashjoin":                     "on",
	"enable_mergejoin":                    "on",
	"enable_nestloop":                     "on",
	"client_min_messages":                 "notice",
	"bytea_output":                        "hex",
	"block_size":                          "8192",
	"max_connections":                     "100",
	"default_text_search_config":          "pg_catalog.simple",
	"row_security":                        "on",
	"synchronous_commit":                  "on",
	"jit":                                 "off",
	"basalt_version":                      "basalt " + Version,
}

// ReportedParameters are sent to clients at startup (ParameterStatus).
var ReportedParameters = []string{"server_version", "server_encoding", "client_encoding", "application_name",
	"DateStyle", "TimeZone", "IntervalStyle", "integer_datetimes", "standard_conforming_strings", "is_superuser",
	"session_authorization"}

// NewSession starts a session.
func (db *DB) NewSession(user, database string) *Session {
	s := &Session{db: db, user: user, database: database, settings: map[string]string{},
		prepared: map[string]*Prepared{}, currval: map[string]int64{}}
	s.pid = db.pids.Add(1)
	s.secret = rand.Int32()
	for k, v := range defaultSettings {
		s.settings[k] = v
	}
	s.settings["session_authorization"] = user
	db.register(s)
	return s
}

// Close ends the session, rolling back any open transaction.
func (s *Session) Close() {
	if s.txn != nil {
		s.txn.Abort()
		s.txn = nil
	}
	s.db.unregister(s)
}

// BackendKey returns the cancellation key.
func (s *Session) BackendKey() (int64, int32) { return s.pid, s.secret }

// Setting returns a setting (case-insensitive).
func (s *Session) Setting(name string) (string, bool) {
	v, ok := s.settings[strings.ToLower(name)]
	return v, ok
}

// SetStartupParameter applies a parameter from the startup message.
func (s *Session) SetStartupParameter(k, v string) {
	switch strings.ToLower(k) {
	case "user", "database", "replication":
		return
	case "options":
		// -c name=value pairs.
		for _, f := range strings.Fields(v) {
			f = strings.TrimPrefix(f, "-c")
			if kv := strings.SplitN(f, "=", 2); len(kv) == 2 {
				s.settings[strings.ToLower(kv[0])] = kv[1]
			}
		}
		return
	}
	s.settings[strings.ToLower(k)] = v
}

// Status reports the transaction status.
func (s *Session) Status() TxnStatus {
	switch {
	case s.failed:
		return TxnFailed
	case s.explicit:
		return TxnActive
	}
	return TxnIdle
}

func nowMicros() int64 { return types.TimestampFromTime(time.Now()) }

func (s *Session) beginTxn() {
	if s.txn == nil {
		s.txn = s.db.txns.Begin()
		s.txn.ReadOnly = s.readOnly
		s.txnStart = nowMicros()
	}
}

func (s *Session) endTxn(commit bool) error {
	t := s.txn
	s.txn = nil
	s.readOnly = false
	if t == nil {
		return nil
	}
	if commit {
		return t.Commit()
	}
	t.Abort()
	return nil
}

// statement context and cancellation

func (s *Session) startStatement() context.Context {
	s.stmtStart = nowMicros()
	s.Notices = nil
	ctx, cancel := context.WithCancelCause(context.Background())
	stop := func() { cancel(nil) }
	if ms, _ := strconv.Atoi(s.settings["statement_timeout"]); ms > 0 {
		var c2 context.CancelFunc
		ctx, c2 = context.WithTimeoutCause(ctx, time.Duration(ms)*time.Millisecond,
			pgerr.New(pgerr.QueryCanceled, "canceling statement due to statement timeout"))
		stop = func() { c2(); cancel(nil) }
	}
	s.cancelMu.Lock()
	s.curCancel = cancel
	s.stmtCancel = stop
	s.cancelMu.Unlock()
	return ctx
}

func (s *Session) endStatement() {
	s.cancelMu.Lock()
	stop := s.stmtCancel
	s.stmtCancel, s.curCancel = nil, nil
	s.cancelMu.Unlock()
	if stop != nil {
		stop()
	}
}

// cancel interrupts the running statement (CancelRequest).
func (s *Session) cancel() {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if s.curCancel != nil {
		s.curCancel(pgerr.New(pgerr.QueryCanceled, "canceling statement due to user request"))
	}
}

func checkFn(ctx context.Context) func() error {
	return func() error {
		select {
		case <-ctx.Done():
			if e, ok := context.Cause(ctx).(*pgerr.Error); ok {
				return e
			}
			return pgerr.New(pgerr.QueryCanceled, "canceling statement due to user request")
		default:
			return nil
		}
	}
}

// ---- simple query ----

// Exec runs a query string that may contain several statements, with the
// semantics of the simple query protocol: the statements run in one
// implicit transaction unless the string contains transaction control.
// Results of completed statements are returned along with the error of
// the failing one.
func (s *Session) Exec(query string) ([]*Result, error) {
	stmts, err := sql.Parse(query)
	if err != nil {
		s.failTxn()
		return nil, err
	}
	if len(stmts) == 0 {
		return []*Result{{Empty: true}}, nil
	}
	var results []*Result
	for _, raw := range stmts {
		res, err := s.run(raw.Stmt, nil, nil, len(stmts) > 1)
		if err != nil {
			s.failTxn()
			return results, err
		}
		results = append(results, res)
	}
	if !s.explicit && s.txn != nil {
		if err := s.endTxn(true); err != nil {
			return results, err
		}
	}
	return results, nil
}

// failTxn handles an error: an implicit transaction is rolled back; an
// explicit one enters the failed state until ROLLBACK.
func (s *Session) failTxn() {
	if s.explicit {
		s.failed = true
		if s.txn != nil {
			s.txn.Abort() // release locks now; the block stays failed
		}
		return
	}
	_ = s.endTxn(false)
}

// Sync ends an implicit transaction of the extended protocol.
func (s *Session) Sync() error {
	if !s.explicit && s.txn != nil {
		return s.endTxn(true)
	}
	return nil
}

func isDDL(st sql.Stmt) bool {
	switch st.(type) {
	case *sql.CreateTableStmt, *sql.CreateIndexStmt, *sql.DropStmt, *sql.AlterTableStmt, *sql.TruncateStmt, *sql.CreateSequenceStmt:
		return true
	}
	return false
}

func stmtName(st sql.Stmt) string {
	switch x := st.(type) {
	case *sql.CreateTableStmt:
		return "CREATE TABLE"
	case *sql.CreateIndexStmt:
		return "CREATE INDEX"
	case *sql.DropStmt:
		return [...]string{"DROP TABLE", "DROP INDEX", "DROP SEQUENCE", "DROP VIEW"}[x.Kind]
	case *sql.AlterTableStmt:
		return "ALTER TABLE"
	case *sql.TruncateStmt:
		return "TRUNCATE TABLE"
	case *sql.CreateSequenceStmt:
		return "CREATE SEQUENCE"
	case *sql.VacuumStmt:
		return "VACUUM"
	}
	return "this statement"
}

// run executes one statement. params are bound parameter values; prep is
// the prepared statement when run through the extended protocol.
func (s *Session) run(st sql.Stmt, params []types.Value, prep *Prepared, inBatch bool) (*Result, error) {
	ctx := s.startStatement()
	defer s.endStatement()
	if t, ok := st.(*sql.TransactionStmt); ok {
		return s.txnControl(t)
	}
	if s.failed {
		return nil, pgerr.New(pgerr.InFailedSQLTransaction, "current transaction is aborted, commands ignored until end of transaction block")
	}
	switch x := st.(type) {
	case *sql.SetStmt:
		return s.set(x)
	case *sql.ShowStmt:
		return s.show(x)
	case *sql.DeallocateStmt:
		if x.All {
			s.prepared = map[string]*Prepared{}
		} else {
			delete(s.prepared, x.Name)
		}
		return &Result{Tag: "DEALLOCATE"}, nil
	case *sql.DiscardStmt:
		s.prepared = map[string]*Prepared{}
		return &Result{Tag: "DISCARD ALL"}, nil
	case *sql.NoopStmt:
		return &Result{Tag: x.Tag}, nil
	case *sql.CheckpointStmt:
		return &Result{Tag: "CHECKPOINT"}, s.db.Checkpoint()
	case *sql.VacuumStmt:
		if s.explicit {
			return nil, pgerr.New(pgerr.ActiveSQLTransaction, "VACUUM cannot run inside a transaction block")
		}
		if s.txn != nil && inBatch {
			if err := s.endTxn(true); err != nil {
				return nil, err
			}
		}
		return s.vacuum(ctx, x)
	}
	if isDDL(st) {
		if s.explicit {
			return nil, pgerr.New(pgerr.ActiveSQLTransaction, "%s cannot run inside a transaction block", stmtName(st)).
				WithHint("basalt does not support transactional DDL; run DDL outside BEGIN ... COMMIT.")
		}
		if s.txn != nil {
			// Inside a multi-statement batch: commit what came before.
			if err := s.endTxn(true); err != nil {
				return nil, err
			}
		}
		return s.ddl(ctx, st)
	}
	s.beginTxn()
	s.txn.Ctx = ctx
	switch x := st.(type) {
	case *sql.AnalyzeStmt:
		return s.analyze(ctx, x)
	case *sql.CopyStmt:
		return nil, pgerr.Unsupported("COPY is only supported through the wire protocol")
	}
	return s.execPlanned(ctx, st, params, prep)
}

func (s *Session) txnControl(t *sql.TransactionStmt) (*Result, error) {
	switch t.Kind {
	case sql.TxnBegin:
		if t.Isolation == "serializable" {
			return nil, pgerr.Unsupported("SERIALIZABLE isolation is not supported; basalt provides snapshot isolation (REPEATABLE READ)")
		}
		if s.explicit {
			s.Notices = append(s.Notices, "there is already a transaction in progress")
			return &Result{Tag: "BEGIN"}, nil
		}
		if s.txn != nil {
			// BEGIN inside a batch commits the implicit transaction first.
			if err := s.endTxn(true); err != nil {
				return nil, err
			}
		}
		s.readOnly = t.ReadOnly
		s.beginTxn()
		s.explicit = true
		return &Result{Tag: "BEGIN"}, nil
	case sql.TxnCommit:
		if !s.explicit {
			s.Notices = append(s.Notices, "there is no transaction in progress")
			return &Result{Tag: "COMMIT"}, s.endTxn(true)
		}
		s.explicit = false
		if s.failed {
			s.failed = false
			_ = s.endTxn(false)
			return &Result{Tag: "ROLLBACK"}, nil
		}
		if err := s.endTxn(true); err != nil {
			return nil, err
		}
		return &Result{Tag: "COMMIT"}, nil
	case sql.TxnRollback:
		if !s.explicit {
			s.Notices = append(s.Notices, "there is no transaction in progress")
		}
		s.explicit, s.failed = false, false
		_ = s.endTxn(false)
		return &Result{Tag: "ROLLBACK"}, nil
	}
	return nil, pgerr.Unsupported("savepoints are not supported")
}

// ---- planning and execution ----

type plannedStmt struct {
	plan       planner.Plan
	output     []planner.OutputCol
	tables     []planner.TableUse
	catVersion uint64
	explain    *sql.ExplainStmt
	kind       string // SELECT, INSERT, UPDATE, DELETE
	hasRows    bool
}

func (s *Session) virtual() func(schema, name string) (*planner.VirtualTable, bool) {
	return func(schema, name string) (*planner.VirtualTable, bool) {
		return s.virtualTable(schema, name)
	}
}

func (s *Session) setting(name string) string { return s.settings[name] }

// plan binds and plans a statement against the current catalog.
func (s *Session) plan(st sql.Stmt, paramTypes []types.T) (*plannedStmt, []types.T, error) {
	cat := s.db.Catalog()
	ps := &plannedStmt{catVersion: cat.Version}
	if ex, ok := st.(*sql.ExplainStmt); ok {
		ps.explain = ex
		st = ex.Stmt
	}
	b := planner.NewBinder(cat, s.virtual(), paramTypes)
	rw := &planner.Rewriter{Ctx: &expr.Ctx{Env: s}}
	opt := planner.NewOptimizer(cat, b, rw, s.db.heapPages, s.setting)
	var err error
	switch x := st.(type) {
	case *sql.SelectStmt:
		var q *planner.Query
		if q, err = b.BindQuery(x); err != nil {
			return nil, nil, err
		}
		if ps.plan, err = opt.Optimize(q.Root); err != nil {
			return nil, nil, err
		}
		ps.output, ps.kind, ps.hasRows = q.Output, "SELECT", true
		if x.ForUpdate {
			return nil, nil, pgerr.Unsupported("SELECT FOR UPDATE is not supported")
		}
	case *sql.InsertStmt:
		var ins *planner.Insert
		if ins, err = b.BindInsert(x); err != nil {
			return nil, nil, err
		}
		if ps.plan, err = opt.PlanInsert(ins); err != nil {
			return nil, nil, err
		}
		ps.output, ps.kind, ps.hasRows = ins.Output, "INSERT", len(ins.Returning) > 0
	case *sql.UpdateStmt:
		var up *planner.Update
		if up, err = b.BindUpdate(x); err != nil {
			return nil, nil, err
		}
		if ps.plan, err = opt.PlanUpdate(up); err != nil {
			return nil, nil, err
		}
		ps.output, ps.kind, ps.hasRows = up.Output, "UPDATE", len(up.Returning) > 0
	case *sql.DeleteStmt:
		var del *planner.Delete
		if del, err = b.BindDelete(x); err != nil {
			return nil, nil, err
		}
		if ps.plan, err = opt.PlanDelete(del); err != nil {
			return nil, nil, err
		}
		ps.output, ps.kind, ps.hasRows = del.Output, "DELETE", len(del.Returning) > 0
	default:
		return nil, nil, pgerr.Unsupported("%T cannot be planned", st)
	}
	ps.tables = b.Tables
	if ps.explain != nil {
		ps.output = []planner.OutputCol{{Name: "QUERY PLAN", T: types.Text}}
		ps.hasRows = true
	}
	return ps, b.Params, nil
}

func (s *Session) execPlanned(ctx context.Context, st sql.Stmt, params []types.Value, prep *Prepared) (*Result, error) {
	var ps *plannedStmt
	var err error
	for attempt := 0; ; attempt++ {
		if prep != nil && prep.planned != nil && prep.planned.catVersion == s.db.Catalog().Version {
			ps = prep.planned
		} else {
			var ptypes []types.T
			if prep != nil {
				ptypes = prep.ParamTypes
			}
			if ps, _, err = s.plan(st, ptypes); err != nil {
				return nil, err
			}
			if prep != nil && ps.explain == nil {
				prep.planned = ps
			}
		}
		// Lock the tables, then make sure the plan is still current.
		for _, tu := range ps.tables {
			mode := txn.AccessShare
			if tu.Write {
				mode = txn.RowExclusive
			}
			if err := s.txn.Lock(txn.Tag{Kind: txn.TagRelation, ID: uint64(tu.Oid)}, mode); err != nil {
				return nil, err
			}
		}
		if ps.catVersion == s.db.Catalog().Version || attempt >= 3 {
			break
		}
		if prep != nil {
			prep.planned = nil
		}
	}
	s.txn.Snapshot()
	cat := s.db.Catalog()
	ec := &expr.Ctx{Params: params, Env: s, Check: checkFn(ctx)}
	xc := executor.New(s.txn, s.db.store, cat, ec)
	xc.TableChanged = s.db.noteChange
	defer xc.Close()
	if ps.explain != nil {
		return s.explain(ps, xc, ec)
	}
	if ps.kind == "SELECT" {
		if err := s.checkReadOnly(ps); err != nil {
			return nil, err
		}
	}
	it, err := xc.Build(ps.plan)
	if err != nil {
		return nil, err
	}
	rows, err := executor.Drain(it, ec)
	s.txn.CommandCounterIncrement()
	if err != nil {
		return nil, err
	}
	res := &Result{HasRows: ps.hasRows}
	for _, o := range ps.output {
		res.Columns = append(res.Columns, Column{Name: o.Name, Type: o.T, TableOid: o.TableOid, Attnum: o.Attnum})
	}
	if ps.hasRows {
		res.Rows = s.convertRegs(res.Columns, rows)
	}
	switch ps.kind {
	case "SELECT":
		res.Tag = fmt.Sprintf("SELECT %d", len(rows))
	case "INSERT":
		res.Tag = fmt.Sprintf("INSERT 0 %d", xc.RowsAffected)
	default:
		res.Tag = fmt.Sprintf("%s %d", ps.kind, xc.RowsAffected)
	}
	return res, nil
}

func (s *Session) checkReadOnly(*plannedStmt) error { return nil }

// convertRegs renders reg* values (stored as OIDs) as names.
func (s *Session) convertRegs(cols []Column, rows [][]types.Value) [][]types.Value {
	var idx []int
	for i, c := range cols {
		switch c.Type.Oid {
		case types.OidRegclass, types.OidRegtype, types.OidRegproc, types.OidRegnamespace, types.OidRegrole:
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return rows
	}
	for _, r := range rows {
		for _, i := range idx {
			if r[i].K == types.KInt {
				v, err := s.CatalogFunc("cast", []types.Value{r[i], types.NewInt(int64(cols[i].Type.Oid)), types.NewInt(int64(types.OidText))})
				if err == nil {
					r[i] = v
				}
			}
		}
	}
	return rows
}

func (s *Session) explain(ps *plannedStmt, xc *executor.Ctx, ec *expr.Ctx) (*Result, error) {
	opt := planner.ExplainOptions{Costs: ps.explain.Costs, Analyze: ps.explain.Analyze, Verbose: ps.explain.Verbose}
	var extra []string
	if ps.explain.Analyze {
		xc.Analyze = true
		start := time.Now()
		it, err := xc.Build(ps.plan)
		if err != nil {
			return nil, err
		}
		if _, err := executor.Drain(it, ec); err != nil {
			return nil, err
		}
		s.txn.CommandCounterIncrement()
		extra = append(extra, fmt.Sprintf("Execution Time: %.3f ms", float64(time.Since(start).Microseconds())/1000))
	}
	res := &Result{Columns: []Column{{Name: "QUERY PLAN", Type: types.Text}}, HasRows: true, Tag: "EXPLAIN"}
	for _, l := range append(planner.Explain(ps.plan, opt), extra...) {
		res.Rows = append(res.Rows, []types.Value{types.NewText(l)})
	}
	return res, nil
}

// ---- extended protocol ----

// Prepared is a parsed statement with inferred parameter types.
type Prepared struct {
	SQL        string
	Stmt       sql.Stmt
	ParamTypes []types.T
	Columns    []Column
	HasRows    bool
	planned    *plannedStmt
}

// Prepare parses and analyzes a statement. paramOids may pre-specify
// parameter types (0 = infer).
func (s *Session) Prepare(name, query string, paramOids []uint32) (*Prepared, error) {
	stmts, err := sql.Parse(query)
	if err != nil {
		return nil, err
	}
	if len(stmts) > 1 {
		return nil, pgerr.New(pgerr.SyntaxError, "cannot insert multiple commands into a prepared statement")
	}
	p := &Prepared{SQL: query}
	for _, o := range paramOids {
		t := types.Unknown
		if o != 0 {
			t = types.T{Oid: o, Mod: -1}
			if !types.Known(o) {
				t = types.Text
			}
		}
		p.ParamTypes = append(p.ParamTypes, t)
	}
	if len(stmts) == 0 {
		s.prepared[name] = p
		return p, nil
	}
	p.Stmt = stmts[0].Stmt
	switch st := p.Stmt.(type) {
	case *sql.SelectStmt, *sql.InsertStmt, *sql.UpdateStmt, *sql.DeleteStmt, *sql.ExplainStmt:
		if s.failed {
			return nil, pgerr.New(pgerr.InFailedSQLTransaction, "current transaction is aborted, commands ignored until end of transaction block")
		}
		ps, params, err := s.plan(st, p.ParamTypes)
		if err != nil {
			return nil, err
		}
		p.ParamTypes = params
		p.HasRows = ps.hasRows
		for _, o := range ps.output {
			p.Columns = append(p.Columns, Column{Name: o.Name, Type: o.T, TableOid: o.TableOid, Attnum: o.Attnum})
		}
		if ps.explain == nil {
			p.planned = ps
		}
	case *sql.ShowStmt:
		p.HasRows = true
		p.Columns = []Column{{Name: st.Name, Type: types.Text}}
	}
	s.prepared[name] = p
	return p, nil
}

// Lookup returns a prepared statement.
func (s *Session) Lookup(name string) (*Prepared, bool) {
	p, ok := s.prepared[name]
	return p, ok
}

// ClosePrepared removes a prepared statement.
func (s *Session) ClosePrepared(name string) { delete(s.prepared, name) }

// ExecPrepared runs a prepared statement with parameter values.
func (s *Session) ExecPrepared(p *Prepared, params []types.Value) (*Result, error) {
	if p.Stmt == nil {
		return &Result{Empty: true}, nil
	}
	if len(params) != len(p.ParamTypes) {
		return nil, pgerr.New(pgerr.ProtocolViolation, "bind message supplies %d parameters, but prepared statement requires %d", len(params), len(p.ParamTypes))
	}
	res, err := s.run(p.Stmt, params, p, false)
	if err != nil {
		s.failTxn()
		return nil, err
	}
	return res, nil
}

// ---- SET / SHOW ----

func (s *Session) set(x *sql.SetStmt) (*Result, error) {
	name := strings.ToLower(x.Name)
	if name == "transaction" || name == "transaction_isolation" {
		if x.TxnIsolation == "serializable" {
			return nil, pgerr.Unsupported("SERIALIZABLE isolation is not supported; basalt provides snapshot isolation (REPEATABLE READ)")
		}
		return &Result{Tag: "SET"}, nil
	}
	if x.Reset {
		if name == "all" {
			for k, v := range defaultSettings {
				s.settings[k] = v
			}
		} else if v, ok := defaultSettings[name]; ok {
			s.settings[name] = v
		}
		return &Result{Tag: "SET"}, nil
	}
	v := strings.Trim(x.Value, "'")
	switch name {
	case "timezone":
		if !strings.EqualFold(v, "utc") && !strings.EqualFold(v, "gmt") && v != "0" && !strings.EqualFold(v, "etc/utc") {
			return nil, pgerr.Unsupported("only the UTC time zone is supported")
		}
		v = "UTC"
	case "client_encoding":
		if !strings.EqualFold(strings.ReplaceAll(v, "-", ""), "utf8") && !strings.EqualFold(v, "unicode") {
			return nil, pgerr.Unsupported("only UTF8 client encoding is supported")
		}
		v = "UTF8"
	case "default_transaction_isolation":
		if strings.EqualFold(v, "serializable") {
			return nil, pgerr.Unsupported("SERIALIZABLE isolation is not supported")
		}
	case "datestyle":
		if !strings.Contains(strings.ToUpper(v), "ISO") {
			return nil, pgerr.Unsupported("only the ISO DateStyle is supported")
		}
	case "statement_timeout":
		v = strings.TrimSuffix(v, "ms")
		if _, err := strconv.Atoi(v); err != nil {
			return nil, pgerr.New(pgerr.InvalidParameterValue, "invalid value for parameter \"statement_timeout\": \"%s\"", x.Value)
		}
	}
	s.settings[name] = v
	return &Result{Tag: "SET"}, nil
}

func (s *Session) show(x *sql.ShowStmt) (*Result, error) {
	name := strings.ToLower(x.Name)
	if name == "all" {
		res := &Result{Columns: []Column{{Name: "name", Type: types.Text}, {Name: "setting", Type: types.Text}}, HasRows: true, Tag: "SHOW"}
		keys := make([]string, 0, len(s.settings))
		for k := range s.settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			res.Rows = append(res.Rows, []types.Value{types.NewText(k), types.NewText(s.settings[k])})
		}
		return res, nil
	}
	v, ok := s.settings[name]
	if !ok {
		return nil, pgerr.New(pgerr.UndefinedObject, "unrecognized configuration parameter \"%s\"", x.Name)
	}
	return &Result{Columns: []Column{{Name: name, Type: types.Text}}, Rows: [][]types.Value{{types.NewText(v)}}, HasRows: true, Tag: "SHOW"}, nil
}

// ---- expr.Env ----

// TxnTimestamp implements expr.Env.
func (s *Session) TxnTimestamp() int64 {
	if s.txnStart == 0 {
		return nowMicros()
	}
	return s.txnStart
}

// StatementTimestamp implements expr.Env.
func (s *Session) StatementTimestamp() int64 { return s.stmtStart }

// User implements expr.Env.
func (s *Session) User() string { return s.user }

// Database implements expr.Env.
func (s *Session) Database() string { return s.database }

// BackendPID implements expr.Env.
func (s *Session) BackendPID() int64 { return s.pid }

// Nextval implements expr.Env.
func (s *Session) Nextval(seq string) (int64, error) {
	v, err := s.db.Nextval(seqName(seq))
	if err == nil {
		s.currval[seqName(seq)] = v
	}
	return v, err
}

// Currval implements expr.Env.
func (s *Session) Currval(seq string) (int64, error) {
	v, ok := s.currval[seqName(seq)]
	if !ok {
		return 0, pgerr.New("55000", "currval of sequence \"%s\" is not yet defined in this session", seq)
	}
	return v, nil
}

// Setval implements expr.Env.
func (s *Session) Setval(seq string, v int64, called bool) (int64, error) {
	r, err := s.db.Setval(seqName(seq), v, called)
	if err == nil && called {
		s.currval[seqName(seq)] = v
	}
	return r, err
}

func seqName(s string) string {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "public."), "pg_catalog.")
	if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return strings.ToLower(s)
}

// TxnID implements expr.Env.
func (s *Session) TxnID() int64 {
	if s.txn == nil {
		return 0
	}
	x, err := s.txn.AssignXid()
	if err != nil {
		return 0
	}
	return int64(x)
}

// SettingEnv implements expr.Env's Setting.
func (s *Session) settingEnv(name string) (string, bool) { return s.Setting(name) }

var _ = catalog.QuoteIdent
