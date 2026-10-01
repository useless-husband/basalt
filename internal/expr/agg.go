package expr

import (
	"math"
	"math/big"
	"strings"

	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/types"
)

// AggState accumulates one aggregate for one group.
type AggState interface {
	Step(args []types.Value) error
	Result() (types.Value, error)
}

// AggDef is an aggregate function.
type AggDef struct {
	Name string
	// Resolve returns the result type and argument casts.
	Resolve func(args []types.T) (types.T, []types.T, error)
	New     func(ret types.T) AggState
	// NullOK aggregates see NULL arguments (array_agg); others skip rows
	// where any argument is NULL.
	NullOK bool
}

// AggCall is an aggregate call in a query.
type AggCall struct {
	Def      *AggDef
	Args     []Expr
	Distinct bool
	Star     bool
	Filter   Expr
	OrderBy  []SortKey
	T        types.T
	ID       ColumnID // output column of the Aggregate operator
}

// SortKey is an ORDER BY key.
type SortKey struct {
	E          Expr
	Desc       bool
	NullsFirst bool
}

func (a *AggCall) String() string {
	var b strings.Builder
	b.WriteString(a.Def.Name)
	b.WriteByte('(')
	if a.Star {
		b.WriteByte('*')
	}
	if a.Distinct {
		b.WriteString("DISTINCT ")
	}
	b.WriteString(joinExprs(a.Args, ", "))
	b.WriteByte(')')
	if a.Filter != nil {
		b.WriteString(" FILTER (WHERE " + a.Filter.String() + ")")
	}
	return b.String()
}

var aggs = map[string]*AggDef{}

// LookupAgg returns an aggregate by name.
func LookupAgg(name string) (*AggDef, bool) {
	a, ok := aggs[name]
	return a, ok
}

// IsAggregate reports whether name is an aggregate function.
func IsAggregate(name string) bool { _, ok := aggs[name]; return ok }

type countState struct{ n int64 }

func (s *countState) Step([]types.Value) error { s.n++; return nil }
func (s *countState) Result() (types.Value, error) {
	return types.NewInt(s.n), nil
}

// sumState sums integers exactly (switching to big arithmetic on
// overflow), numerics exactly and floats in float64.
type sumState struct {
	ret   types.T
	any   bool
	i     int64
	big   *big.Int
	dec   types.Decimal
	f     float64
	isDec bool
	count int64
	avg   bool
}

func (s *sumState) Step(a []types.Value) error {
	v := a[0]
	s.any = true
	s.count++
	switch v.K {
	case types.KInt:
		if s.big != nil {
			s.big.Add(s.big, big.NewInt(v.I))
			return nil
		}
		r := s.i + v.I
		if (v.I > 0 && r < s.i) || (v.I < 0 && r > s.i) {
			s.big = big.NewInt(s.i)
			s.big.Add(s.big, big.NewInt(v.I))
			return nil
		}
		s.i = r
	case types.KNumeric:
		if !s.isDec {
			s.isDec = true
			s.dec = types.DecimalFromInt(0)
		}
		s.dec = s.dec.Add(v.Num())
	case types.KFloat:
		s.f += v.Float()
	}
	return nil
}

func (s *sumState) exact() types.Decimal {
	if s.isDec {
		return s.dec
	}
	if s.big != nil {
		return types.DecimalFromBig(s.big, 0)
	}
	return types.DecimalFromInt(s.i)
}

func (s *sumState) Result() (types.Value, error) {
	if !s.any {
		return types.Null, nil
	}
	if s.avg {
		switch s.ret.Kind() {
		case types.KFloat:
			return types.NewFloat(s.f / float64(s.count)), nil
		}
		q, err := s.exact().Div(types.DecimalFromInt(s.count))
		if err != nil {
			return types.Null, err
		}
		return types.NewNumeric(q), nil
	}
	switch s.ret.Kind() {
	case types.KInt:
		if s.big != nil {
			return types.Null, pgerr.New(pgerr.NumericValueOutOfRange, "bigint out of range")
		}
		return types.NewInt(s.i), nil
	case types.KFloat:
		return types.NewFloat(s.f), nil
	case types.KInterval:
		return types.Null, nil
	}
	return types.NewNumeric(s.exact()), nil
}

