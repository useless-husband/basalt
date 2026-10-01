package planner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/types"
)

// TableUse records a table a statement touches, for locking.
type TableUse struct {
	Oid   uint32
	Write bool
}

// Binder resolves names and types. One Binder binds one statement.
type Binder struct {
	Cat *catalog.Catalog
	// Virtual looks up system tables (pg_catalog, information_schema).
	Virtual func(schema, name string) (*VirtualTable, bool)
	// Params holds parameter types; entries may be pre-set by the client
	// and are inferred from context otherwise.
	Params []types.T
	Tables []TableUse

	cols       []ColInfo
	nextSub    int
	ctes       []map[string]*cteDef
	unknownPar []*expr.Param
	defaults   map[*catalog.Column]expr.Expr
}

type cteDef struct {
	name  string
	cols  []string
	query *sql.SelectStmt
	busy  bool
}

type scopeCol struct {
	ID     expr.ColumnID
	Name   string
	Table  string
	Hidden bool
}

// level is the name-resolution scope of one query level.
type level struct {
	cols   []scopeCol
	parent *level
	outer  map[expr.ColumnID]bool
	agg    *aggCtx
	// aggForbidden names the clause in which aggregates are not allowed.
	aggForbidden string
}

func newLevel(parent *level) *level {
	return &level{parent: parent, outer: map[expr.ColumnID]bool{}}
}

// proxy returns a level that shares lvl's outer-reference set but cannot
// see lvl's columns (for non-LATERAL derived tables and CTEs).
func (lvl *level) proxy() *level {
	return &level{parent: lvl.parent, outer: lvl.outer}
}

// NewBinder returns a binder for one statement.
func NewBinder(cat *catalog.Catalog, virtual func(schema, name string) (*VirtualTable, bool), params []types.T) *Binder {
	b := &Binder{Cat: cat, Virtual: virtual, Params: append([]types.T(nil), params...), cols: []ColInfo{{}}}
	b.defaults = map[*catalog.Column]expr.Expr{}
	return b
}

// ColInfo returns the description of a column.
func (b *Binder) ColInfo(id expr.ColumnID) ColInfo { return b.cols[id] }

// Columns returns all column descriptions (index = ColumnID).
func (b *Binder) Columns() []ColInfo { return b.cols }

func (b *Binder) newCol(info ColInfo) expr.ColumnID {
	b.cols = append(b.cols, info)
	return expr.ColumnID(len(b.cols) - 1)
}

func (b *Binder) colRef(id expr.ColumnID) *expr.Col {
	ci := b.cols[id]
	name := ci.Name
	if ci.Table != "" {
		name = ci.Table + "." + ci.Name
	}
	return &expr.Col{ID: id, T: ci.T, Name: name}
}

func (b *Binder) useTable(oid uint32, write bool) {
	for i, t := range b.Tables {
		if t.Oid == oid {
			b.Tables[i].Write = b.Tables[i].Write || write
			return
		}
	}
	b.Tables = append(b.Tables, TableUse{oid, write})
}

// finish resolves parameters whose type could not be inferred to text.
func (b *Binder) finish() {
	for i, t := range b.Params {
		if t.Oid == types.OidUnknown || t.Oid == 0 {
			b.Params[i] = types.Text
		}
	}
	for _, p := range b.unknownPar {
		p.T = b.Params[p.N-1]
	}
}

func errorAt(code string, pos int, format string, args ...any) error {
	e := pgerr.New(code, format, args...)
	if pos >= 0 {
		e.Position = pos + 1
	}
	return e
}

// ---- statements ----

// BindQuery binds a SELECT.
func (b *Binder) BindQuery(s *sql.SelectStmt) (*Query, error) {
	bq, err := b.bindSelect(s, nil)
	if err != nil {
		return nil, err
	}
	// Unknown-typed outputs (string literals) are text.
	node, outs, err := b.resolveUnknownOutputs(bq.node, bq.outs)
	if err != nil {
		return nil, err
	}
	b.finish()
	q := &Query{Root: node}
	for _, o := range outs {
		ci := b.cols[o.id]
		q.Output = append(q.Output, OutputCol{Name: o.name, ID: o.id, T: ci.T, TableOid: ci.TableOid, Attnum: ci.Attnum})
	}
	return q, nil
}

func (b *Binder) resolveUnknownOutputs(node Node, outs []outCol) (Node, []outCol, error) {
	needs := false
	for _, o := range outs {
		if b.cols[o.id].T.Oid == types.OidUnknown {
			needs = true
		}
	}
	if !needs {
		return node, outs, nil
	}
	p := &Project{Input: node}
	var nouts []outCol
	for _, o := range outs {
		e := expr.Expr(b.colRef(o.id))
		id := o.id
		if b.cols[o.id].T.Oid == types.OidUnknown {
			ce, err := b.coerce(e, types.Text, coerceImplicit)
			if err != nil {
				return nil, nil, err
			}
			e = ce
			id = b.newCol(ColInfo{Name: o.name, T: types.Text})
		}
		p.Exprs = append(p.Exprs, e)
		p.ColIDs = append(p.ColIDs, id)
		nouts = append(nouts, outCol{name: o.name, id: id})
	}
	return p, nouts, nil
}

type outCol struct {
	name string
	id   expr.ColumnID
}

type boundQuery struct {
	node  Node
	outs  []outCol
	outer map[expr.ColumnID]bool
}

