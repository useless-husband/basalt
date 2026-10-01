package planner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/useless-husband/basalt/internal/expr"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/types"
)

type coerceMode int

const (
	coerceImplicit coerceMode = iota
	coerceAssign
	coerceExplicit
)

// resolveTypeName converts a parsed type name to a type.
func resolveTypeName(tn *sql.TypeName) (types.T, error) {
	t, ok := types.ByName(tn.Name)
	if !ok {
		return types.Unknown, pgerr.New(pgerr.UndefinedObject, "type \"%s\" does not exist", tn.Name)
	}
	switch t.Oid {
	case types.OidVarchar, types.OidBpchar:
		if len(tn.Mods) == 1 {
			if tn.Mods[0] < 1 {
				return types.Unknown, pgerr.New(pgerr.InvalidParameterValue, "length for type %s must be at least 1", t.TypName())
			}
			if t.Oid == types.OidVarchar {
				t = types.VarcharN(tn.Mods[0])
			} else {
				t = types.BpcharN(tn.Mods[0])
			}
		}
	case types.OidNumeric:
		switch len(tn.Mods) {
		case 1:
			t = types.NumericPS(tn.Mods[0], 0)
		case 2:
			if tn.Mods[1] > tn.Mods[0] || tn.Mods[0] < 1 || tn.Mods[0] > 1000 {
				return types.Unknown, pgerr.New(pgerr.InvalidParameterValue, "NUMERIC precision %d must be between 1 and 1000", tn.Mods[0])
			}
			t = types.NumericPS(tn.Mods[0], tn.Mods[1])
		}
	case types.OidFloat8:
		if tn.Name == "float" && len(tn.Mods) == 1 && tn.Mods[0] <= 24 {
			t = types.Float4
		}
	}
	if tn.Array {
		at, ok := types.ArrayOf(t)
		if !ok {
			return types.Unknown, pgerr.Unsupported("arrays of type %s are not supported", t)
		}
		return at, nil
	}
	return t, nil
}

// ResolveTypeName is resolveTypeName for other packages.
func ResolveTypeName(tn *sql.TypeName) (types.T, error) { return resolveTypeName(tn) }

func typeMismatch(e expr.Expr, t types.T) error {
	return pgerr.New(pgerr.DatatypeMismatch, "expression is of type %s but %s was expected", e.Type(), t).
		WithHint("You will need to rewrite or cast the expression.")
}

// coerce converts e to type t, folding literals and inferring parameter
// types where possible.
func (b *Binder) coerce(e expr.Expr, t types.T, mode coerceMode) (expr.Expr, error) {
	from := e.Type()
	if from == t {
		return e, nil
	}
	switch x := e.(type) {
	case *expr.Const:
		if from.Oid == types.OidUnknown {
			if x.V.IsNull() {
				return expr.NullConst(t), nil
			}
			if needsRuntimeCast(t) {
				return &expr.Cast{Arg: &expr.Const{V: x.V, T: types.Text}, T: t, Explicit: true}, nil
			}
			v, err := types.Parse(x.V.S, t)
			if err != nil {
				return nil, err
			}
			if t.Kind() == types.KText {
				v, err = types.ApplyCharMod(v, t, mode == coerceExplicit)
				if err != nil {
					return nil, err
				}
			}
			return &expr.Const{V: v, T: t}, nil
		}
	case *expr.Param:
		if from.Oid == types.OidUnknown {
			if t.Oid != types.OidUnknown {
				b.Params[x.N-1] = types.T{Oid: t.Oid, Mod: -1}
			}
			p := &expr.Param{N: x.N, T: types.T{Oid: t.Oid, Mod: -1}}
			if t.Mod >= 0 {
				return &expr.Cast{Arg: p, T: t, Explicit: mode == coerceExplicit}, nil
			}
			return p, nil
		}
	}
	if from.Oid == t.Oid {
		// Same type, different modifier: enforce it.
		if t.Mod >= 0 {
			return &expr.Cast{Arg: e, T: t, Explicit: mode == coerceExplicit}, nil
		}
		return retype(e, t), nil
	}
	ok, implicit := types.CanCast(from, t)
	if !ok && (from.IsInteger() && t.IsInteger()) {
		ok, implicit = true, true
	}
	if !ok && needsRuntimeCast(t) && (from.IsString() || from.IsInteger()) {
		ok = true
	}
	if !ok && needsRuntimeCast(from) && (t.IsString() || t.IsInteger()) {
		ok = true
	}
	if !ok {
		return nil, pgerr.New(pgerr.CannotCoerce, "cannot cast type %s to %s", from, t)
	}
	if !implicit && mode == coerceImplicit {
		return nil, typeMismatch(e, t)
	}
	if !implicit && mode == coerceAssign {
		// Assignment casts: between numeric types, to string types.
		if !(expr.NumericRank(from) > 0 && expr.NumericRank(t) > 0) && !t.IsString() && !(from.Kind() == types.KTimestamp || from.Kind() == types.KTimestampTZ) {
			return nil, pgerr.New(pgerr.DatatypeMismatch, "column is of type %s but expression is of type %s", t, from).
				WithHint("You will need to rewrite or cast the expression.")
		}
	}
	return &expr.Cast{Arg: e, T: t, Explicit: mode == coerceExplicit}, nil
}

