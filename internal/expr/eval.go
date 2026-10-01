package expr

import (
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/types"
)

// SubqueryRunner executes subquery plans; the executor implements it.
type SubqueryRunner interface {
	// RunSubquery runs sq.Plan with the given outer column values and
	// returns its rows; limit > 0 allows stopping after that many rows.
	RunSubquery(c *Ctx, sq *Subquery, outer map[ColumnID]types.Value, limit int) ([][]types.Value, error)
}

// Ctx is the evaluation context of one statement execution.
type Ctx struct {
	Params []types.Value
	Outer  map[ColumnID]types.Value
	Env    Env
	Sub    SubqueryRunner
	Check  func() error
	cache  map[int]any
}

// Layout maps the columns of an operator's input row to positions.
type Layout map[ColumnID]int

// Fn is a compiled expression.
type Fn func(c *Ctx, row []types.Value) (types.Value, error)

// WithOuter returns a copy of c whose outer bindings are extended.
func (c *Ctx) WithOuter(outer map[ColumnID]types.Value) *Ctx {
	n := *c
	n.Outer = outer
	return &n
}

// ResetCache clears cached uncorrelated subquery results (new execution).
func (c *Ctx) ResetCache() { c.cache = nil }

func (c *Ctx) cached(id int) (any, bool) {
	v, ok := c.cache[id]
	return v, ok
}

func (c *Ctx) setCached(id int, v any) {
	if c.cache == nil {
		c.cache = map[int]any{}
	}
	c.cache[id] = v
}

// Truth reports whether a boolean value is TRUE (not FALSE, not NULL).
func Truth(v types.Value) bool { return v.K == types.KBool && v.I != 0 }