type intervalSum struct {
	any bool
	iv  types.Interval
	n   int64
	avg bool
}

func (s *intervalSum) Step(a []types.Value) error {
	s.any = true
	s.n++
	s.iv = s.iv.Add(a[0].Interval())
	return nil
}

func (s *intervalSum) Result() (types.Value, error) {
	if !s.any {
		return types.Null, nil
	}
	if s.avg {
		f := 1 / float64(s.n)
		return intervalMul.Impl(nil, []types.Value{types.NewInterval(s.iv), types.NewFloat(f)})
	}
	return types.NewInterval(s.iv), nil
}

type minMaxState struct {
	v   types.Value
	any bool
	max bool
}

func (s *minMaxState) Step(a []types.Value) error {
	v := a[0]
	if !s.any {
		s.v, s.any = v, true
		return nil
	}
	c := types.Compare(v, s.v)
	if (s.max && c > 0) || (!s.max && c < 0) {
		s.v = v
	}
	return nil
}

func (s *minMaxState) Result() (types.Value, error) {
	if !s.any {
		return types.Null, nil
	}
	return s.v, nil
}

type boolState struct {
	and, any, v bool
}

func (s *boolState) Step(a []types.Value) error {
	b := a[0].Bool()
	if !s.any {
		s.v, s.any = b, true
		return nil
	}
	if s.and {
		s.v = s.v && b
	} else {
		s.v = s.v || b
	}
	return nil
}

func (s *boolState) Result() (types.Value, error) {
	if !s.any {
		return types.Null, nil
	}
	return types.NewBool(s.v), nil
}

type stringAgg struct {
	b   strings.Builder
	any bool
}

func (s *stringAgg) Step(a []types.Value) error {
	if a[0].IsNull() {
		return nil
	}
	if s.any && !a[1].IsNull() {
		s.b.WriteString(a[1].S)
	}
	s.any = true
	s.b.WriteString(a[0].S)
	return nil
}

func (s *stringAgg) Result() (types.Value, error) {
	if !s.any {
		return types.Null, nil
	}
	return types.NewText(s.b.String()), nil
}

type arrayAgg struct {
	elem types.T
	vals []types.Value
	any  bool
}

func (s *arrayAgg) Step(a []types.Value) error {
	s.any = true
	s.vals = append(s.vals, a[0])
	return nil
}

func (s *arrayAgg) Result() (types.Value, error) {
	if !s.any {
		return types.Null, nil
	}
	return types.NewArray(s.elem, s.vals), nil
}

// varState computes variance/stddev with Welford's algorithm.
type varState struct {
	n         float64
	mean, m2  float64
	pop, sqrt bool
	ret       types.T
}

func (s *varState) Step(a []types.Value) error {
	x := a[0].AsFloat()
	s.n++
	d := x - s.mean
	s.mean += d / s.n
	s.m2 += d * (x - s.mean)
	return nil
}

func (s *varState) Result() (types.Value, error) {
	div := s.n - 1
	if s.pop {
		div = s.n
	}
	if s.n == 0 || div <= 0 {
		return types.Null, nil
	}
	r := s.m2 / div
	if s.sqrt {
		r = math.Sqrt(r)
	}
	if s.ret.Oid == types.OidNumeric {
		d, err := types.DecimalFromFloat(r)
		if err != nil {
			return types.Null, err
		}
		return types.NewNumeric(d), nil
	}
	return types.NewFloat(r), nil
}