func needsRuntimeCast(t types.T) bool {
	switch t.Oid {
	case types.OidRegclass, types.OidRegtype, types.OidRegproc, types.OidRegnamespace, types.OidRegrole:
		return true
	}
	return false
}

func retype(e expr.Expr, t types.T) expr.Expr {
	switch x := e.(type) {
	case *expr.Const:
		return &expr.Const{V: x.V, T: t}
	}
	return &expr.Cast{Arg: e, T: t}
}

func (b *Binder) coerceBool(e expr.Expr, clause string) (expr.Expr, error) {
	if e.Type().Oid == types.OidBool {
		return e, nil
	}
	if e.Type().Oid == types.OidUnknown {
		return b.coerce(e, types.Bool, coerceImplicit)
	}
	return nil, pgerr.New(pgerr.DatatypeMismatch, "argument of %s must be type boolean, not type %s", clause, e.Type())
}

func (b *Binder) bindExpr(e sql.Expr, lvl *level) (expr.Expr, error) {
	switch x := e.(type) {
	case *sql.Literal:
		return bindLiteral(x)
	case *sql.ColumnRef:
		return b.resolveColumn(lvl, x.Parts, x.Pos)
	case *sql.ParamRef:
		for len(b.Params) < x.N {
			b.Params = append(b.Params, types.Unknown)
		}
		t := b.Params[x.N-1]
		if t.Oid == 0 {
			t = types.Unknown
			b.Params[x.N-1] = t
		}
		p := &expr.Param{N: x.N, T: t}
		if t.Oid == types.OidUnknown {
			b.unknownPar = append(b.unknownPar, p)
		}
		return p, nil
	case *sql.BinaryExpr:
		return b.bindBinary(x, lvl)
	case *sql.UnaryExpr:
		a, err := b.bindExpr(x.Expr, lvl)
		if err != nil {
			return nil, err
		}
		if x.Op == "NOT" {
			if a, err = b.coerceBool(a, "NOT"); err != nil {
				return nil, err
			}
			return &expr.Not{Arg: a}, nil
		}
		fn, ret, argT, err := expr.ResolveUnary(x.Op, a.Type())
		if err != nil {
			return nil, err
		}
		if a, err = b.coerce(a, argT, coerceImplicit); err != nil {
			return nil, err
		}
		return &expr.Call{Fn: fn, Args: []expr.Expr{a}, T: ret}, nil
	case *sql.FuncCall:
		return b.bindFunc(x, lvl)
	case *sql.CastExpr:
		a, err := b.bindExpr(x.Expr, lvl)
		if err != nil {
			return nil, err
		}
		t, err := resolveTypeName(x.Type)
		if err != nil {
			return nil, err
		}
		return b.coerce(a, t, coerceExplicit)
	case *sql.CaseExpr:
		return b.bindCase(x, lvl)
	case *sql.IsExpr:
		a, err := b.bindExpr(x.Expr, lvl)
		if err != nil {
			return nil, err
		}
		if x.What == sql.IsNull {
			if a.Type().Oid == types.OidUnknown {
				a, _ = b.coerce(a, types.Text, coerceImplicit)
			}
			return &expr.IsNull{Arg: a, Not: x.Not}, nil
		}
		if a, err = b.coerceBool(a, "IS"); err != nil {
			return nil, err
		}
		what := map[sql.IsKind]int{sql.IsTrue: expr.TestTrue, sql.IsFalse: expr.TestFalse, sql.IsUnknown: expr.TestUnknown}[x.What]
		return &expr.IsBool{Arg: a, What: what, Not: x.Not}, nil
	case *sql.IsDistinctExpr:
		l, r, err := b.bindPair(x.Left, x.Right, lvl)
		if err != nil {
			return nil, err
		}
		return &expr.Distinct{L: l, R: r, Not: x.Not}, nil
	case *sql.BetweenExpr:
		v, err := b.bindExpr(x.Expr, lvl)
		if err != nil {
			return nil, err
		}
		lo, err := b.bindExpr(x.Low, lvl)
		if err != nil {
			return nil, err
		}
		hi, err := b.bindExpr(x.High, lvl)
		if err != nil {
			return nil, err
		}
		between := func(lo, hi expr.Expr) (expr.Expr, error) {
			ge, err := b.makeCompare(">=", v, lo)
			if err != nil {
				return nil, err
			}
			le, err := b.makeCompare("<=", v, hi)
			if err != nil {
				return nil, err
			}
			return &expr.And{Args: []expr.Expr{ge, le}}, nil
		}
		res, err := between(lo, hi)
		if err != nil {
			return nil, err
		}
		if x.Symmetric {
			r2, err := between(hi, lo)
			if err != nil {
				return nil, err
			}
			res = &expr.Or{Args: []expr.Expr{res, r2}}
		}
		if x.Not {
			return &expr.Not{Arg: res}, nil
		}
		return res, nil
	case *sql.InExpr:
		if x.Subquery != nil {
			sq, err := b.bindAnySubquery(x.Expr, "=", false, x.Subquery, lvl)
			if err != nil {
				return nil, err
			}
			if x.Not {
				sq.Not = true
			}
			return sq, nil
		}
		return b.bindInList(x.Expr, x.List, x.Not, lvl)
	case *sql.ExistsExpr:
		bq, err := b.bindSelect(x.Query, lvl)
		if err != nil {
			return nil, err
		}
		return b.newSubquery(expr.SubExists, bq, types.Bool), nil
	case *sql.SubqueryExpr:
		bq, err := b.bindSelect(x.Query, lvl)
		if err != nil {
			return nil, err
		}
		if len(bq.outs) != 1 {
			return nil, errorAt(pgerr.SyntaxError, x.Pos, "subquery must return only one column")
		}
		t := b.cols[bq.outs[0].id].T
		if t.Oid == types.OidUnknown {
			t = types.Text
		}
		return b.newSubquery(expr.SubScalar, bq, t), nil
	case *sql.LikeExpr:
		v, err := b.bindExpr(x.Expr, lvl)
		if err != nil {
			return nil, err
		}
		pat, err := b.bindExpr(x.Pattern, lvl)
		if err != nil {
			return nil, err
		}
		op := "~~"
		if x.ILike {
			op = "~~*"
		}
		if x.Not {
			op = "!" + op
		}
		bo, err := expr.ResolveBinary(op, v.Type(), pat.Type())
		if err != nil {
			return nil, err
		}
		if v, err = b.coerce(v, types.Text, coerceExplicit); err != nil {
			return nil, err
		}
		if pat, err = b.coerce(pat, types.Text, coerceImplicit); err != nil {
			return nil, err
		}
		args := []expr.Expr{v, pat}
		if x.Escape != nil {
			esc, err := b.bindExpr(x.Escape, lvl)
			if err != nil {
				return nil, err
			}
			if esc, err = b.coerce(esc, types.Text, coerceImplicit); err != nil {
				return nil, err
			}
			args = append(args, esc)
		}
		return &expr.Call{Fn: bo.Fn, Args: args, T: types.Bool}, nil
	case *sql.AnyAllExpr:
		if x.Subquery != nil {
			sq, err := b.bindAnySubquery(x.Left, x.Op, x.All, x.Subquery, lvl)
			if err != nil {
				return nil, err
			}
			return sq, nil
		}
		return b.bindArrayOp(x, lvl)
	case *sql.ArrayExpr:
		if x.Subquery != nil {
			bq, err := b.bindSelect(x.Subquery, lvl)
			if err != nil {
				return nil, err
			}
			if len(bq.outs) != 1 {
				return nil, pgerr.New(pgerr.SyntaxError, "subquery must return only one column")
			}
			et := b.cols[bq.outs[0].id].T
			at, ok := types.ArrayOf(et)
			if !ok {
				return nil, pgerr.Unsupported("arrays of type %s are not supported", et)
			}
			return b.newSubquery(expr.SubArray, bq, at), nil
		}
		var elems []expr.Expr
		et := types.Unknown
		for _, el := range x.Elems {
			e, err := b.bindExpr(el, lvl)
			if err != nil {
				return nil, err
			}
			ct, ok := unify(et, e.Type())
			if !ok {
				return nil, pgerr.New(pgerr.DatatypeMismatch, "ARRAY types %s and %s cannot be matched", et, e.Type())
			}
			et = ct
			elems = append(elems, e)
		}
		if et.Oid == types.OidUnknown {
			et = types.Text
		}
		for i := range elems {
			c, err := b.coerce(elems[i], et, coerceImplicit)
			if err != nil {
				return nil, err
			}
			elems[i] = c
		}
		at, ok := types.ArrayOf(types.T{Oid: et.Oid, Mod: -1})
		if !ok {
			return nil, pgerr.Unsupported("arrays of type %s are not supported", et)
		}
		return &expr.ArrayCons{Elems: elems, T: at}, nil
	case *sql.SubscriptExpr:
		a, err := b.bindExpr(x.Expr, lvl)
		if err != nil {
			return nil, err
		}
		if !a.Type().IsArray() && a.Type().Oid != types.OidInt2Vector && a.Type().Oid != types.OidOidVector {
			return nil, pgerr.New(pgerr.DatatypeMismatch, "cannot subscript type %s because it is not an array", a.Type())
		}
		i, err := b.bindExpr(x.Index, lvl)
		if err != nil {
			return nil, err
		}
		if i, err = b.coerce(i, types.Int4, coerceAssign); err != nil {
			return nil, err
		}
		et := a.Type().Elem()
		return &expr.Subscript{Arr: a, Idx: i, T: et}, nil
	case *sql.RowExpr:
		return nil, pgerr.Unsupported("row expressions are only supported in comparisons")
	case *sql.DefaultExpr:
		return nil, pgerr.New(pgerr.SyntaxError, "DEFAULT is not allowed in this context")
	case *sql.StarExpr:
		return nil, pgerr.New(pgerr.SyntaxError, "* is not allowed in this context")
	}
	return nil, pgerr.Internal("unsupported expression %T", e)
}