// Compile turns an expression into a closure over rows with layout l.
func Compile(e Expr, l Layout) (Fn, error) {
	switch x := e.(type) {
	case *Col:
		if pos, ok := l[x.ID]; ok {
			return func(_ *Ctx, row []types.Value) (types.Value, error) { return row[pos], nil }, nil
		}
		id := x.ID
		name := x.Name
		return func(c *Ctx, _ []types.Value) (types.Value, error) {
			v, ok := c.Outer[id]
			if !ok {
				return types.Null, pgerr.Internal("column %s (#%d) is not available here", name, id)
			}
			return v, nil
		}, nil
	case *Const:
		v := x.V
		return func(*Ctx, []types.Value) (types.Value, error) { return v, nil }, nil
	case *Param:
		n := x.N - 1
		return func(c *Ctx, _ []types.Value) (types.Value, error) {
			if n >= len(c.Params) {
				return types.Null, pgerr.New(pgerr.UndefinedParameter, "there is no parameter $%d", n+1)
			}
			return c.Params[n], nil
		}, nil
	case *Call:
		args, err := compileAll(x.Args, l)
		if err != nil {
			return nil, err
		}
		impl, strict := x.Fn.Impl, !x.Fn.NonStrict
		switch len(args) {
		case 2:
			a0, a1 := args[0], args[1]
			return func(c *Ctx, row []types.Value) (types.Value, error) {
				v0, err := a0(c, row)
				if err != nil {
					return types.Null, err
				}
				if strict && v0.IsNull() {
					return types.Null, nil
				}
				v1, err := a1(c, row)
				if err != nil {
					return types.Null, err
				}
				if strict && v1.IsNull() {
					return types.Null, nil
				}
				return impl(c, []types.Value{v0, v1})
			}, nil
		}
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			vals := make([]types.Value, len(args))
			for i, a := range args {
				v, err := a(c, row)
				if err != nil {
					return types.Null, err
				}
				if strict && v.IsNull() {
					return types.Null, nil
				}
				vals[i] = v
			}
			return impl(c, vals)
		}, nil
	case *And:
		args, err := compileAll(x.Args, l)
		if err != nil {
			return nil, err
		}
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			sawNull := false
			for _, a := range args {
				v, err := a(c, row)
				if err != nil {
					return types.Null, err
				}
				if v.IsNull() {
					sawNull = true
				} else if !v.Bool() {
					return types.False, nil
				}
			}
			if sawNull {
				return types.Null, nil
			}
			return types.True, nil
		}, nil
	case *Or:
		args, err := compileAll(x.Args, l)
		if err != nil {
			return nil, err
		}
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			sawNull := false
			for _, a := range args {
				v, err := a(c, row)
				if err != nil {
					return types.Null, err
				}
				if v.IsNull() {
					sawNull = true
				} else if v.Bool() {
					return types.True, nil
				}
			}
			if sawNull {
				return types.Null, nil
			}
			return types.False, nil
		}, nil
	case *Not:
		a, err := Compile(x.Arg, l)
		if err != nil {
			return nil, err
		}
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			v, err := a(c, row)
			if err != nil || v.IsNull() {
				return types.Null, err
			}
			return types.NewBool(!v.Bool()), nil
		}, nil
	case *Case:
		type arm struct{ cond, then Fn }
		arms := make([]arm, len(x.Whens))
		for i, w := range x.Whens {
			c, err := Compile(w.Cond, l)
			if err != nil {
				return nil, err
			}
			t, err := Compile(w.Then, l)
			if err != nil {
				return nil, err
			}
			arms[i] = arm{c, t}
		}
		var els Fn
		if x.Else != nil {
			var err error
			if els, err = Compile(x.Else, l); err != nil {
				return nil, err
			}
		}
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			for _, a := range arms {
				v, err := a.cond(c, row)
				if err != nil {
					return types.Null, err
				}
				if Truth(v) {
					return a.then(c, row)
				}
			}
			if els != nil {
				return els(c, row)
			}
			return types.Null, nil
		}, nil
	case *Cast:
		a, err := Compile(x.Arg, l)
		if err != nil {
			return nil, err
		}
		from, to, explicit := x.Arg.Type(), x.T, x.Explicit
		if needsCatalogCast(from, to) {
			return func(c *Ctx, row []types.Value) (types.Value, error) {
				v, err := a(c, row)
				if err != nil || v.IsNull() {
					return types.Null, err
				}
				return c.Env.CatalogFunc("cast", []types.Value{v, types.NewInt(int64(from.Oid)), types.NewInt(int64(to.Oid))})
			}, nil
		}
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			v, err := a(c, row)
			if err != nil {
				return types.Null, err
			}
			return types.Cast(v, from, to, explicit)
		}, nil
	case *IsNull:
		a, err := Compile(x.Arg, l)
		if err != nil {
			return nil, err
		}
		not := x.Not
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			v, err := a(c, row)
			if err != nil {
				return types.Null, err
			}
			return types.NewBool(v.IsNull() != not), nil
		}, nil
	case *IsBool:
		a, err := Compile(x.Arg, l)
		if err != nil {
			return nil, err
		}
		what, not := x.What, x.Not
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			v, err := a(c, row)
			if err != nil {
				return types.Null, err
			}
			var r bool
			switch what {
			case TestTrue:
				r = !v.IsNull() && v.Bool()
			case TestFalse:
				r = !v.IsNull() && !v.Bool()
			default:
				r = v.IsNull()
			}
			return types.NewBool(r != not), nil
		}, nil
	case *Distinct:
		a, err := Compile(x.L, l)
		if err != nil {
			return nil, err
		}
		b, err := Compile(x.R, l)
		if err != nil {
			return nil, err
		}
		not := x.Not
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			va, err := a(c, row)
			if err != nil {
				return types.Null, err
			}
			vb, err := b(c, row)
			if err != nil {
				return types.Null, err
			}
			return types.NewBool(types.Equal(va, vb) == not), nil
		}, nil
	case *InList:
		return compileInList(x, l)
	case *Coalesce:
		args, err := compileAll(x.Args, l)
		if err != nil {
			return nil, err
		}
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			for _, a := range args {
				v, err := a(c, row)
				if err != nil {
					return types.Null, err
				}
				if !v.IsNull() {
					return v, nil
				}
			}
			return types.Null, nil
		}, nil
	case *NullIf:
		a, err := Compile(x.A, l)
		if err != nil {
			return nil, err
		}
		b, err := Compile(x.B, l)
		if err != nil {
			return nil, err
		}
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			va, err := a(c, row)
			if err != nil {
				return types.Null, err
			}
			vb, err := b(c, row)
			if err != nil {
				return types.Null, err
			}
			if !va.IsNull() && !vb.IsNull() && types.Compare(va, vb) == 0 {
				return types.Null, nil
			}
			return va, nil
		}, nil
	case *MinMax:
		args, err := compileAll(x.Args, l)
		if err != nil {
			return nil, err
		}
		greatest := x.Greatest
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			var best types.Value
			for _, a := range args {
				v, err := a(c, row)
				if err != nil {
					return types.Null, err
				}
				if v.IsNull() {
					continue
				}
				if best.IsNull() {
					best = v
					continue
				}
				cmp := types.Compare(v, best)
				if (greatest && cmp > 0) || (!greatest && cmp < 0) {
					best = v
				}
			}
			return best, nil
		}, nil
	case *Subquery:
		return compileSubquery(x, l)
	case *ArrayCons:
		elems, err := compileAll(x.Elems, l)
		if err != nil {
			return nil, err
		}
		et := x.T.Elem()
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			vals := make([]types.Value, len(elems))
			for i, el := range elems {
				v, err := el(c, row)
				if err != nil {
					return types.Null, err
				}
				vals[i] = v
			}
			return types.NewArray(et, vals), nil
		}, nil
	case *Subscript:
		a, err := Compile(x.Arr, l)
		if err != nil {
			return nil, err
		}
		i, err := Compile(x.Idx, l)
		if err != nil {
			return nil, err
		}
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			av, err := a(c, row)
			if err != nil || av.IsNull() {
				return types.Null, err
			}
			iv, err := i(c, row)
			if err != nil || iv.IsNull() {
				return types.Null, err
			}
			vals := av.Arr().Vals
			if iv.I < 1 || iv.I > int64(len(vals)) {
				return types.Null, nil
			}
			return vals[iv.I-1], nil
		}, nil
	}
	return nil, pgerr.Internal("cannot compile expression %T", e)
}

