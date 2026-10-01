package planner

import (
	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/types"
)

func (b *Binder) table(tn *sql.TableName) (*catalog.Table, error) {
	if tn.Schema != "" && tn.Schema != "public" {
		return nil, errorAt(pgerr.UndefinedTable, tn.Pos, "relation \"%s.%s\" does not exist", tn.Schema, tn.Name)
	}
	t := b.Cat.TableByName(tn.Name)
	if t == nil {
		return nil, errorAt(pgerr.UndefinedTable, tn.Pos, "relation \"%s\" does not exist", tn.Name)
	}
	return t, nil
}

// columnDefault binds a column's DEFAULT (NULL if none).
func (b *Binder) columnDefault(c *catalog.Column) (expr.Expr, error) {
	if c.Default == "" {
		return expr.NullConst(c.Type), nil
	}
	if d, ok := b.defaults[c]; ok {
		return d, nil
	}
	ast, err := sql.ParseExpr(c.Default)
	if err != nil {
		return nil, err
	}
	e, err := b.bindExpr(ast, newLevel(nil))
	if err != nil {
		return nil, err
	}
	if e, err = b.coerce(e, c.Type, coerceAssign); err != nil {
		return nil, err
	}
	b.defaults[c] = e
	return e, nil
}

// BindTableExpr binds an expression over a table's columns (CHECK
// constraints, defaults). The columns get fresh IDs, returned in order.
func (b *Binder) BindTableExpr(t *catalog.Table, e sql.Expr) (expr.Expr, []expr.ColumnID, error) {
	lvl := newLevel(nil)
	lvl.aggForbidden = "check constraints"
	_, cols := b.newScan(t, t.Name, nil)
	lvl.cols = cols
	x, err := b.bindExpr(e, lvl)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]expr.ColumnID, len(cols))
	for i, c := range cols {
		ids[i] = c.ID
	}
	b.finish()
	return x, ids, nil
}

// BindExprNoColumns binds a standalone expression (column defaults).
func (b *Binder) BindExprNoColumns(e sql.Expr, t types.T) (expr.Expr, error) {
	lvl := newLevel(nil)
	lvl.aggForbidden = "DEFAULT expressions"
	x, err := b.bindExpr(e, lvl)
	if err != nil {
		return nil, err
	}
	return b.coerce(x, t, coerceAssign)
}

func (b *Binder) bindReturning(targets []*sql.Target, lvl *level) ([]expr.Expr, []OutputCol, error) {
	var exprs []expr.Expr
	var outs []OutputCol
	for _, t := range targets {
		if t.Star {
			for _, c := range lvl.cols {
				if c.Hidden || (t.StarTable != "" && c.Table != t.StarTable) {
					continue
				}
				exprs = append(exprs, b.colRef(c.ID))
				ci := b.cols[c.ID]
				outs = append(outs, OutputCol{Name: c.Name, T: ci.T, TableOid: ci.TableOid, Attnum: ci.Attnum})
			}
			continue
		}
		e, err := b.bindExpr(t.Expr, lvl)
		if err != nil {
			return nil, nil, err
		}
		if e.Type().Oid == types.OidUnknown {
			if e, err = b.coerce(e, types.Text, coerceImplicit); err != nil {
				return nil, nil, err
			}
		}
		name := t.Alias
		if name == "" {
			name = exprName(t.Expr)
		}
		exprs = append(exprs, e)
		outs = append(outs, OutputCol{Name: name, T: e.Type()})
	}
	for i := range outs {
		outs[i].ID = expr.ColumnID(-1 - i)
	}
	return exprs, outs, nil
}