func bindLiteral(x *sql.Literal) (expr.Expr, error) {
	switch x.Kind {
	case sql.LitNull:
		return &expr.Const{V: types.Null, T: types.Unknown}, nil
	case sql.LitBool:
		return &expr.Const{V: types.NewBool(x.Val == "true"), T: types.Bool}, nil
	case sql.LitInt:
		i, err := strconv.ParseInt(x.Val, 10, 64)
		if err != nil {
			return nil, err
		}
		if i >= -2147483648 && i <= 2147483647 {
			return &expr.Const{V: types.NewInt(i), T: types.Int4}, nil
		}
		return &expr.Const{V: types.NewInt(i), T: types.Int8}, nil
	case sql.LitNumeric:
		d, err := types.ParseDecimal(x.Val)
		if err != nil {
			return nil, err
		}
		return &expr.Const{V: types.NewNumeric(d), T: types.Numeric}, nil
	}
	return &expr.Const{V: types.NewText(x.Val), T: types.Unknown}, nil
}

// bindPair binds two expressions and coerces them to a common type.
func (b *Binder) bindPair(l, r sql.Expr, lvl *level) (expr.Expr, expr.Expr, error) {
	le, err := b.bindExpr(l, lvl)
	if err != nil {
		return nil, nil, err
	}
	re, err := b.bindExpr(r, lvl)
	if err != nil {
		return nil, nil, err
	}
	ct, ok := expr.CommonType(le.Type(), re.Type())
	if !ok {
		return nil, nil, pgerr.New(pgerr.UndefinedFunction, "operator does not exist: %s = %s", le.Type(), re.Type())
	}
	if le, err = b.coerce(le, ct, coerceImplicit); err != nil {
		return nil, nil, err
	}
	if re, err = b.coerce(re, ct, coerceImplicit); err != nil {
		return nil, nil, err
	}
	return le, re, nil
}