func needsCatalogCast(from, to types.T) bool {
	isReg := func(o uint32) bool {
		return o == types.OidRegclass || o == types.OidRegtype || o == types.OidRegproc || o == types.OidRegnamespace || o == types.OidRegrole
	}
	if isReg(to.Oid) && from.IsString() {
		return true
	}
	if isReg(from.Oid) && to.IsString() {
		return true
	}
	return false
}

func compileAll(es []Expr, l Layout) ([]Fn, error) {
	out := make([]Fn, len(es))
	for i, e := range es {
		f, err := Compile(e, l)
		if err != nil {
			return nil, err
		}
		out[i] = f
	}
	return out, nil
}

func compileInList(x *InList, l Layout) (Fn, error) {
	arg, err := Compile(x.Arg, l)
	if err != nil {
		return nil, err
	}
	not := x.Not
	// All-constant lists use a hash set.
	allConst := true
	for _, e := range x.List {
		if _, ok := e.(*Const); !ok {
			allConst = false
			break
		}
	}
	if allConst && len(x.List) > 4 {
		set := map[string]bool{}
		hasNull := false
		for _, e := range x.List {
			v := e.(*Const).V
			if v.IsNull() {
				hasNull = true
				continue
			}
			set[string(types.EncodeKey(nil, v))] = true
		}
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			v, err := arg(c, row)
			if err != nil || v.IsNull() {
				return types.Null, err
			}
			if set[string(types.EncodeKey(nil, v))] {
				return types.NewBool(!not), nil
			}
			if hasNull {
				return types.Null, nil
			}
			return types.NewBool(not), nil
		}, nil
	}
	list, err := compileAll(x.List, l)
	if err != nil {
		return nil, err
	}
	return func(c *Ctx, row []types.Value) (types.Value, error) {
		v, err := arg(c, row)
		if err != nil || v.IsNull() {
			return types.Null, err
		}
		sawNull := false
		for _, e := range list {
			w, err := e(c, row)
			if err != nil {
				return types.Null, err
			}
			if w.IsNull() {
				sawNull = true
				continue
			}
			if types.Compare(v, w) == 0 {
				return types.NewBool(!not), nil
			}
		}
		if sawNull {
			return types.Null, nil
		}
		return types.NewBool(not), nil
	}, nil
}

