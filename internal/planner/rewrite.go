package planner

import (
	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/types"
)

// Rewriter applies logical rewrite rules: constant folding and predicate
// pushdown.
type Rewriter struct {
	// Ctx evaluates constant expressions during folding.
	Ctx *expr.Ctx
}

// ---- constant folding ----

// Fold simplifies an expression whose parts are constant.
func (r *Rewriter) Fold(e expr.Expr) expr.Expr {
	if e == nil {
		return nil
	}
	return expr.Map(e, r.foldNode)
}

func isConst(e expr.Expr) bool { _, ok := e.(*expr.Const); return ok }

func constBool(e expr.Expr) (val bool, isNull bool, ok bool) {
	c, isC := e.(*expr.Const)
	if !isC || (c.T.Oid != types.OidBool && !c.V.IsNull()) {
		return false, false, false
	}
	if c.V.IsNull() {
		return false, true, true
	}
	return c.V.Bool(), false, true
}

func (r *Rewriter) eval(e expr.Expr) (expr.Expr, bool) {
	if r.Ctx == nil {
		return e, false
	}
	v, err := expr.Eval(r.Ctx, e)
	if err != nil {
		return e, false // leave it: the error surfaces only if evaluated
	}
	return &expr.Const{V: v, T: e.Type()}, true
}

func (r *Rewriter) foldNode(e expr.Expr) expr.Expr {
	switch x := e.(type) {
	case *expr.Call:
		if x.Fn.Volatile {
			return e
		}
		for _, a := range x.Args {
			if !isConst(a) {
				return e
			}
		}
		if f, ok := r.eval(e); ok {
			return f
		}
	case *expr.Cast:
		if c, ok := x.Arg.(*expr.Const); ok {
			if needsRuntimeCast(x.T) || needsRuntimeCast(c.T) {
				return e
			}
			v, err := types.Cast(c.V, c.T, x.T, x.Explicit)
			if err == nil {
				return &expr.Const{V: v, T: x.T}
			}
		}
	case *expr.And:
		var args []expr.Expr
		sawNull := false
		for _, a := range x.Args {
			if v, null, ok := constBool(a); ok {
				if null {
					sawNull = true
					continue
				}
				if !v {
					return &expr.Const{V: types.False, T: types.Bool}
				}
				continue
			}
			args = append(args, a)
		}
		if sawNull {
			args = append(args, &expr.Const{V: types.Null, T: types.Bool})
		}
		switch len(args) {
		case 0:
			return &expr.Const{V: types.True, T: types.Bool}
		case 1:
			return args[0]
		}
		return &expr.And{Args: args}
	case *expr.Or:
		var args []expr.Expr
		sawNull := false
		for _, a := range x.Args {
			if v, null, ok := constBool(a); ok {
				if null {
					sawNull = true
					continue
				}
				if v {
					return &expr.Const{V: types.True, T: types.Bool}
				}
				continue
			}
			args = append(args, a)
		}
		if sawNull {
			args = append(args, &expr.Const{V: types.Null, T: types.Bool})
		}
		switch len(args) {
		case 0:
			return &expr.Const{V: types.False, T: types.Bool}
		case 1:
			return args[0]
		}
		return &expr.Or{Args: args}
	case *expr.Not, *expr.IsNull, *expr.IsBool, *expr.Distinct, *expr.NullIf, *expr.MinMax:
		for _, c := range childrenOf(e) {
			if !isConst(c) {
				return e
			}
		}
		if f, ok := r.eval(e); ok {
			return f
		}
	case *expr.Coalesce:
		var args []expr.Expr
		for _, a := range x.Args {
			if c, ok := a.(*expr.Const); ok {
				if c.V.IsNull() {
					continue
				}
				if len(args) == 0 {
					return &expr.Const{V: c.V, T: x.T}
				}
			}
			args = append(args, a)
		}
		if len(args) == 0 {
			return expr.NullConst(x.T)
		}
		return &expr.Coalesce{Args: args, T: x.T}
	case *expr.Case:
		var whens []expr.When
		for _, w := range x.Whens {
			if v, null, ok := constBool(w.Cond); ok {
				if !v || null {
					continue
				}
				if len(whens) == 0 {
					return w.Then
				}
				return &expr.Case{Whens: whens, Else: w.Then, T: x.T}
			}
			whens = append(whens, w)
		}
		if len(whens) == 0 {
			if x.Else == nil {
				return expr.NullConst(x.T)
			}
			return x.Else
		}
		return &expr.Case{Whens: whens, Else: x.Else, T: x.T}
	case *expr.InList:
		if !isConst(x.Arg) {
			return e
		}
		for _, a := range x.List {
			if !isConst(a) {
				return e
			}
		}
		if f, ok := r.eval(e); ok {
			return f
		}
	}
	return e
}

func childrenOf(e expr.Expr) []expr.Expr {
	var out []expr.Expr
	first := true
	expr.Walk(e, func(x expr.Expr) bool {
		if first {
			first = false
			return true
		}
		out = append(out, x)
		return false
	})
	return out
}