// makeCompare builds a comparison between bound expressions.
func (b *Binder) makeCompare(op string, l, r expr.Expr) (expr.Expr, error) {
	bo, err := expr.ResolveBinary(op, l.Type(), r.Type())
	if err != nil {
		return nil, err
	}
	if l, err = b.coerce(l, bo.Left, coerceImplicit); err != nil {
		return nil, err
	}
	if r, err = b.coerce(r, bo.Right, coerceImplicit); err != nil {
		return nil, err
	}
	return &expr.Call{Fn: bo.Fn, Args: []expr.Expr{l, r}, T: bo.Ret}, nil
}

func (b *Binder) bindBinary(x *sql.BinaryExpr, lvl *level) (expr.Expr, error) {
	if x.Op == "AND" || x.Op == "OR" {
		l, err := b.bindExpr(x.Left, lvl)
		if err != nil {
			return nil, err
		}
		r, err := b.bindExpr(x.Right, lvl)
		if err != nil {
			return nil, err
		}
		if l, err = b.coerceBool(l, x.Op); err != nil {
			return nil, err
		}
		if r, err = b.coerceBool(r, x.Op); err != nil {
			return nil, err
		}
		if x.Op == "AND" {
			return &expr.And{Args: append(flattenAnd(l), flattenAnd(r)...)}, nil
		}
		return &expr.Or{Args: append(flattenOr(l), flattenOr(r)...)}, nil
	}
	// Row comparisons.
	lr, lok := x.Left.(*sql.RowExpr)
	rr, rok := x.Right.(*sql.RowExpr)
	if lok || rok {
		if !lok || !rok || len(lr.Exprs) != len(rr.Exprs) || !expr.IsComparison(x.Op) {
			return nil, pgerr.Unsupported("unsupported row comparison")
		}
		return b.bindRowCompare(x.Op, lr.Exprs, rr.Exprs, lvl)
	}
	l, err := b.bindExpr(x.Left, lvl)
	if err != nil {
		return nil, err
	}
	r, err := b.bindExpr(x.Right, lvl)
	if err != nil {
		return nil, err
	}
	bo, err := expr.ResolveBinary(x.Op, l.Type(), r.Type())
	if err != nil {
		if pe, ok := err.(*pgerr.Error); ok {
			pe.Position = x.Pos + 1
		}
		return nil, err
	}
	lm, rm := coerceImplicit, coerceImplicit
	if x.Op == "||" && bo.Ret.Oid == types.OidText {
		// text || anynonarray: the other operand goes through its output
		// function, as PostgreSQL's anytextcat does.
		if l.Type().IsString() {
			rm = coerceExplicit
		}
		if r.Type().IsString() {
			lm = coerceExplicit
		}
	}
	if l, err = b.coerce(l, bo.Left, lm); err != nil {
		return nil, err
	}
	if r, err = b.coerce(r, bo.Right, rm); err != nil {
		return nil, err
	}
	return &expr.Call{Fn: bo.Fn, Args: []expr.Expr{l, r}, T: bo.Ret}, nil
}