// BindInsert binds an INSERT.
func (b *Binder) BindInsert(s *sql.InsertStmt) (*Insert, error) {
	if err := b.pushCTEs(s.With); err != nil {
		return nil, err
	}
	defer b.popCTEs(s.With)
	t, err := b.table(s.Table)
	if err != nil {
		return nil, err
	}
	b.useTable(t.Oid, true)
	// Target columns.
	var targets []int
	if len(s.Columns) > 0 {
		seen := map[int]bool{}
		for _, name := range s.Columns {
			i := t.ColumnIndex(name)
			if i < 0 {
				return nil, pgerr.New(pgerr.UndefinedColumn, "column \"%s\" of relation \"%s\" does not exist", name, t.Name)
			}
			if seen[i] {
				return nil, pgerr.New(pgerr.DuplicateColumn, "column \"%s\" specified more than once", name)
			}
			seen[i] = true
			targets = append(targets, i)
		}
	} else {
		for i := range t.Columns {
			targets = append(targets, i)
		}
	}
	ins := &Insert{Table: t}
	var srcCols []expr.ColumnID
	switch {
	case s.DefaultValues:
		ins.Source = &Values{Rows: [][]expr.Expr{{}}}
		targets = nil
	case s.Source.Values != nil && s.Source.SetOp == sql.SetNone && s.Source.OrderBy == nil && s.Source.Limit == nil && s.Source.With == nil:
		// VALUES: coerce each cell to its column, allowing DEFAULT.
		v := &Values{}
		for _, row := range s.Source.Values {
			if len(row) > len(targets) {
				return nil, pgerr.New(pgerr.SyntaxError, "INSERT has more expressions than target columns")
			}
			if len(row) < len(targets) && len(s.Columns) > 0 {
				return nil, pgerr.New(pgerr.SyntaxError, "INSERT has more target columns than expressions")
			}
			var bound []expr.Expr
			for j, cell := range row {
				col := t.Columns[targets[j]]
				var e expr.Expr
				if _, ok := cell.(*sql.DefaultExpr); ok {
					if e, err = b.columnDefault(col); err != nil {
						return nil, err
					}
				} else {
					if e, err = b.bindExpr(cell, newLevel(nil)); err != nil {
						return nil, err
					}
					if e, err = b.coerce(e, col.Type, coerceAssign); err != nil {
						return nil, err
					}
				}
				bound = append(bound, e)
			}
			// Short rows (no column list) get defaults for the rest.
			for j := len(row); j < len(targets); j++ {
				d, err := b.columnDefault(t.Columns[targets[j]])
				if err != nil {
					return nil, err
				}
				bound = append(bound, d)
			}
			v.Rows = append(v.Rows, bound)
		}
		for _, ti := range targets {
			id := b.newCol(ColInfo{Name: t.Columns[ti].Name, T: t.Columns[ti].Type})
			v.ColIDs = append(v.ColIDs, id)
		}
		ins.Source = v
		srcCols = v.ColIDs
	default:
		bq, err := b.bindSelect(s.Source, nil)
		if err != nil {
			return nil, err
		}
		if len(bq.outs) > len(targets) {
			return nil, pgerr.New(pgerr.SyntaxError, "INSERT has more expressions than target columns")
		}
		if len(bq.outs) < len(targets) && len(s.Columns) > 0 {
			return nil, pgerr.New(pgerr.SyntaxError, "INSERT has more target columns than expressions")
		}
		targets = targets[:len(bq.outs)]
		p := &Project{Input: bq.node}
		for j, o := range bq.outs {
			col := t.Columns[targets[j]]
			e, err := b.coerce(b.colRef(o.id), col.Type, coerceAssign)
			if err != nil {
				return nil, err
			}
			id := b.newCol(ColInfo{Name: col.Name, T: col.Type})
			p.Exprs = append(p.Exprs, e)
			p.ColIDs = append(p.ColIDs, id)
		}
		ins.Source = p
		srcCols = p.ColIDs
	}
	ins.Exprs = make([]expr.Expr, len(t.Columns))
	for j, ti := range targets {
		ins.Exprs[ti] = b.colRef(srcCols[j])
	}
	for i, c := range t.Columns {
		if ins.Exprs[i] == nil {
			d, err := b.columnDefault(c)
			if err != nil {
				return nil, err
			}
			ins.Exprs[i] = d
		}
	}
	// RETURNING sees the inserted row.
	rl := newLevel(nil)
	alias := s.Table.Alias
	if alias == "" {
		alias = t.Name
	}
	_, rcols := b.newScan(t, alias, nil)
	rl.cols = rcols
	for _, c := range rcols {
		ins.ReturningCols = append(ins.ReturningCols, c.ID)
	}
	if oc := s.OnConflict; oc != nil {
		for _, name := range oc.Columns {
			i := t.ColumnIndex(name)
			if i < 0 {
				return nil, pgerr.New(pgerr.UndefinedColumn, "column \"%s\" does not exist", name)
			}
			ins.ConflictCols = append(ins.ConflictCols, i)
		}
		if oc.DoNothing {
			ins.OnConflictDoNothing = true
		} else {
			if len(oc.Columns) == 0 {
				return nil, pgerr.New(pgerr.SyntaxError, "ON CONFLICT DO UPDATE requires inference specification or constraint name").
					WithHint("For example, ON CONFLICT (column_name).")
			}
			ul := newLevel(nil)
			_, ex := b.newScan(t, alias, nil)
			_, exc := b.newScan(t, "excluded", nil)
			ul.cols = append(ex, exc...)
			for _, c := range ex {
				ins.ExistingCols = append(ins.ExistingCols, c.ID)
			}
			for _, c := range exc {
				ins.ExcludedCols = append(ins.ExcludedCols, c.ID)
			}
			ins.ConflictSet = map[int]expr.Expr{}
			for _, sc := range oc.Set {
				i := t.ColumnIndex(sc.Column)
				if i < 0 {
					return nil, pgerr.New(pgerr.UndefinedColumn, "column \"%s\" of relation \"%s\" does not exist", sc.Column, t.Name)
				}
				var e expr.Expr
				if _, ok := sc.Value.(*sql.DefaultExpr); ok {
					if e, err = b.columnDefault(t.Columns[i]); err != nil {
						return nil, err
					}
				} else {
					if e, err = b.bindExpr(sc.Value, ul); err != nil {
						return nil, err
					}
					if e, err = b.coerce(e, t.Columns[i].Type, coerceAssign); err != nil {
						return nil, err
					}
				}
				ins.ConflictSet[i] = e
			}
			if oc.Where != nil {
				w, err := b.bindExpr(oc.Where, ul)
				if err != nil {
					return nil, err
				}
				if ins.ConflictWhere, err = b.coerceBool(w, "WHERE"); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(s.Returning) > 0 {
		if ins.Returning, ins.Output, err = b.bindReturning(s.Returning, rl); err != nil {
			return nil, err
		}
	}
	b.finish()
	return ins, nil
}

// BindUpdate binds an UPDATE.
func (b *Binder) BindUpdate(s *sql.UpdateStmt) (*Update, error) {
	if err := b.pushCTEs(s.With); err != nil {
		return nil, err
	}
	defer b.popCTEs(s.With)
	t, err := b.table(s.Table)
	if err != nil {
		return nil, err
	}
	b.useTable(t.Oid, true)
	alias := s.Table.Alias
	if alias == "" {
		alias = t.Name
	}
	lvl := newLevel(nil)
	scan, cols := b.newScan(t, alias, nil)
	scan.TIDCol = b.newCol(ColInfo{Name: "ctid", T: types.Int8, Table: alias})
	scan.Lock = true
	lvl.cols = cols
	var node Node = scan
	if len(s.From) > 0 {
		from, err := b.bindFrom(s.From, lvl)
		if err != nil {
			return nil, err
		}
		node = &Join{Kind: JoinInner, Left: node, Right: from}
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
		node = &Filter{Input: node, Cond: w}
	}
	up := &Update{Table: t, Scan: scan, Set: map[int]expr.Expr{}}
	lvl.aggForbidden = "UPDATE"
	for _, sc := range s.Set {
		i := t.ColumnIndex(sc.Column)
		if i < 0 {
			return nil, pgerr.New(pgerr.UndefinedColumn, "column \"%s\" of relation \"%s\" does not exist", sc.Column, t.Name)
		}
		if _, dup := up.Set[i]; dup {
			return nil, pgerr.New(pgerr.SyntaxError, "multiple assignments to same column \"%s\"", sc.Column)
		}
		var e expr.Expr
		if _, ok := sc.Value.(*sql.DefaultExpr); ok {
			if e, err = b.columnDefault(t.Columns[i]); err != nil {
				return nil, err
			}
		} else {
			if e, err = b.bindExpr(sc.Value, lvl); err != nil {
				return nil, err
			}
			if e, err = b.coerce(e, t.Columns[i].Type, coerceAssign); err != nil {
				return nil, err
			}
		}
		up.Set[i] = e
	}
	up.Source = node
	if len(s.Returning) > 0 {
		// RETURNING sees the new row and the FROM tables.
		rl := newLevel(nil)
		_, ncols := b.newScan(t, alias, nil)
		for _, c := range ncols {
			up.NewCols = append(up.NewCols, c.ID)
		}
		rl.cols = append(ncols, lvl.cols[len(cols):]...)
		if up.Returning, up.Output, err = b.bindReturning(s.Returning, rl); err != nil {
			return nil, err
		}
	}
	b.finish()
	return up, nil
}

// BindDelete binds a DELETE.
func (b *Binder) BindDelete(s *sql.DeleteStmt) (*Delete, error) {
	if err := b.pushCTEs(s.With); err != nil {
		return nil, err
	}
	defer b.popCTEs(s.With)
	t, err := b.table(s.Table)
	if err != nil {
		return nil, err
	}
	b.useTable(t.Oid, true)
	alias := s.Table.Alias
	if alias == "" {
		alias = t.Name
	}
	lvl := newLevel(nil)
	scan, cols := b.newScan(t, alias, nil)
	scan.TIDCol = b.newCol(ColInfo{Name: "ctid", T: types.Int8, Table: alias})
	scan.Lock = true
	lvl.cols = cols
	var node Node = scan
	if len(s.Using) > 0 {
		from, err := b.bindFrom(s.Using, lvl)
		if err != nil {
			return nil, err
		}
		node = &Join{Kind: JoinInner, Left: node, Right: from}
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
		node = &Filter{Input: node, Cond: w}
	}
	del := &Delete{Table: t, Source: node, Scan: scan}
	if len(s.Returning) > 0 {
		if del.Returning, del.Output, err = b.bindReturning(s.Returning, lvl); err != nil {
			return nil, err
		}
	}
	b.finish()
	return del, nil
}