// foldPlan folds every expression of a logical plan.
func (r *Rewriter) foldPlan(n Node) Node {
	f := r.Fold
	fs := func(es []expr.Expr) []expr.Expr {
		out := make([]expr.Expr, len(es))
		for i, e := range es {
			out[i] = f(e)
		}
		return out
	}
	switch x := n.(type) {
	case *Scan:
		x.Filters = fs(x.Filters)
	case *VScan:
		x.Filters = fs(x.Filters)
	case *Values:
		for i := range x.Rows {
			x.Rows[i] = fs(x.Rows[i])
		}
	case *FuncScan:
		x.Args = fs(x.Args)
	case *Filter:
		x.Input = r.foldPlan(x.Input)
		x.Cond = f(x.Cond)
	case *Project:
		x.Input = r.foldPlan(x.Input)
		x.Exprs = fs(x.Exprs)
	case *Join:
		x.Left = r.foldPlan(x.Left)
		x.Right = r.foldPlan(x.Right)
		if x.Cond != nil {
			x.Cond = f(x.Cond)
		}
	case *Aggregate:
		x.Input = r.foldPlan(x.Input)
		x.GroupBy = fs(x.GroupBy)
		for _, a := range x.Aggs {
			a.Args = fs(a.Args)
			if a.Filter != nil {
				a.Filter = f(a.Filter)
			}
		}
	case *Sort:
		x.Input = r.foldPlan(x.Input)
		for i := range x.Keys {
			x.Keys[i].E = f(x.Keys[i].E)
		}
	case *Limit:
		x.Input = r.foldPlan(x.Input)
		if x.Limit != nil {
			x.Limit = f(x.Limit)
		}
		if x.Offset != nil {
			x.Offset = f(x.Offset)
		}
	case *Distinct:
		x.Input = r.foldPlan(x.Input)
		x.On = fs(x.On)
	case *SetOp:
		x.Left = r.foldPlan(x.Left)
		x.Right = r.foldPlan(x.Right)
	}
	return n
}

// ---- predicate pushdown ----