func init() {
	aggs["count"] = &AggDef{Name: "count", Resolve: func(a []types.T) (types.T, []types.T, error) {
		return types.Int8, a, nil
	}, New: func(types.T) AggState { return &countState{} }}
	aggs["sum"] = &AggDef{Name: "sum", Resolve: func(a []types.T) (types.T, []types.T, error) {
		if len(a) != 1 {
			return types.Unknown, nil, undefinedFunc("sum", a)
		}
		switch a[0].Oid {
		case types.OidInt2, types.OidInt4:
			return types.Int8, a, nil
		case types.OidInt8, types.OidNumeric:
			return types.Numeric, a, nil
		case types.OidUnknown:
			return types.Numeric, []types.T{types.Numeric}, nil
		case types.OidFloat4, types.OidFloat8:
			return types.Float8, []types.T{types.Float8}, nil
		case types.OidInterval:
			return types.IntervalT, a, nil
		}
		return types.Unknown, nil, undefinedFunc("sum", a)
	}, New: func(ret types.T) AggState {
		if ret.Oid == types.OidInterval {
			return &intervalSum{}
		}
		return &sumState{ret: ret}
	}}
	aggs["avg"] = &AggDef{Name: "avg", Resolve: func(a []types.T) (types.T, []types.T, error) {
		if len(a) != 1 {
			return types.Unknown, nil, undefinedFunc("avg", a)
		}
		switch a[0].Oid {
		case types.OidInt2, types.OidInt4, types.OidInt8, types.OidNumeric:
			return types.Numeric, a, nil
		case types.OidUnknown:
			return types.Numeric, []types.T{types.Numeric}, nil
		case types.OidFloat4, types.OidFloat8:
			return types.Float8, []types.T{types.Float8}, nil
		case types.OidInterval:
			return types.IntervalT, a, nil
		}
		return types.Unknown, nil, undefinedFunc("avg", a)
	}, New: func(ret types.T) AggState {
		if ret.Oid == types.OidInterval {
			return &intervalSum{avg: true}
		}
		return &sumState{ret: ret, avg: true}
	}}
	minmax := func(name string, isMax bool) {
		aggs[name] = &AggDef{Name: name, Resolve: func(a []types.T) (types.T, []types.T, error) {
			if len(a) != 1 {
				return types.Unknown, nil, undefinedFunc(name, a)
			}
			t := a[0]
			if t.Oid == types.OidUnknown {
				t = types.Text
			}
			return t, []types.T{t}, nil
		}, New: func(types.T) AggState { return &minMaxState{max: isMax} }}
	}
	minmax("min", false)
	minmax("max", true)
	boolAgg := func(name string, and bool) {
		aggs[name] = &AggDef{Name: name, Resolve: func(a []types.T) (types.T, []types.T, error) {
			return types.Bool, []types.T{types.Bool}, nil
		}, New: func(types.T) AggState { return &boolState{and: and} }}
	}
	boolAgg("bool_and", true)
	boolAgg("every", true)
	boolAgg("bool_or", false)
	aggs["string_agg"] = &AggDef{Name: "string_agg", NullOK: true, Resolve: func(a []types.T) (types.T, []types.T, error) {
		if len(a) != 2 {
			return types.Unknown, nil, undefinedFunc("string_agg", a)
		}
		return types.Text, []types.T{types.Text, types.Text}, nil
	}, New: func(types.T) AggState { return &stringAgg{} }}
	aggs["array_agg"] = &AggDef{Name: "array_agg", NullOK: true, Resolve: func(a []types.T) (types.T, []types.T, error) {
		if len(a) != 1 {
			return types.Unknown, nil, undefinedFunc("array_agg", a)
		}
		t := a[0]
		if t.Oid == types.OidUnknown {
			t = types.Text
		}
		at, ok := types.ArrayOf(t)
		if !ok {
			return types.Unknown, nil, pgerr.Unsupported("array_agg of type %s is not supported", t)
		}
		return at, []types.T{t}, nil
	}, New: func(ret types.T) AggState { return &arrayAgg{elem: ret.Elem()} }}
	variance := func(name string, pop, sqrt bool) {
		aggs[name] = &AggDef{Name: name, Resolve: func(a []types.T) (types.T, []types.T, error) {
			if len(a) != 1 || NumericRank(a[0]) == 0 {
				return types.Unknown, nil, undefinedFunc(name, a)
			}
			if a[0].Kind() == types.KFloat {
				return types.Float8, []types.T{types.Float8}, nil
			}
			return types.Numeric, a, nil
		}, New: func(ret types.T) AggState { return &varState{pop: pop, sqrt: sqrt, ret: ret} }}
	}
	variance("variance", false, false)
	variance("var_samp", false, false)
	variance("var_pop", true, false)
	variance("stddev", false, true)
	variance("stddev_samp", false, true)
	variance("stddev_pop", true, true)
}