func flattenAnd(e expr.Expr) []expr.Expr {
	if a, ok := e.(*expr.And); ok {
		return a.Args
	}
	return []expr.Expr{e}
}

func flattenOr(e expr.Expr) []expr.Expr {
	if a, ok := e.(*expr.Or); ok {
		return a.Args
	}
	return []expr.Expr{e}
}

func (b *Binder) bindRowCompare(op string, ls, rs []sql.Expr, lvl *level) (expr.Expr, error) {
	n := len(ls)
	le := make([]expr.Expr, n)
	re := make([]expr.Expr, n)
	for i := range ls {
		l, r, err := b.bindPair(ls[i], rs[i], lvl)
		if err != nil {
			return nil, err
		}
		le[i], re[i] = l, r
	}
	cmp := func(op string, i int) (expr.Expr, error) { return b.makeCompare(op, le[i], re[i]) }
	switch op {
	case "=", "<>":
		var terms []expr.Expr
		for i := 0; i < n; i++ {
			c, err := cmp("=", i)
			if err != nil {
				return nil, err
			}
			terms = append(terms, c)
		}
		var res expr.Expr = &expr.And{Args: terms}
		if op == "<>" {
			res = &expr.Not{Arg: res}
		}
		return res, nil
	}
	// Lexicographic: (a,b) < (c,d)  <=>  a < c OR (a = c AND b < d).
	strict := op == "<" || op == ">"
	base := op[:1]
	var build func(i int) (expr.Expr, error)
	build = func(i int) (expr.Expr, error) {
		last := i == n-1
		o := base
		if last && !strict {
			o = op
		}
		c, err := cmp(o, i)
		if err != nil {
			return nil, err
		}
		if last {
			return c, nil
		}
		eq, err := cmp("=", i)
		if err != nil {
			return nil, err
		}
		rest, err := build(i + 1)
		if err != nil {
			return nil, err
		}
		return &expr.Or{Args: []expr.Expr{c, &expr.And{Args: []expr.Expr{eq, rest}}}}, nil
	}
	return build(0)
}

func (b *Binder) bindInList(left sql.Expr, list []sql.Expr, not bool, lvl *level) (expr.Expr, error) {
	if lr, ok := left.(*sql.RowExpr); ok {
		// (a, b) IN ((1, 2), (3, 4)) becomes an OR of row comparisons.
		var terms []expr.Expr
		for _, item := range list {
			rr, ok := item.(*sql.RowExpr)
			if !ok || len(rr.Exprs) != len(lr.Exprs) {
				return nil, pgerr.Unsupported("unsupported row IN list")
			}
			c, err := b.bindRowCompare("=", lr.Exprs, rr.Exprs, lvl)
			if err != nil {
				return nil, err
			}
			terms = append(terms, c)
		}
		var res expr.Expr = &expr.Or{Args: terms}
		if not {
			res = &expr.Not{Arg: res}
		}
		return res, nil
	}
	v, err := b.bindExpr(left, lvl)
	if err != nil {
		return nil, err
	}
	t := v.Type()
	items := make([]expr.Expr, len(list))
	for i, it := range list {
		e, err := b.bindExpr(it, lvl)
		if err != nil {
			return nil, err
		}
		ct, ok := unify(t, e.Type())
		if !ok {
			return nil, pgerr.New(pgerr.UndefinedFunction, "operator does not exist: %s = %s", t, e.Type())
		}
		t = ct
		items[i] = e
	}
	if t.Oid == types.OidUnknown {
		t = types.Text
	}
	if v, err = b.coerce(v, t, coerceImplicit); err != nil {
		return nil, err
	}
	for i := range items {
		if items[i], err = b.coerce(items[i], t, coerceImplicit); err != nil {
			return nil, err
		}
	}
	return &expr.InList{Arg: v, List: items, Not: not}, nil
}