func colSet(ids []expr.ColumnID) map[expr.ColumnID]bool {
	m := make(map[expr.ColumnID]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// refersOnly reports whether every column of e that belongs to the region
// (all) is in side. Columns outside the region are outer references and
// are available everywhere.
func refersOnly(e expr.Expr, side, all map[expr.ColumnID]bool) bool {
	for id := range expr.Columns(e) {
		if all[id] && !side[id] {
			return false
		}
	}
	return true
}

func refersAny(e expr.Expr, side map[expr.ColumnID]bool) bool {
	for id := range expr.Columns(e) {
		if side[id] {
			return true
		}
	}
	return false
}

// pushdown moves filters as close to the scans as possible.
func (r *Rewriter) pushdown(n Node) Node {
	switch x := n.(type) {
	case *Filter:
		in := r.pushdown(x.Input)
		return r.pushInto(in, expr.Conjuncts(x.Cond))
	case *Project:
		x.Input = r.pushdown(x.Input)
	case *Join:
		x.Left = r.pushdown(x.Left)
		x.Right = r.pushdown(x.Right)
		if x.Kind == JoinInner && x.Cond != nil {
			conds := expr.Conjuncts(x.Cond)
			x.Cond = nil
			return r.pushInto(x, conds)
		}
		if x.Kind == JoinLeft && x.Cond != nil {
			// ON conditions that only involve the nullable side filter it.
			rc := colSet(x.Right.Cols())
			all := colSet(x.Cols())
			var keep, push []expr.Expr
			for _, c := range expr.Conjuncts(x.Cond) {
				if refersOnly(c, rc, all) && !expr.IsVolatile(c) && refersAny(c, rc) {
					push = append(push, c)
				} else {
					keep = append(keep, c)
				}
			}
			if len(push) > 0 {
				x.Right = r.pushInto(x.Right, push)
			}
			x.Cond = expr.MakeAnd(keep)
		}
	case *Aggregate:
		x.Input = r.pushdown(x.Input)
	case *Sort:
		x.Input = r.pushdown(x.Input)
	case *Limit:
		x.Input = r.pushdown(x.Input)
	case *Distinct:
		x.Input = r.pushdown(x.Input)
	case *SetOp:
		x.Left = r.pushdown(x.Left)
		x.Right = r.pushdown(x.Right)
	}
	return n
}

func wrapFilter(n Node, preds []expr.Expr) Node {
	if len(preds) == 0 {
		return n
	}
	return &Filter{Input: n, Cond: expr.MakeAnd(preds)}
}

// pushInto pushes conjuncts into n, returning the new subtree.
func (r *Rewriter) pushInto(n Node, preds []expr.Expr) Node {
	if len(preds) == 0 {
		return n
	}
	// A constant FALSE (or NULL) filter empties the input; keep it simple:
	// leave such filters where they are.
	switch x := n.(type) {
	case *Scan:
		x.Filters = append(x.Filters, preds...)
		return x
	case *VScan:
		x.Filters = append(x.Filters, preds...)
		return x
	case *Filter:
		return r.pushInto(x.Input, append(expr.Conjuncts(x.Cond), preds...))
	case *Project:
		// Substitute output columns by their definitions when those are
		// plain (no subqueries, not volatile).
		defs := map[expr.ColumnID]expr.Expr{}
		for i, id := range x.ColIDs {
			defs[id] = x.Exprs[i]
		}
		var down, keep []expr.Expr
		for _, p := range preds {
			ok := true
			for id := range expr.Columns(p) {
				if d, isOut := defs[id]; isOut && (expr.HasSubquery(d) || expr.IsVolatile(d)) {
					ok = false
				}
			}
			if !ok || expr.IsVolatile(p) {
				keep = append(keep, p)
				continue
			}
			down = append(down, expr.Map(p, func(e expr.Expr) expr.Expr {
				if c, isCol := e.(*expr.Col); isCol {
					if d, isOut := defs[c.ID]; isOut {
						return d
					}
				}
				return e
			}))
		}
		x.Input = r.pushInto(x.Input, down)
		return wrapFilter(x, keep)
	case *Join:
		lc, rc := colSet(x.Left.Cols()), colSet(x.Right.Cols())
		all := colSet(append(x.Left.Cols(), x.Right.Cols()...))
		var toL, toR, here, above []expr.Expr
		for _, p := range preds {
			onlyL := refersOnly(p, lc, all)
			onlyR := refersOnly(p, rc, all)
			volatile := expr.IsVolatile(p)
			switch x.Kind {
			case JoinInner:
				switch {
				case volatile:
					above = append(above, p)
				case onlyL && !onlyR:
					toL = append(toL, p)
				case onlyR && !onlyL:
					toR = append(toR, p)
				case onlyL && onlyR:
					// No region columns at all (constant or outer refs only).
					toL = append(toL, p)
				default:
					here = append(here, p)
				}
			case JoinLeft:
				switch {
				case onlyL && !volatile:
					toL = append(toL, p)
				case nullRejecting(p, rc):
					// A WHERE condition that fails on NULLs from the right
					// side turns the outer join into an inner join.
					x.Kind = JoinInner
					conds := expr.Conjuncts(x.Cond)
					x.Cond = nil
					return r.pushInto(x, append(conds, preds...))
				default:
					above = append(above, p)
				}
			case JoinSemi, JoinAnti:
				if onlyL && !volatile {
					toL = append(toL, p)
				} else {
					above = append(above, p)
				}
			default:
				above = append(above, p)
			}
		}
		if len(toL) > 0 {
			x.Left = r.pushInto(x.Left, toL)
		}
		if len(toR) > 0 {
			x.Right = r.pushInto(x.Right, toR)
		}
		if len(here) > 0 {
			x.Cond = expr.MakeAnd(append(expr.Conjuncts(x.Cond), here...))
		}
		return wrapFilter(x, above)
	case *Aggregate:
		gc := colSet(x.GroupCols)
		all := colSet(x.Cols())
		var down, keep []expr.Expr
		for _, p := range preds {
			if refersOnly(p, gc, all) && !expr.IsVolatile(p) && !expr.HasSubquery(p) {
				down = append(down, expr.Map(p, func(e expr.Expr) expr.Expr {
					if c, ok := e.(*expr.Col); ok {
						for i, id := range x.GroupCols {
							if id == c.ID {
								return x.GroupBy[i]
							}
						}
					}
					return e
				}))
			} else {
				keep = append(keep, p)
			}
		}
		if len(x.GroupBy) == 0 {
			// Without GROUP BY the aggregate returns a row even for empty
			// input; filters cannot move below it.
			return wrapFilter(x, preds)
		}
		x.Input = r.pushInto(x.Input, down)
		return wrapFilter(x, keep)
	case *Sort:
		x.Input = r.pushInto(x.Input, preds)
		return x
	case *Distinct:
		if len(x.On) == 0 {
			x.Input = r.pushInto(x.Input, preds)
			return x
		}
	}
	return wrapFilter(n, preds)
}

// nullRejecting reports whether p is false or NULL whenever all columns of
// side are NULL (so it removes the NULL-extended rows of an outer join).
func nullRejecting(p expr.Expr, side map[expr.ColumnID]bool) bool {
	switch x := p.(type) {
	case *expr.Call:
		if x.Fn.NonStrict {
			return false
		}
		for _, a := range x.Args {
			if c, ok := a.(*expr.Col); ok && side[c.ID] {
				return true
			}
			if cast, ok := a.(*expr.Cast); ok {
				if c, ok := cast.Arg.(*expr.Col); ok && side[c.ID] {
					return true
				}
			}
		}
	case *expr.IsNull:
		if c, ok := x.Arg.(*expr.Col); ok && x.Not && side[c.ID] {
			return true
		}
	case *expr.And:
		for _, a := range x.Args {
			if nullRejecting(a, side) {
				return true
			}
		}
	case *expr.InList:
		if c, ok := x.Arg.(*expr.Col); ok && side[c.ID] {
			return true
		}
	}
	return false
}

// Rewrite applies all rules to a logical plan, including the plans of
// subqueries.
func (r *Rewriter) Rewrite(n Node) Node {
	n = r.foldPlan(n)
	n = r.pushdown(n)
	return n
}