func (b *Binder) pushCTEs(w *sql.With) error {
	if w == nil {
		return nil
	}
	if w.Recursive {
		return pgerr.Unsupported("WITH RECURSIVE is not supported")
	}
	m := map[string]*cteDef{}
	for _, c := range w.CTEs {
		if _, dup := m[c.Name]; dup {
			return pgerr.New(pgerr.DuplicateObject, "WITH query name \"%s\" specified more than once", c.Name)
		}
		m[c.Name] = &cteDef{name: c.Name, cols: c.Columns, query: c.Query}
	}
	b.ctes = append(b.ctes, m)
	return nil
}

func (b *Binder) popCTEs(w *sql.With) {
	if w != nil && !w.Recursive {
		b.ctes = b.ctes[:len(b.ctes)-1]
	}
}

func (b *Binder) lookupCTE(name string) *cteDef {
	for i := len(b.ctes) - 1; i >= 0; i-- {
		if c, ok := b.ctes[i][name]; ok {
			return c
		}
	}
	return nil
}

func (b *Binder) bindSelect(s *sql.SelectStmt, parent *level) (*boundQuery, error) {
	if err := b.pushCTEs(s.With); err != nil {
		return nil, err
	}
	defer b.popCTEs(s.With)
	if s.SetOp == sql.SetNone && s.Values == nil {
		return b.bindSimple(s, parent)
	}
	var bq *boundQuery
	var err error
	if s.SetOp != sql.SetNone {
		bq, err = b.bindSetOp(s, parent)
	} else {
		bq, err = b.bindValues(s.Values, parent, nil)
	}
	if err != nil {
		return nil, err
	}
	// ORDER BY / LIMIT over the output columns.
	lvl := newLevel(parent)
	lvl.outer = bq.outer
	for _, o := range bq.outs {
		lvl.cols = append(lvl.cols, scopeCol{ID: o.id, Name: o.name})
	}
	if len(s.OrderBy) > 0 {
		var keys []expr.SortKey
		for _, it := range s.OrderBy {
			var e expr.Expr
			if lit, ok := it.Expr.(*sql.Literal); ok && lit.Kind == sql.LitInt {
				n, _ := strconv.Atoi(lit.Val)
				if n < 1 || n > len(bq.outs) {
					return nil, errorAt(pgerr.InvalidColumnReference, lit.Pos, "ORDER BY position %d is not in select list", n)
				}
				e = b.colRef(bq.outs[n-1].id)
			} else {
				e, err = b.bindExpr(it.Expr, lvl)
				if err != nil {
					return nil, err
				}
			}
			keys = append(keys, sortKey(e, it))
		}
		bq.node = &Sort{Input: bq.node, Keys: keys}
	}
	if bq.node, err = b.bindLimit(s, bq.node, lvl); err != nil {
		return nil, err
	}
	return bq, nil
}

func sortKey(e expr.Expr, it *sql.OrderItem) expr.SortKey {
	k := expr.SortKey{E: e, Desc: it.Desc, NullsFirst: it.Desc}
	if it.NullsFirst != nil {
		k.NullsFirst = *it.NullsFirst
	}
	return k
}