func (b *Binder) newSubquery(kind expr.SubKind, bq *boundQuery, t types.T) *expr.Subquery {
	b.nextSub++
	sq := &expr.Subquery{Kind: kind, Plan: bq.node, T: t, ID: b.nextSub, Outer: sortedOuter(bq.outer)}
	if len(sq.Outer) > 0 {
		sq.Label = fmt.Sprintf("SubPlan %d", sq.ID)
	} else {
		sq.Label = fmt.Sprintf("InitPlan %d", sq.ID)
	}
	return sq
}

// bindAnySubquery binds x op ANY/ALL (subquery) and x IN (subquery).
func (b *Binder) bindAnySubquery(left sql.Expr, op string, all bool, q *sql.SelectStmt, lvl *level) (*expr.Subquery, error) {
	if _, ok := left.(*sql.RowExpr); ok {
		return nil, pgerr.Unsupported("row comparisons with subqueries are not supported")
	}
	v, err := b.bindExpr(left, lvl)
	if err != nil {
		return nil, err
	}
	bq, err := b.bindSelect(q, lvl)
	if err != nil {
		return nil, err
	}
	if len(bq.outs) != 1 {
		return nil, pgerr.New(pgerr.SyntaxError, "subquery has too many columns")
	}
	st := b.cols[bq.outs[0].id].T
	bo, err := expr.ResolveBinary(op, v.Type(), st)
	if err != nil {
		return nil, err
	}
	if v, err = b.coerce(v, bo.Left, coerceImplicit); err != nil {
		return nil, err
	}
	if st != bo.Right {
		// Cast the subquery's column.
		p := &Project{Input: bq.node}
		e, err := b.coerce(b.colRef(bq.outs[0].id), bo.Right, coerceImplicit)
		if err != nil {
			return nil, err
		}
		id := b.newCol(ColInfo{Name: bq.outs[0].name, T: bo.Right})
		p.Exprs = []expr.Expr{e}
		p.ColIDs = []expr.ColumnID{id}
		bq = &boundQuery{node: p, outs: []outCol{{bq.outs[0].name, id}}, outer: bq.outer}
	}
	kind := expr.SubAny
	if all {
		kind = expr.SubAll
	}
	sq := b.newSubquery(kind, bq, types.Bool)
	sq.Arg = v
	sq.Cmp = bo.Fn
	return sq, nil
}

// bindArrayOp binds x op ANY/ALL (array).
func (b *Binder) bindArrayOp(x *sql.AnyAllExpr, lvl *level) (expr.Expr, error) {
	if ae, ok := x.Right.(*sql.ArrayExpr); ok && ae.Subquery == nil && x.Op == "=" && !x.All {
		return b.bindInList(x.Left, ae.Elems, false, lvl)
	}
	if ae, ok := x.Right.(*sql.ArrayExpr); ok && ae.Subquery == nil && x.Op == "<>" && x.All {
		return b.bindInList(x.Left, ae.Elems, true, lvl)
	}
	v, err := b.bindExpr(x.Left, lvl)
	if err != nil {
		return nil, err
	}
	arr, err := b.bindExpr(x.Right, lvl)
	if err != nil {
		return nil, err
	}
	at := arr.Type()
	if at.Oid == types.OidUnknown {
		vt := v.Type()
		if vt.Oid == types.OidUnknown {
			vt = types.Text
		}
		t, ok := types.ArrayOf(vt)
		if !ok {
			return nil, pgerr.Unsupported("arrays of type %s are not supported", vt)
		}
		if arr, err = b.coerce(arr, t, coerceImplicit); err != nil {
			return nil, err
		}
		at = t
	}
	if !at.IsArray() {
		return nil, pgerr.New(pgerr.DatatypeMismatch, "op ANY/ALL (array) requires array on right side")
	}
	bo, err := expr.ResolveBinary(x.Op, v.Type(), at.Elem())
	if err != nil {
		return nil, err
	}
	if v, err = b.coerce(v, bo.Left, coerceImplicit); err != nil {
		return nil, err
	}
	fn := scalarArrayOp(bo.Fn, x.All)
	return &expr.Call{Fn: fn, Args: []expr.Expr{v, arr}, T: types.Bool}, nil
}