func compileSubquery(sq *Subquery, l Layout) (Fn, error) {
	var arg Fn
	if sq.Arg != nil {
		var err error
		if arg, err = Compile(sq.Arg, l); err != nil {
			return nil, err
		}
	}
	// Collect the outer values the subquery needs from this row or from
	// our own outer bindings.
	type outerRef struct {
		id  ColumnID
		pos int // -1: take from c.Outer
	}
	refs := make([]outerRef, len(sq.Outer))
	for i, id := range sq.Outer {
		src := id
		if r, ok := sq.Remap[id]; ok {
			src = r
		}
		pos, ok := l[src]
		if !ok {
			pos = -1
		}
		refs[i] = outerRef{id, pos}
	}
	correlated := len(refs) > 0
	limit := 0
	switch sq.Kind {
	case SubExists:
		limit = 1
	case SubScalar:
		limit = 2
	}
	run := func(c *Ctx, row []types.Value) ([][]types.Value, error) {
		if !correlated {
			if v, ok := c.cached(sq.ID); ok {
				return v.([][]types.Value), nil
			}
		}
		var outer map[ColumnID]types.Value
		if correlated {
			outer = make(map[ColumnID]types.Value, len(refs)+len(c.Outer))
			for k, v := range c.Outer {
				outer[k] = v
			}
			for _, r := range refs {
				if r.pos >= 0 {
					outer[r.id] = row[r.pos]
				} else if v, ok := c.Outer[r.id]; ok {
					outer[r.id] = v
				}
			}
		} else {
			outer = c.Outer
		}
		rows, err := c.Sub.RunSubquery(c, sq, outer, limit)
		if err != nil {
			return nil, err
		}
		if !correlated {
			c.setCached(sq.ID, rows)
		}
		return rows, nil
	}
	cmp := sq.Cmp
	switch sq.Kind {
	case SubScalar:
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			rows, err := run(c, row)
			if err != nil {
				return types.Null, err
			}
			if len(rows) > 1 {
				return types.Null, pgerr.New(pgerr.CardinalityViolation, "more than one row returned by a subquery used as an expression")
			}
			if len(rows) == 0 {
				return types.Null, nil
			}
			return rows[0][0], nil
		}, nil
	case SubExists:
		not := sq.Not
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			rows, err := run(c, row)
			if err != nil {
				return types.Null, err
			}
			return types.NewBool((len(rows) > 0) != not), nil
		}, nil
	case SubArray:
		et := sq.T.Elem()
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			rows, err := run(c, row)
			if err != nil {
				return types.Null, err
			}
			vals := make([]types.Value, len(rows))
			for i, r := range rows {
				vals[i] = r[0]
			}
			return types.NewArray(et, vals), nil
		}, nil
	case SubAny, SubAll:
		all := sq.Kind == SubAll
		not := sq.Not
		hashable := !correlated && cmp.Op == "=" && !all
		return func(c *Ctx, row []types.Value) (types.Value, error) {
			v, err := arg(c, row)
			if err != nil {
				return types.Null, err
			}
			if hashable {
				hs, err := hashSet(c, sq, run, row)
				if err != nil {
					return types.Null, err
				}
				var r types.Value
				switch {
				case hs.empty:
					r = types.False
				case v.IsNull():
					r = types.Null
				case hs.set[string(types.EncodeKey(nil, v))]:
					r = types.True
				case hs.hasNull:
					r = types.Null
				default:
					r = types.False
				}
				if not && !r.IsNull() {
					r = types.NewBool(!r.Bool())
				}
				return r, nil
			}
			rows, err := run(c, row)
			if err != nil {
				return types.Null, err
			}
			// ANY: true if any comparison is true; NULL if none true but
			// some NULL; else false. ALL is the dual.
			sawNull := false
			result := all
			for _, r := range rows {
				w := r[0]
				if v.IsNull() || w.IsNull() {
					sawNull = true
					continue
				}
				t, err := cmp.Impl(c, []types.Value{v, w})
				if err != nil {
					return types.Null, err
				}
				if all && !t.Bool() {
					result = false
					sawNull = false
					break
				}
				if !all && t.Bool() {
					result = true
					sawNull = false
					break
				}
			}
			var out types.Value
			if sawNull && result == all {
				out = types.Null
			} else {
				out = types.NewBool(result)
			}
			if not && !out.IsNull() {
				out = types.NewBool(!out.Bool())
			}
			return out, nil
		}, nil
	}
	return nil, pgerr.Internal("unknown subquery kind")
}

type subHash struct {
	set     map[string]bool
	hasNull bool
	empty   bool
}

func hashSet(c *Ctx, sq *Subquery, run func(*Ctx, []types.Value) ([][]types.Value, error), row []types.Value) (*subHash, error) {
	key := -sq.ID - 1
	if v, ok := c.cached(key); ok {
		return v.(*subHash), nil
	}
	rows, err := run(c, row)
	if err != nil {
		return nil, err
	}
	hs := &subHash{set: make(map[string]bool, len(rows)), empty: len(rows) == 0}
	for _, r := range rows {
		if r[0].IsNull() {
			hs.hasNull = true
			continue
		}
		hs.set[string(types.EncodeKey(nil, r[0]))] = true
	}
	c.setCached(key, hs)
	return hs, nil
}

// Eval compiles and evaluates a constant expression (no columns).
func Eval(c *Ctx, e Expr) (types.Value, error) {
	f, err := Compile(e, nil)
	if err != nil {
		return types.Null, err
	}
	return f(c, nil)
}