func (b *Binder) bindLimit(s *sql.SelectStmt, node Node, lvl *level) (Node, error) {
	if s.Limit == nil && s.Offset == nil {
		return node, nil
	}
	l := &Limit{Input: node}
	bindN := func(e sql.Expr) (expr.Expr, error) {
		x, err := b.bindExpr(e, &level{outer: map[expr.ColumnID]bool{}, parent: lvl.parent})
		if err != nil {
			return nil, err
		}
		return b.coerce(x, types.Int8, coerceAssign)
	}
	var err error
	if s.Limit != nil {
		if l.Limit, err = bindN(s.Limit); err != nil {
			return nil, err
		}
	}
	if s.Offset != nil {
		if l.Offset, err = bindN(s.Offset); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (b *Binder) bindValues(rows [][]sql.Expr, parent *level, target []types.T) (*boundQuery, error) {
	lvl := newLevel(parent)
	n := len(rows[0])
	bound := make([][]expr.Expr, len(rows))
	for i, r := range rows {
		if len(r) != n {
			return nil, pgerr.New(pgerr.SyntaxError, "VALUES lists must all be the same length")
		}
		bound[i] = make([]expr.Expr, n)
		for j, e := range r {
			if _, ok := e.(*sql.DefaultExpr); ok {
				return nil, pgerr.New(pgerr.SyntaxError, "DEFAULT is not allowed in this context")
			}
			x, err := b.bindExpr(e, lvl)
			if err != nil {
				return nil, err
			}
			bound[i][j] = x
		}
	}
	v := &Values{Rows: bound}
	var outs []outCol
	for j := 0; j < n; j++ {
		var t types.T
		if target != nil && j < len(target) {
			t = target[j]
		} else {
			t = types.Unknown
			for _, r := range bound {
				ct, ok := expr.CommonType(t, r[j].Type())
				if !ok {
					return nil, pgerr.New(pgerr.DatatypeMismatch, "VALUES types %s and %s cannot be matched", t, r[j].Type())
				}
				t = ct
			}
			if t.Oid == types.OidUnknown {
				t = types.Text
			}
		}
		for _, r := range bound {
			c, err := b.coerce(r[j], t, coerceAssignIf(target != nil))
			if err != nil {
				return nil, err
			}
			r[j] = c
		}
		name := fmt.Sprintf("column%d", j+1)
		id := b.newCol(ColInfo{Name: name, T: t})
		v.ColIDs = append(v.ColIDs, id)
		outs = append(outs, outCol{name, id})
	}
	return &boundQuery{node: v, outs: outs, outer: lvl.outer}, nil
}

func coerceAssignIf(assign bool) coerceMode {
	if assign {
		return coerceAssign
	}
	return coerceImplicit
}

func (b *Binder) bindSetOp(s *sql.SelectStmt, parent *level) (*boundQuery, error) {
	l, err := b.bindSelect(s.Left, parent)
	if err != nil {
		return nil, err
	}
	r, err := b.bindSelect(s.Right, parent)
	if err != nil {
		return nil, err
	}
	if len(l.outs) != len(r.outs) {
		return nil, pgerr.New(pgerr.SyntaxError, "each %s query must have the same number of columns", s.SetOp)
	}
	outer := l.outer
	for k := range r.outer {
		outer[k] = true
	}
	// Unify column types, casting either side as needed.
	castSide := func(q *boundQuery, ts []types.T) (Node, []expr.ColumnID, error) {
		need := false
		for i, o := range q.outs {
			if b.cols[o.id].T != ts[i] {
				need = true
			}
		}
		ids := make([]expr.ColumnID, len(q.outs))
		if !need {
			for i, o := range q.outs {
				ids[i] = o.id
			}
			return q.node, ids, nil
		}
		p := &Project{Input: q.node}
		for i, o := range q.outs {
			e, err := b.coerce(b.colRef(o.id), ts[i], coerceImplicit)
			if err != nil {
				return nil, nil, err
			}
			id := b.newCol(ColInfo{Name: o.name, T: ts[i]})
			p.Exprs = append(p.Exprs, e)
			p.ColIDs = append(p.ColIDs, id)
			ids[i] = id
		}
		return p, ids, nil
	}
	ts := make([]types.T, len(l.outs))
	for i := range l.outs {
		lt, rt := b.cols[l.outs[i].id].T, b.cols[r.outs[i].id].T
		ct, ok := expr.CommonType(lt, rt)
		if !ok {
			return nil, pgerr.New(pgerr.DatatypeMismatch, "%s types %s and %s cannot be matched", s.SetOp, lt, rt)
		}
		if ct.Oid == types.OidUnknown {
			ct = types.Text
		}
		ts[i] = ct
	}
	ln, _, err := castSide(l, ts)
	if err != nil {
		return nil, err
	}
	rn, _, err := castSide(r, ts)
	if err != nil {
		return nil, err
	}
	so := &SetOp{Kind: int(s.SetOp), All: s.SetAll, Left: ln, Right: rn}
	var outs []outCol
	for i, o := range l.outs {
		id := b.newCol(ColInfo{Name: o.name, T: ts[i]})
		so.ColIDs = append(so.ColIDs, id)
		outs = append(outs, outCol{o.name, id})
	}
	return &boundQuery{node: so, outs: outs, outer: outer}, nil
}

// ---- simple SELECT ----

type aggCtx struct {
	groupExprs []expr.Expr
	groupCols  []expr.ColumnID
	calls      []*expr.AggCall
	fromIDs    map[expr.ColumnID]bool
}

func (b *Binder) bindSimple(s *sql.SelectStmt, parent *level) (*boundQuery, error) {
	lvl := newLevel(parent)
	var node Node
	var err error
	if len(s.From) == 0 {
		node = &Values{Rows: [][]expr.Expr{{}}}
	} else if node, err = b.bindFrom(s.From, lvl); err != nil {
		return nil, err
	}
	if s.Where != nil {
		lvl.aggForbidden = "WHERE"
		w, err := b.bindExpr(s.Where, lvl)
		if err != nil {
			return nil, err
		}
		if w, err = b.coerceBool(w, "WHERE"); err != nil {
			return nil, err
		}
		lvl.aggForbidden = ""
		node = &Filter{Input: node, Cond: w}
	}
	// Expand the target list.
	type target struct {
		ast  sql.Expr
		name string
		pos  int
		col  *expr.Col // for * expansion
	}
	var targets []target
	for _, t := range s.Targets {
		if t.Star {
			n := 0
			for _, c := range lvl.cols {
				if c.Hidden || (t.StarTable != "" && c.Table != t.StarTable) {
					continue
				}
				targets = append(targets, target{name: c.Name, col: b.colRef(c.ID), pos: t.Pos})
				n++
			}
			if t.StarTable != "" && n == 0 {
				return nil, errorAt(pgerr.UndefinedTable, t.Pos, "missing FROM-clause entry for table \"%s\"", t.StarTable)
			}
			if t.StarTable == "" && len(s.From) == 0 {
				return nil, errorAt(pgerr.SyntaxError, t.Pos, "SELECT * with no tables specified is not valid")
			}
			continue
		}
		name := t.Alias
		if name == "" {
			name = exprName(t.Expr)
		}
		targets = append(targets, target{ast: t.Expr, name: name, pos: t.Pos})
	}
	hasAgg := len(s.GroupBy) > 0 || s.Having != nil
	for _, t := range targets {
		if t.ast != nil && containsAgg(t.ast) {
			hasAgg = true
		}
	}
	for _, o := range s.OrderBy {
		if containsAgg(o.Expr) {
			hasAgg = true
		}
	}
	if hasAgg {
		agg := &aggCtx{fromIDs: map[expr.ColumnID]bool{}}
		for _, c := range lvl.cols {
			agg.fromIDs[c.ID] = true
		}
		for _, g := range s.GroupBy {
			var ge expr.Expr
			if lit, ok := g.(*sql.Literal); ok && lit.Kind == sql.LitInt {
				n, _ := strconv.Atoi(lit.Val)
				if n < 1 || n > len(targets) {
					return nil, errorAt(pgerr.InvalidColumnReference, lit.Pos, "GROUP BY position %d is not in select list", n)
				}
				t := targets[n-1]
				if t.col != nil {
					ge = t.col
				} else if ge, err = b.bindExprNoAgg(t.ast, lvl, "GROUP BY"); err != nil {
					return nil, err
				}
			} else if cr, ok := g.(*sql.ColumnRef); ok && len(cr.Parts) == 1 && !b.inScope(lvl, cr.Parts[0]) {
				found := false
				for _, t := range targets {
					if t.name == cr.Parts[0] && t.ast != nil {
						if ge, err = b.bindExprNoAgg(t.ast, lvl, "GROUP BY"); err != nil {
							return nil, err
						}
						found = true
						break
					}
				}
				if !found {
					if ge, err = b.bindExprNoAgg(g, lvl, "GROUP BY"); err != nil {
						return nil, err
					}
				}
			} else if ge, err = b.bindExprNoAgg(g, lvl, "GROUP BY"); err != nil {
				return nil, err
			}
			dup := false
			for _, x := range agg.groupExprs {
				if expr.Equal(x, ge) {
					dup = true
				}
			}
			if dup {
				continue
			}
			agg.groupExprs = append(agg.groupExprs, ge)
			info := ColInfo{Name: exprName(g), T: ge.Type()}
			if c, ok := ge.(*expr.Col); ok {
				info = b.cols[c.ID]
			}
			agg.groupCols = append(agg.groupCols, b.newCol(info))
		}
		lvl.agg = agg
	}
	// Bind the targets.
	texprs := make([]expr.Expr, len(targets))
	for i, t := range targets {
		var e expr.Expr = t.col
		if t.ast != nil {
			if e, err = b.bindExpr(t.ast, lvl); err != nil {
				return nil, err
			}
		}
		texprs[i] = e
	}
	var having expr.Expr
	if s.Having != nil {
		if having, err = b.bindExpr(s.Having, lvl); err != nil {
			return nil, err
		}
		if having, err = b.coerceBool(having, "HAVING"); err != nil {
			return nil, err
		}
	}
	// ORDER BY: output column names and positions first, then expressions.
	var orderExprs []expr.Expr
	var orderTarget []int // index of the target an ORDER BY item refers to, or -1
	for _, it := range s.OrderBy {
		ti := -1
		if lit, ok := it.Expr.(*sql.Literal); ok && lit.Kind == sql.LitInt {
			n, _ := strconv.Atoi(lit.Val)
			if n < 1 || n > len(targets) {
				return nil, errorAt(pgerr.InvalidColumnReference, lit.Pos, "ORDER BY position %d is not in select list", n)
			}
			ti = n - 1
		} else if cr, ok := it.Expr.(*sql.ColumnRef); ok && len(cr.Parts) == 1 {
			for i, t := range targets {
				if t.name == cr.Parts[0] {
					if ti >= 0 && !expr.Equal(texprs[ti], texprs[i]) {
						return nil, errorAt(pgerr.AmbiguousColumn, cr.Pos, "ORDER BY \"%s\" is ambiguous", cr.Parts[0])
					}
					ti = i
				}
			}
		}
		if ti >= 0 {
			orderExprs = append(orderExprs, texprs[ti])
		} else {
			e, err := b.bindExpr(it.Expr, lvl)
			if err != nil {
				return nil, err
			}
			orderExprs = append(orderExprs, e)
		}
		orderTarget = append(orderTarget, ti)
	}
	var distinctOn []expr.Expr
	for _, d := range s.DistinctOn {
		var e expr.Expr
		if lit, ok := d.(*sql.Literal); ok && lit.Kind == sql.LitInt {
			n, _ := strconv.Atoi(lit.Val)
			if n < 1 || n > len(targets) {
				return nil, errorAt(pgerr.InvalidColumnReference, lit.Pos, "SELECT DISTINCT ON position %d is not in select list", n)
			}
			e = texprs[n-1]
		} else if e, err = b.bindExpr(d, lvl); err != nil {
			return nil, err
		}
		distinctOn = append(distinctOn, e)
	}
	if lvl.agg != nil {
		agg := lvl.agg
		rw := func(e expr.Expr) (expr.Expr, error) { return b.aggRewrite(agg, e) }
		for i := range texprs {
			if texprs[i], err = rw(texprs[i]); err != nil {
				return nil, err
			}
		}
		if having != nil {
			if having, err = rw(having); err != nil {
				return nil, err
			}
		}
		for i := range orderExprs {
			if orderExprs[i], err = rw(orderExprs[i]); err != nil {
				return nil, err
			}
		}
		for i := range distinctOn {
			if distinctOn[i], err = rw(distinctOn[i]); err != nil {
				return nil, err
			}
		}
		node = &Aggregate{Input: node, GroupBy: agg.groupExprs, GroupCols: agg.groupCols, Aggs: agg.calls}
		if having != nil {
			node = &Filter{Input: node, Cond: having}
		}
		lvl.agg = nil
	}
	// Projection.
	proj := &Project{Input: node}
	var outs []outCol
	for i, t := range targets {
		e := texprs[i]
		info := ColInfo{Name: t.name, T: e.Type()}
		if c, ok := e.(*expr.Col); ok {
			ci := b.cols[c.ID]
			info.TableOid, info.Attnum, info.Table = ci.TableOid, ci.Attnum, ci.Table
		}
		id := b.newCol(info)
		proj.Exprs = append(proj.Exprs, e)
		proj.ColIDs = append(proj.ColIDs, id)
		outs = append(outs, outCol{t.name, id})
	}
	keys := make([]expr.SortKey, len(orderExprs))
	for i, e := range orderExprs {
		keys[i] = sortKey(e, s.OrderBy[i])
	}
	switch {
	case len(distinctOn) > 0:
		// DISTINCT ON: sort by the ON expressions (then ORDER BY), keep the
		// first row of each group, then project.
		for i, k := range keys {
			if i < len(distinctOn) && !expr.Equal(k.E, distinctOn[i]) {
				return nil, pgerr.New(pgerr.InvalidColumnReference, "SELECT DISTINCT ON expressions must match initial ORDER BY expressions")
			}
		}
		var all []expr.SortKey
		for i, d := range distinctOn {
			if i < len(keys) {
				all = append(all, keys[i])
			} else {
				all = append(all, expr.SortKey{E: d})
			}
		}
		if len(keys) > len(distinctOn) {
			all = append(all, keys[len(distinctOn):]...)
		}
		node = &Sort{Input: node, Keys: all}
		node = &Distinct{Input: node, On: distinctOn}
		proj.Input = node
		node = proj
	case s.Distinct:
		node = &Distinct{Input: proj}
		if len(keys) > 0 {
			// Sort keys must be output columns.
			for i := range keys {
				ti := orderTarget[i]
				if ti < 0 {
					for j, te := range texprs {
						if expr.Equal(te, keys[i].E) {
							ti = j
							break
						}
					}
				}
				if ti < 0 {
					return nil, pgerr.New(pgerr.InvalidColumnReference, "for SELECT DISTINCT, ORDER BY expressions must appear in select list")
				}
				keys[i].E = b.colRef(proj.ColIDs[ti])
			}
			node = &Sort{Input: node, Keys: keys}
		}
	default:
		if len(keys) > 0 {
			node = &Sort{Input: node, Keys: keys}
		}
		proj.Input = node
		node = proj
	}
	if node, err = b.bindLimit(s, node, lvl); err != nil {
		return nil, err
	}
	return &boundQuery{node: node, outs: outs, outer: lvl.outer}, nil
}

func (b *Binder) inScope(lvl *level, name string) bool {
	for _, c := range lvl.cols {
		if c.Name == name && !c.Hidden {
			return true
		}
	}
	return false
}

func (b *Binder) bindExprNoAgg(e sql.Expr, lvl *level, clause string) (expr.Expr, error) {
	saved, savedF := lvl.agg, lvl.aggForbidden
	lvl.agg, lvl.aggForbidden = nil, clause
	defer func() { lvl.agg, lvl.aggForbidden = saved, savedF }()
	return b.bindExpr(e, lvl)
}

// aggRewrite replaces grouped expressions by the Aggregate's group columns
// and rejects references to ungrouped columns.
func (b *Binder) aggRewrite(agg *aggCtx, e expr.Expr) (expr.Expr, error) {
	isAggOut := func(id expr.ColumnID) bool {
		for _, a := range agg.calls {
			if a.ID == id {
				return true
			}
		}
		return false
	}
	var bad error
	out := expr.Replace(e, func(n expr.Expr) expr.Expr {
		for i, g := range agg.groupExprs {
			if expr.Equal(n, g) {
				return b.colRef(agg.groupCols[i])
			}
		}
		switch x := n.(type) {
		case *expr.Col:
			if agg.fromIDs[x.ID] && !isAggOut(x.ID) && bad == nil {
				bad = pgerr.New(pgerr.GroupingError, "column \"%s\" must appear in the GROUP BY clause or be used in an aggregate function", x.Name)
			}
		case *expr.Subquery:
			c := *x
			for _, id := range x.Outer {
				if !agg.fromIDs[id] {
					continue
				}
				mapped := false
				for i, g := range agg.groupExprs {
					if gc, ok := g.(*expr.Col); ok && gc.ID == id {
						if c.Remap == nil {
							c.Remap = map[expr.ColumnID]expr.ColumnID{}
						}
						c.Remap[id] = agg.groupCols[i]
						mapped = true
					}
				}
				if !mapped && bad == nil {
					bad = pgerr.New(pgerr.GroupingError, "subquery uses ungrouped column \"%s\" from outer query", b.cols[id].Name)
				}
			}
			if c.Arg != nil {
				a, err := b.aggRewrite(agg, c.Arg)
				if err != nil && bad == nil {
					bad = err
				}
				c.Arg = a
			}
			return &c
		}
		return nil
	})
	return out, bad
}

// containsAgg reports whether an AST expression calls an aggregate outside
// subqueries.
func containsAgg(e sql.Expr) bool {
	found := false
	var walk func(sql.Expr)
	walkAll := func(es []sql.Expr) {
		for _, x := range es {
			walk(x)
		}
	}
	walk = func(e sql.Expr) {
		if e == nil || found {
			return
		}
		switch x := e.(type) {
		case *sql.FuncCall:
			if expr.IsAggregate(x.Name) && x.Over == nil {
				found = true
				return
			}
			walkAll(x.Args)
		case *sql.BinaryExpr:
			walk(x.Left)
			walk(x.Right)
		case *sql.UnaryExpr:
			walk(x.Expr)
		case *sql.CastExpr:
			walk(x.Expr)
		case *sql.CaseExpr:
			walk(x.Operand)
			for _, w := range x.Whens {
				walk(w.Cond)
				walk(w.Result)
			}
			walk(x.Else)
		case *sql.IsExpr:
			walk(x.Expr)
		case *sql.IsDistinctExpr:
			walk(x.Left)
			walk(x.Right)
		case *sql.BetweenExpr:
			walk(x.Expr)
			walk(x.Low)
			walk(x.High)
		case *sql.InExpr:
			walk(x.Expr)
			walkAll(x.List)
		case *sql.LikeExpr:
			walk(x.Expr)
			walk(x.Pattern)
		case *sql.AnyAllExpr:
			walk(x.Left)
			walk(x.Right)
		case *sql.ArrayExpr:
			walkAll(x.Elems)
		case *sql.SubscriptExpr:
			walk(x.Expr)
			walk(x.Index)
		case *sql.RowExpr:
			walkAll(x.Exprs)
		}
	}
	walk(e)
	return found
}

// exprName derives an output column name the way PostgreSQL does.
func exprName(e sql.Expr) string {
	switch x := e.(type) {
	case *sql.ColumnRef:
		return x.Parts[len(x.Parts)-1]
	case *sql.FuncCall:
		switch x.Name {
		case "current_date", "current_timestamp", "current_user", "session_user", "localtimestamp", "current_time", "user", "current_role", "current_catalog", "current_schema":
			return x.Name
		case "btrim", "ltrim", "rtrim":
			return x.Name
		}
		return x.Name
	case *sql.CastExpr:
		if inner := exprName(x.Expr); inner != "?column?" {
			return inner
		}
		t, err := resolveTypeName(x.Type)
		if err == nil {
			return t.TypName()
		}
		return x.Type.Name
	case *sql.CaseExpr:
		return "case"
	case *sql.ExistsExpr:
		return "exists"
	case *sql.ArrayExpr:
		return "array"
	case *sql.SubqueryExpr:
		if len(x.Query.Targets) == 1 && x.Query.Targets[0].Alias != "" {
			return x.Query.Targets[0].Alias
		}
		if len(x.Query.Targets) == 1 && x.Query.Targets[0].Expr != nil {
			return exprName(x.Query.Targets[0].Expr)
		}
	case *sql.RowExpr:
		return "row"
	}
	return "?column?"
}

// ---- FROM ----

func (b *Binder) bindFrom(items []sql.TableExpr, lvl *level) (Node, error) {
	var node Node
	for _, it := range items {
		n, cols, err := b.bindTableExpr(it, lvl)
		if err != nil {
			return nil, err
		}
		for _, c := range cols {
			for _, e := range lvl.cols {
				if c.Table != "" && e.Table == c.Table && e.ID != c.ID && e.Name == c.Name && !c.Hidden && !e.Hidden {
					if c.Table != "" {
						return nil, pgerr.New(pgerr.DuplicateTable, "table name \"%s\" specified more than once", c.Table)
					}
				}
			}
		}
		lvl.cols = append(lvl.cols, cols...)
		if node == nil {
			node = n
		} else {
			node = &Join{Kind: JoinInner, Left: node, Right: n}
		}
	}
	return node, nil
}

func (b *Binder) bindTableExpr(te sql.TableExpr, lvl *level) (Node, []scopeCol, error) {
	switch x := te.(type) {
	case *sql.TableName:
		return b.bindTableName(x, lvl)
	case *sql.SubqueryTable:
		if x.Lateral {
			return nil, nil, pgerr.Unsupported("LATERAL subqueries are not supported")
		}
		bq, err := b.bindSelect(x.Query, lvl.proxy())
		if err != nil {
			return nil, nil, err
		}
		alias := x.Alias
		return b.aliasOutputs(bq.node, bq.outs, alias, x.ColAliases)
	case *sql.FuncTable:
		return b.bindFuncTable(x, lvl)
	case *sql.JoinExpr:
		return b.bindJoin(x, lvl)
	}
	return nil, nil, pgerr.Internal("unknown FROM item %T", te)
}

func (b *Binder) aliasOutputs(node Node, outs []outCol, alias string, colAliases []string) (Node, []scopeCol, error) {
	if len(colAliases) > len(outs) {
		return nil, nil, pgerr.New(pgerr.InvalidColumnReference, "table \"%s\" has %d columns available but %d columns specified", alias, len(outs), len(colAliases))
	}
	cols := make([]scopeCol, len(outs))
	for i, o := range outs {
		name := o.name
		if i < len(colAliases) {
			name = colAliases[i]
			b.cols[o.id].Name = name
		}
		b.cols[o.id].Table = alias
		cols[i] = scopeCol{ID: o.id, Name: name, Table: alias}
	}
	return node, cols, nil
}

func (b *Binder) bindTableName(x *sql.TableName, lvl *level) (Node, []scopeCol, error) {
	alias := x.Alias
	if alias == "" {
		alias = x.Name
	}
	if x.Schema == "" {
		if cte := b.lookupCTE(x.Name); cte != nil {
			if cte.busy {
				return nil, nil, pgerr.Unsupported("recursive reference to query \"%s\" is not supported", x.Name)
			}
			cte.busy = true
			bq, err := b.bindSelect(cte.query, lvl.proxy())
			cte.busy = false
			if err != nil {
				return nil, nil, err
			}
			names := cte.cols
			if len(x.ColAliases) > 0 {
				names = x.ColAliases
			}
			// Each reference gets fresh output columns.
			p := &Project{Input: bq.node}
			var outs []outCol
			for _, o := range bq.outs {
				id := b.newCol(ColInfo{Name: o.name, T: b.cols[o.id].T})
				p.Exprs = append(p.Exprs, b.colRef(o.id))
				p.ColIDs = append(p.ColIDs, id)
				outs = append(outs, outCol{o.name, id})
			}
			return b.aliasOutputs(p, outs, alias, names)
		}
	}
	if x.Schema == "pg_catalog" || x.Schema == "information_schema" || (x.Schema == "" && strings.HasPrefix(x.Name, "pg_")) {
		if b.Virtual != nil {
			schema := x.Schema
			if schema == "" {
				schema = "pg_catalog"
			}
			if vt, ok := b.Virtual(schema, x.Name); ok {
				vs := &VScan{VT: vt, Alias: alias}
				var cols []scopeCol
				for i, c := range vt.Columns {
					name := c.Name
					if i < len(x.ColAliases) {
						name = x.ColAliases[i]
					}
					id := b.newCol(ColInfo{Name: name, T: c.T, Table: alias})
					vs.ColIDs = append(vs.ColIDs, id)
					cols = append(cols, scopeCol{ID: id, Name: name, Table: alias})
				}
				return vs, cols, nil
			}
		}
		if x.Schema != "" {
			return nil, nil, errorAt(pgerr.UndefinedTable, x.Pos, "relation \"%s.%s\" does not exist", x.Schema, x.Name)
		}
	}
	if x.Schema != "" && x.Schema != "public" {
		return nil, nil, errorAt(pgerr.UndefinedTable, x.Pos, "relation \"%s.%s\" does not exist", x.Schema, x.Name)
	}
	t := b.Cat.TableByName(x.Name)
	if t == nil {
		return nil, nil, errorAt(pgerr.UndefinedTable, x.Pos, "relation \"%s\" does not exist", x.Name)
	}
	b.useTable(t.Oid, false)
	scan, cols := b.newScan(t, alias, x.ColAliases)
	return scan, cols, nil
}

func (b *Binder) newScan(t *catalog.Table, alias string, colAliases []string) (*Scan, []scopeCol) {
	scan := &Scan{Table: t, Alias: alias}
	var cols []scopeCol
	for i, c := range t.Columns {
		name := c.Name
		if i < len(colAliases) {
			name = colAliases[i]
		}
		id := b.newCol(ColInfo{Name: name, T: c.Type, Table: alias, TableOid: t.Oid, Attnum: int16(i + 1)})
		scan.ColIDs = append(scan.ColIDs, id)
		cols = append(cols, scopeCol{ID: id, Name: name, Table: alias})
	}
	return scan, cols
}

func (b *Binder) bindFuncTable(x *sql.FuncTable, lvl *level) (Node, []scopeCol, error) {
	fc := x.Func
	alias := x.Alias
	if alias == "" {
		alias = fc.Name
	}
	tmp := lvl.proxy()
	var args []expr.Expr
	for _, a := range fc.Args {
		e, err := b.bindExpr(a, tmp)
		if err != nil {
			return nil, nil, err
		}
		args = append(args, e)
	}
	var t types.T
	switch fc.Name {
	case "generate_series":
		if len(args) < 2 || len(args) > 3 {
			return nil, nil, pgerr.New(pgerr.UndefinedFunction, "function generate_series with %d arguments does not exist", len(args))
		}
		k0, k1 := args[0].Type().Kind(), args[1].Type().Kind()
		if k0 == types.KTimestamp || k0 == types.KTimestampTZ || k0 == types.KDate || k1 == types.KTimestamp {
			t = types.Timestamp
			if k0 == types.KTimestampTZ {
				t = types.TimestampTZ
			}
			for i := 0; i < 2; i++ {
				c, err := b.coerce(args[i], t, coerceAssign)
				if err != nil {
					return nil, nil, err
				}
				args[i] = c
			}
			if len(args) != 3 {
				return nil, nil, pgerr.New(pgerr.UndefinedFunction, "generate_series over timestamps needs an interval step")
			}
			c, err := b.coerce(args[2], types.IntervalT, coerceAssign)
			if err != nil {
				return nil, nil, err
			}
			args[2] = c
		} else {
			t = types.Int4
			for _, a := range args {
				if a.Type().Oid == types.OidInt8 {
					t = types.Int8
				}
				if a.Type().Oid == types.OidNumeric {
					t = types.Numeric
				}
			}
			for i := range args {
				c, err := b.coerce(args[i], t, coerceAssign)
				if err != nil {
					return nil, nil, err
				}
				args[i] = c
			}
		}
	case "unnest":
		if len(args) != 1 || !args[0].Type().IsArray() {
			return nil, nil, pgerr.New(pgerr.UndefinedFunction, "function unnest requires one array argument")
		}
		t = args[0].Type().Elem()
	default:
		return nil, nil, pgerr.Unsupported("set-returning function %s is not supported in FROM", fc.Name)
	}
	name := fc.Name
	if len(x.ColAliases) > 0 {
		name = x.ColAliases[0]
	} else if x.Alias != "" {
		name = x.Alias
	}
	id := b.newCol(ColInfo{Name: name, T: t, Table: alias})
	return &FuncScan{Name: fc.Name, Args: args, ColIDs: []expr.ColumnID{id}, T: t}, []scopeCol{{ID: id, Name: name, Table: alias}}, nil
}

func (b *Binder) bindJoin(j *sql.JoinExpr, lvl *level) (Node, []scopeCol, error) {
	ln, lcols, err := b.bindTableExpr(j.Left, lvl)
	if err != nil {
		return nil, nil, err
	}
	rn, rcols, err := b.bindTableExpr(j.Right, lvl)
	if err != nil {
		return nil, nil, err
	}
	kind := JoinInner
	switch j.Type {
	case sql.JoinLeft:
		kind = JoinLeft
	case sql.JoinRight:
		kind = JoinLeft
		ln, rn = rn, ln
		lcols, rcols = rcols, lcols
	case sql.JoinFull:
		kind = JoinFull
	}
	join := &Join{Kind: kind, Left: ln, Right: rn}
	cols := append(append([]scopeCol(nil), lcols...), rcols...)
	if j.Type == sql.JoinRight {
		// Keep the written column order: left table's columns first.
		cols = append(append([]scopeCol(nil), rcols...), lcols...)
	}
	using := j.Using
	if j.Natural {
		for _, l := range lcols {
			for _, r := range rcols {
				if l.Name == r.Name && !l.Hidden && !r.Hidden {
					using = append(using, l.Name)
				}
			}
		}
	}
	tmp := &level{cols: cols, parent: lvl.parent, outer: lvl.outer}
	var node Node = join
	if len(using) > 0 {
		var conds []expr.Expr
		var proj *Project
		for _, name := range using {
			find := func(cs []scopeCol, side string) (scopeCol, error) {
				var found []scopeCol
				for _, c := range cs {
					if c.Name == name && !c.Hidden {
						found = append(found, c)
					}
				}
				if len(found) == 0 {
					return scopeCol{}, pgerr.New(pgerr.UndefinedColumn, "column \"%s\" specified in USING clause does not exist in %s table", name, side)
				}
				if len(found) > 1 {
					return scopeCol{}, pgerr.New(pgerr.AmbiguousColumn, "common column name \"%s\" appears more than once in %s table", name, side)
				}
				return found[0], nil
			}
			lc, err := find(lcols, "left")
			if err != nil {
				return nil, nil, err
			}
			rc, err := find(rcols, "right")
			if err != nil {
				return nil, nil, err
			}
			eq, err := b.makeCompare("=", b.colRef(lc.ID), b.colRef(rc.ID))
			if err != nil {
				return nil, nil, err
			}
			conds = append(conds, eq)
			// The merged column is visible once; the inputs stay reachable
			// by qualified name.
			for i := range cols {
				if cols[i].ID == lc.ID || cols[i].ID == rc.ID {
					cols[i].Hidden = true
				}
			}
			var merged scopeCol
			switch kind {
			case JoinFull:
				if proj == nil {
					proj = &Project{Input: join}
					for _, c := range join.Cols() {
						proj.Exprs = append(proj.Exprs, b.colRef(c))
						proj.ColIDs = append(proj.ColIDs, c)
					}
				}
				ct, _ := expr.CommonType(b.cols[lc.ID].T, b.cols[rc.ID].T)
				l, _ := b.coerce(b.colRef(lc.ID), ct, coerceImplicit)
				r, _ := b.coerce(b.colRef(rc.ID), ct, coerceImplicit)
				id := b.newCol(ColInfo{Name: name, T: ct})
				proj.Exprs = append(proj.Exprs, &expr.Coalesce{Args: []expr.Expr{l, r}, T: ct})
				proj.ColIDs = append(proj.ColIDs, id)
				merged = scopeCol{ID: id, Name: name}
			case JoinLeft:
				if j.Type == sql.JoinRight {
					merged = scopeCol{ID: lc.ID, Name: name}
				} else {
					merged = scopeCol{ID: lc.ID, Name: name}
				}
			default:
				merged = scopeCol{ID: lc.ID, Name: name}
			}
			cols = append([]scopeCol{merged}, cols...)
		}
		// Put the merged columns first in USING order.
		n := len(using)
		merged := cols[:n]
		for i, k := 0, n-1; i < k; i, k = i+1, k-1 {
			merged[i], merged[k] = merged[k], merged[i]
		}
		join.Cond = expr.MakeAnd(conds)
		if proj != nil {
			node = proj
		}
		return node, cols, nil
	}
	if j.On != nil {
		tmp.aggForbidden = "JOIN conditions"
		cond, err := b.bindExpr(j.On, tmp)
		if err != nil {
			return nil, nil, err
		}
		if cond, err = b.coerceBool(cond, "JOIN/ON"); err != nil {
			return nil, nil, err
		}
		join.Cond = cond
	}
	return node, cols, nil
}

// ---- names ----

func (b *Binder) resolveColumn(lvl *level, parts []string, pos int) (expr.Expr, error) {
	var table, name string
	switch len(parts) {
	case 1:
		name = parts[0]
	case 2:
		table, name = parts[0], parts[1]
	default:
		table, name = parts[len(parts)-2], parts[len(parts)-1]
	}
	tableSeen := false
	for l, depth := lvl, 0; l != nil; l, depth = l.parent, depth+1 {
		var found *scopeCol
		for i := range l.cols {
			c := &l.cols[i]
			if table != "" {
				if c.Table != table {
					continue
				}
				tableSeen = true
				if c.Name != name {
					continue
				}
			} else if c.Name != name || c.Hidden {
				continue
			}
			if found != nil && found.ID != c.ID {
				return nil, errorAt(pgerr.AmbiguousColumn, pos, "column reference \"%s\" is ambiguous", name)
			}
			found = c
		}
		if found == nil {
			continue
		}
		if depth > 0 {
			for m := lvl; m != l; m = m.parent {
				if m.outer != nil {
					m.outer[found.ID] = true
				}
			}
		}
		return b.colRef(found.ID), nil
	}
	if table != "" && !tableSeen {
		return nil, errorAt(pgerr.UndefinedTable, pos, "missing FROM-clause entry for table \"%s\"", table)
	}
	if table != "" {
		return nil, errorAt(pgerr.UndefinedColumn, pos, "column %s.%s does not exist", table, name)
	}
	return nil, errorAt(pgerr.UndefinedColumn, pos, "column \"%s\" does not exist", name)
}

// sortedOuter returns the outer references of a level in a stable order.
func sortedOuter(m map[expr.ColumnID]bool) []expr.ColumnID {
	out := make([]expr.ColumnID, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