func scalarArrayOp(cmp *expr.Func, all bool) *expr.Func {
	name := "ANY"
	if all {
		name = "ALL"
	}
	return &expr.Func{Name: cmp.Op + " " + name, NonStrict: true, Impl: func(c *expr.Ctx, a []types.Value) (types.Value, error) {
		if a[1].IsNull() {
			return types.Null, nil
		}
		vals := a[1].Arr().Vals
		if len(vals) == 0 {
			return types.NewBool(all), nil
		}
		if a[0].IsNull() {
			return types.Null, nil
		}
		sawNull := false
		for _, w := range vals {
			if w.IsNull() {
				sawNull = true
				continue
			}
			r, err := cmp.Impl(c, []types.Value{a[0], w})
			if err != nil {
				return types.Null, err
			}
			if all && !r.Bool() {
				return types.False, nil
			}
			if !all && r.Bool() {
				return types.True, nil
			}
		}
		if sawNull {
			return types.Null, nil
		}
		return types.NewBool(all), nil
	}}
}

func (b *Binder) bindCase(x *sql.CaseExpr, lvl *level) (expr.Expr, error) {
	var operand expr.Expr
	var err error
	if x.Operand != nil {
		if operand, err = b.bindExpr(x.Operand, lvl); err != nil {
			return nil, err
		}
	}
	c := &expr.Case{}
	var results []expr.Expr
	for _, w := range x.Whens {
		var cond expr.Expr
		if operand != nil {
			v, err := b.bindExpr(w.Cond, lvl)
			if err != nil {
				return nil, err
			}
			if cond, err = b.makeCompare("=", operand, v); err != nil {
				return nil, err
			}
		} else {
			if cond, err = b.bindExpr(w.Cond, lvl); err != nil {
				return nil, err
			}
			if cond, err = b.coerceBool(cond, "CASE/WHEN"); err != nil {
				return nil, err
			}
		}
		r, err := b.bindExpr(w.Result, lvl)
		if err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, expr.When{Cond: cond})
		results = append(results, r)
	}
	if x.Else != nil {
		e, err := b.bindExpr(x.Else, lvl)
		if err != nil {
			return nil, err
		}
		results = append(results, e)
	}
	t := types.Unknown
	for _, r := range results {
		ct, ok := unify(t, r.Type())
		if !ok {
			return nil, pgerr.New(pgerr.DatatypeMismatch, "CASE types %s and %s cannot be matched", t, r.Type())
		}
		t = ct
	}
	if t.Oid == types.OidUnknown {
		t = types.Text
	}
	for i := range results {
		if results[i], err = b.coerce(results[i], t, coerceImplicit); err != nil {
			return nil, err
		}
	}
	for i := range c.Whens {
		c.Whens[i].Then = results[i]
	}
	if x.Else != nil {
		c.Else = results[len(results)-1]
	}
	c.T = t
	return c, nil
}

func (b *Binder) bindFunc(x *sql.FuncCall, lvl *level) (expr.Expr, error) {
	name := x.Name
	if x.Schema != "" && x.Schema != "pg_catalog" && x.Schema != "public" {
		return nil, pgerr.New(pgerr.UndefinedFunction, "function %s.%s does not exist", x.Schema, name)
	}
	if def, ok := expr.LookupAgg(name); ok {
		return b.bindAgg(def, x, lvl)
	}
	if x.Distinct || x.Star || x.Filter != nil || len(x.OrderBy) > 0 {
		return nil, pgerr.New(pgerr.WrongObjectType, "%s is not an aggregate function", name)
	}
	if name == "__identity__" {
		return nil, pgerr.Internal("identity default must be resolved by DDL")
	}
	args := make([]expr.Expr, len(x.Args))
	for i, a := range x.Args {
		// nextval('seq'::regclass): use the name directly.
		if (name == "nextval" || name == "currval" || name == "setval") && i == 0 {
			if ce, ok := a.(*sql.CastExpr); ok {
				if lit, ok := ce.Expr.(*sql.Literal); ok && lit.Kind == sql.LitString {
					a = lit
				}
			}
		}
		e, err := b.bindExpr(a, lvl)
		if err != nil {
			return nil, err
		}
		args[i] = e
	}
	switch name {
	case "coalesce", "greatest", "least":
		if len(args) == 0 {
			return nil, pgerr.New(pgerr.SyntaxError, "%s requires at least one argument", name)
		}
		t := types.Unknown
		for _, a := range args {
			ct, ok := unify(t, a.Type())
			if !ok {
				return nil, pgerr.New(pgerr.DatatypeMismatch, "%s types %s and %s cannot be matched", strings.ToUpper(name), t, a.Type())
			}
			t = ct
		}
		if t.Oid == types.OidUnknown {
			t = types.Text
		}
		for i := range args {
			c, err := b.coerce(args[i], t, coerceImplicit)
			if err != nil {
				return nil, err
			}
			args[i] = c
		}
		if name == "coalesce" {
			return &expr.Coalesce{Args: args, T: t}, nil
		}
		return &expr.MinMax{Args: args, Greatest: name == "greatest", T: t}, nil
	case "nullif":
		if len(args) != 2 {
			return nil, pgerr.New(pgerr.UndefinedFunction, "function nullif requires two arguments")
		}
		ct, ok := expr.CommonType(args[0].Type(), args[1].Type())
		if !ok {
			return nil, pgerr.New(pgerr.UndefinedFunction, "operator does not exist: %s = %s", args[0].Type(), args[1].Type())
		}
		if ct.Oid == types.OidUnknown {
			ct = types.Text
		}
		a, err := b.coerce(args[0], ct, coerceImplicit)
		if err != nil {
			return nil, err
		}
		c, err := b.coerce(args[1], ct, coerceImplicit)
		if err != nil {
			return nil, err
		}
		return &expr.NullIf{A: a, B: c, T: ct}, nil
	case "pg_typeof":
		if len(args) != 1 {
			return nil, pgerr.New(pgerr.UndefinedFunction, "function pg_typeof requires one argument")
		}
		t := args[0].Type()
		return &expr.Const{V: types.NewInt(int64(t.Oid)), T: types.Regtype}, nil
	}
	argT := make([]types.T, len(args))
	for i, a := range args {
		argT[i] = a.Type()
	}
	fn, ret, casts, err := expr.LookupFunc(name, argT)
	if err != nil {
		if pe, ok := err.(*pgerr.Error); ok {
			pe.Position = x.Pos + 1
		}
		return nil, err
	}
	for i := range args {
		if i < len(casts) && casts[i].Oid != 0 {
			mode := coerceImplicit
			if casts[i].IsString() || expr.NumericRank(casts[i]) > 0 {
				mode = coerceAssign
			}
			c, err := b.coerce(args[i], casts[i], mode)
			if err != nil {
				return nil, err
			}
			args[i] = c
		}
	}
	return &expr.Call{Fn: fn, Args: args, T: ret}, nil
}

func (b *Binder) bindAgg(def *expr.AggDef, x *sql.FuncCall, lvl *level) (expr.Expr, error) {
	if lvl.agg == nil {
		where := lvl.aggForbidden
		if where == "" {
			where = "this context"
		}
		return nil, errorAt(pgerr.GroupingError, x.Pos, "aggregate functions are not allowed in %s", where)
	}
	agg := lvl.agg
	lvl.agg = nil
	defer func() { lvl.agg = agg }()
	call := &expr.AggCall{Def: def, Distinct: x.Distinct, Star: x.Star}
	if x.Star && def.Name != "count" {
		return nil, pgerr.New(pgerr.SyntaxError, "%s(*) is not supported", def.Name)
	}
	var argT []types.T
	for _, a := range x.Args {
		e, err := b.bindExpr(a, lvl)
		if err != nil {
			if pgerr.Code(err) == pgerr.GroupingError {
				return nil, errorAt(pgerr.GroupingError, x.Pos, "aggregate function calls cannot be nested")
			}
			return nil, err
		}
		call.Args = append(call.Args, e)
		argT = append(argT, e.Type())
	}
	ret, casts, err := def.Resolve(argT)
	if err != nil {
		return nil, err
	}
	for i := range call.Args {
		if i < len(casts) {
			c, err := b.coerce(call.Args[i], casts[i], coerceImplicit)
			if err != nil {
				return nil, err
			}
			call.Args[i] = c
		}
	}
	if x.Filter != nil {
		f, err := b.bindExpr(x.Filter, lvl)
		if err != nil {
			return nil, err
		}
		if call.Filter, err = b.coerceBool(f, "FILTER"); err != nil {
			return nil, err
		}
	}
	for _, o := range x.OrderBy {
		e, err := b.bindExpr(o.Expr, lvl)
		if err != nil {
			return nil, err
		}
		call.OrderBy = append(call.OrderBy, sortKey(e, o))
	}
	call.T = ret
	// Reuse an identical aggregate.
	for _, c := range agg.calls {
		if c.Def == call.Def && c.Distinct == call.Distinct && c.Star == call.Star && exprsEq(c.Args, call.Args) &&
			expr.Equal(c.Filter, call.Filter) && len(c.OrderBy) == 0 && len(call.OrderBy) == 0 {
			return &expr.Col{ID: c.ID, T: c.T, Name: c.String()}, nil
		}
	}
	call.ID = b.newCol(ColInfo{Name: def.Name, T: ret})
	agg.calls = append(agg.calls, call)
	return &expr.Col{ID: call.ID, T: ret, Name: call.String()}, nil
}

func exprsEq(a, b []expr.Expr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !expr.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// unify folds types for CASE, VALUES, COALESCE and the like: unknown
// (NULL or an untyped literal) adapts to the other type.
func unify(t, u types.T) (types.T, bool) {
	switch {
	case u.Oid == types.OidUnknown:
		return t, true
	case t.Oid == types.OidUnknown:
		return u, true
	}
	return expr.CommonType(t, u)
}
