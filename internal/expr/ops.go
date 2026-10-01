package expr

import (
	"math"
	"strings"
	"sync"

	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/types"
)

// Func is an executable function or operator.
type Func struct {
	Name string
	Op   string // operator symbol, "" for functions
	Impl func(c *Ctx, args []types.Value) (types.Value, error)
	// NonStrict functions are called even if an argument is NULL; strict
	// ones return NULL immediately.
	NonStrict bool
	Volatile  bool
}

var (
	funcCacheMu sync.Mutex
	funcCache   = map[string]*Func{}
)

// cached returns a shared *Func for a key so that structurally equal
// expressions share function pointers.
func cached(key string, mk func() *Func) *Func {
	funcCacheMu.Lock()
	defer funcCacheMu.Unlock()
	if f, ok := funcCache[key]; ok {
		return f
	}
	f := mk()
	funcCache[key] = f
	return f
}

// NumericRank orders the numeric types for implicit promotion.
func NumericRank(t types.T) int {
	switch t.Oid {
	case types.OidInt2:
		return 1
	case types.OidInt4:
		return 2
	case types.OidInt8, types.OidOid:
		return 3
	case types.OidNumeric:
		return 4
	case types.OidFloat4:
		return 5
	case types.OidFloat8:
		return 6
	}
	return 0
}

// CommonType returns the type two operands are coerced to for comparison
// or arithmetic, following PostgreSQL's implicit cast rules for the common
// cases.
func CommonType(a, b types.T) (types.T, bool) {
	if a.Oid == b.Oid {
		if a.Mod != b.Mod {
			return types.T{Oid: a.Oid, Mod: -1}, true
		}
		if a.Oid == types.OidUnknown {
			return types.Text, true
		}
		return a, true
	}
	if a.Oid == types.OidUnknown {
		return b, true
	}
	if b.Oid == types.OidUnknown {
		return a, true
	}
	ra, rb := NumericRank(a), NumericRank(b)
	if ra > 0 && rb > 0 {
		if ra >= rb {
			return types.T{Oid: a.Oid, Mod: -1}, true
		}
		return types.T{Oid: b.Oid, Mod: -1}, true
	}
	// Reg* types compare with integers and oids.
	if a.IsInteger() && b.IsInteger() {
		return types.Int8, true
	}
	if a.IsString() && b.IsString() {
		if a.Oid == types.OidBpchar && b.Oid == types.OidBpchar {
			return types.Bpchar, true
		}
		return types.Text, true
	}
	ka, kb := a.Kind(), b.Kind()
	isTime := func(k types.Kind) bool { return k == types.KDate || k == types.KTimestamp || k == types.KTimestampTZ }
	if isTime(ka) && isTime(kb) {
		if ka == types.KTimestampTZ || kb == types.KTimestampTZ {
			return types.TimestampTZ, true
		}
		return types.Timestamp, true
	}
	if ka == types.KArray && kb == types.KArray {
		return a, true
	}
	return types.Unknown, false
}

func errDivZero() error { return pgerr.New(pgerr.DivisionByZero, "division by zero") }

func checkFloat(f float64, inputsFinite bool) (types.Value, error) {
	if math.IsInf(f, 0) && inputsFinite {
		return types.Null, pgerr.New(pgerr.NumericValueOutOfRange, "value out of range: overflow")
	}
	return types.NewFloat(f), nil
}

func finite(fs ...float64) bool {
	for _, f := range fs {
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return false
		}
	}
	return true
}

// arith implements + - * / % for one numeric result type.
func arith(op string, t types.T) *Func {
	return cached("arith"+op+t.TypName(), func() *Func {
		f := &Func{Name: op, Op: op}
		switch t.Kind() {
		case types.KInt:
			f.Impl = func(_ *Ctx, a []types.Value) (types.Value, error) {
				x, y := a[0].I, a[1].I
				var r int64
				switch op {
				case "+":
					r = x + y
					if (y > 0 && r < x) || (y < 0 && r > x) {
						return types.Null, types.IntOutOfRange(t)
					}
				case "-":
					r = x - y
					if (y < 0 && r < x) || (y > 0 && r > x) {
						return types.Null, types.IntOutOfRange(t)
					}
				case "*":
					if x != 0 && y != 0 {
						r = x * y
						if r/y != x || (x == -1 && y == math.MinInt64) || (y == -1 && x == math.MinInt64) {
							return types.Null, types.IntOutOfRange(t)
						}
					}
				case "/":
					if y == 0 {
						return types.Null, errDivZero()
					}
					if y == -1 && x == math.MinInt64 {
						return types.Null, types.IntOutOfRange(t)
					}
					r = x / y
				case "%":
					if y == 0 {
						return types.Null, errDivZero()
					}
					if y == -1 {
						r = 0
					} else {
						r = x % y
					}
				}
				if err := types.CheckIntRange(r, t); err != nil {
					return types.Null, types.IntOutOfRange(t)
				}
				return types.NewInt(r), nil
			}
		case types.KFloat:
			f.Impl = func(_ *Ctx, a []types.Value) (types.Value, error) {
				x, y := a[0].AsFloat(), a[1].AsFloat()
				var r float64
				switch op {
				case "+":
					r = x + y
				case "-":
					r = x - y
				case "*":
					r = x * y
				case "/":
					if y == 0 {
						return types.Null, errDivZero()
					}
					r = x / y
				case "%":
					if y == 0 {
						return types.Null, errDivZero()
					}
					r = math.Mod(x, y)
				}
				if t.Oid == types.OidFloat4 {
					r = float64(float32(r))
				}
				return checkFloat(r, finite(x, y))
			}
		case types.KNumeric:
			f.Impl = func(_ *Ctx, a []types.Value) (types.Value, error) {
				x, y := a[0].AsDecimal(), a[1].AsDecimal()
				switch op {
				case "+":
					return types.NewNumeric(x.Add(y)), nil
				case "-":
					return types.NewNumeric(x.Sub(y)), nil
				case "*":
					return types.NewNumeric(x.Mul(y)), nil
				case "/":
					d, err := x.Div(y)
					if err != nil {
						return types.Null, err
					}
					return types.NewNumeric(d), nil
				case "%":
					d, err := x.Mod(y)
					if err != nil {
						return types.Null, err
					}
					return types.NewNumeric(d), nil
				}
				return types.Null, nil
			}
		}
		return f
	})
}

var powFunc = &Func{Name: "power", Op: "^", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
	x, y := a[0].AsFloat(), a[1].AsFloat()
	if x == 0 && y < 0 {
		return types.Null, pgerr.New("2201F", "zero raised to a negative power is undefined")
	}
	if x < 0 && y != math.Trunc(y) {
		return types.Null, pgerr.New("2201F", "a negative number raised to a non-integer power yields a complex result")
	}
	return checkFloat(math.Pow(x, y), finite(x, y))
}}

var numericPowFunc = &Func{Name: "power", Op: "^", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
	x, y := a[0].AsDecimal(), a[1].AsDecimal()
	if yi, ok := y.Int64(); ok && y.IsInteger() && yi >= 0 && yi <= 1000 {
		r := types.DecimalFromInt(1)
		for i := int64(0); i < yi; i++ {
			r = r.Mul(x)
		}
		return types.NewNumeric(r), nil
	}
	f := math.Pow(x.Float64(), y.Float64())
	d, err := types.DecimalFromFloat(f)
	if err != nil {
		return types.Null, err
	}
	return types.NewNumeric(d.Round(16)), nil
}}

func negFunc(t types.T) *Func {
	return cached("neg"+t.TypName(), func() *Func {
		return &Func{Name: "-", Op: "-", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
			v := a[0]
			switch v.K {
			case types.KInt:
				if v.I == math.MinInt64 {
					return types.Null, types.IntOutOfRange(t)
				}
				if err := types.CheckIntRange(-v.I, t); err != nil {
					return types.Null, err
				}
				return types.NewInt(-v.I), nil
			case types.KFloat:
				return types.NewFloat(-v.Float()), nil
			case types.KNumeric:
				return types.NewNumeric(v.Num().Neg()), nil
			case types.KInterval:
				return types.NewInterval(v.Interval().Neg()), nil
			}
			return types.Null, pgerr.New(pgerr.UndefinedFunction, "operator does not exist: - %s", t)
		}}
	})
}

// Comparison operators.
var compareOps = map[string]func(int) bool{
	"=":  func(c int) bool { return c == 0 },
	"<>": func(c int) bool { return c != 0 },
	"<":  func(c int) bool { return c < 0 },
	"<=": func(c int) bool { return c <= 0 },
	">":  func(c int) bool { return c > 0 },
	">=": func(c int) bool { return c >= 0 },
}

// IsComparison reports whether op is a comparison operator.
func IsComparison(op string) bool { _, ok := compareOps[op]; return ok }

// CompareFunc returns the comparison function for op on values of type t.
func CompareFunc(op string, t types.T) *Func {
	bp := t.Oid == types.OidBpchar
	key := "cmp" + op
	if bp {
		key += "bpchar"
	}
	return cached(key, func() *Func {
		test := compareOps[op]
		return &Func{Name: op, Op: op, Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
			x, y := a[0], a[1]
			if bp {
				x = types.NewText(strings.TrimRight(x.S, " "))
				y = types.NewText(strings.TrimRight(y.S, " "))
			}
			return types.NewBool(test(types.Compare(x, y))), nil
		}}
	})
}

// CommuteOp returns the operator with operands swapped (a < b == b > a).
func CommuteOp(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op
}

// NegateOp returns the negation of a comparison operator.
func NegateOp(op string) string {
	switch op {
	case "=":
		return "<>"
	case "<>":
		return "="
	case "<":
		return ">="
	case "<=":
		return ">"
	case ">":
		return "<="
	case ">=":
		return "<"
	}
	return ""
}

var concatFunc = &Func{Name: "textcat", Op: "||", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
	return types.NewText(a[0].S + a[1].S), nil
}}

var arrayCatFunc = &Func{Name: "array_cat", Op: "||", NonStrict: true, Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
	var elem types.T
	var vals []types.Value
	for _, v := range a {
		if v.IsNull() {
			continue
		}
		if v.K == types.KArray {
			elem = v.Arr().Elem
			vals = append(vals, v.Arr().Vals...)
		} else {
			vals = append(vals, v)
		}
	}
	return types.NewArray(elem, vals), nil
}}

func likeFunc(op string, ilike, not bool) *Func {
	return cached("like"+op, func() *Func {
		return &Func{Name: op, Op: op, Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
			esc := byte('\\')
			if len(a) > 2 {
				if a[2].S == "" {
					esc = 0
				} else {
					esc = a[2].S[0]
				}
			}
			m, err := Like(a[0].S, a[1].S, esc, ilike)
			if err != nil {
				return types.Null, err
			}
			return types.NewBool(m != not), nil
		}}
	})
}

func regexFunc(op string, icase, not bool) *Func {
	return cached("re"+op, func() *Func {
		return &Func{Name: op, Op: op, Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
			re, err := compileRegex(a[1].S, icase)
			if err != nil {
				return types.Null, err
			}
			return types.NewBool(re.MatchString(a[0].S) != not), nil
		}}
	})
}

// Date and time operators.
var (
	dateAddInt = &Func{Name: "date_pli", Op: "+", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		return types.NewDate(a[0].I + a[1].I), nil
	}}
	dateSubInt = &Func{Name: "date_mii", Op: "-", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		return types.NewDate(a[0].I - a[1].I), nil
	}}
	dateSubDate = &Func{Name: "date_mi", Op: "-", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		return types.NewInt(a[0].I - a[1].I), nil
	}}
	tsAddInterval = &Func{Name: "timestamp_pl_interval", Op: "+", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		ts := a[0].I
		if a[0].K == types.KDate {
			ts *= types.UsPerDay
		}
		r := types.AddIntervalToTimestamp(ts, a[1].Interval())
		if a[0].K == types.KTimestampTZ {
			return types.NewTimestampTZ(r), nil
		}
		return types.NewTimestamp(r), nil
	}}
	tsSubInterval = &Func{Name: "timestamp_mi_interval", Op: "-", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		ts := a[0].I
		if a[0].K == types.KDate {
			ts *= types.UsPerDay
		}
		r := types.AddIntervalToTimestamp(ts, a[1].Interval().Neg())
		if a[0].K == types.KTimestampTZ {
			return types.NewTimestampTZ(r), nil
		}
		return types.NewTimestamp(r), nil
	}}
	tsSubTs = &Func{Name: "timestamp_mi", Op: "-", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		return types.NewInterval(types.SubTimestamps(a[0].I, a[1].I)), nil
	}}
	intervalAdd = &Func{Name: "interval_pl", Op: "+", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		return types.NewInterval(a[0].Interval().Add(a[1].Interval())), nil
	}}
	intervalSub = &Func{Name: "interval_mi", Op: "-", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		return types.NewInterval(a[0].Interval().Add(a[1].Interval().Neg())), nil
	}}
	intervalMul = &Func{Name: "interval_mul", Op: "*", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		iv, f := a[0].Interval(), a[1].AsFloat()
		months := float64(iv.Months) * f
		days := float64(iv.Days)*f + (months-math.Trunc(months))*30
		us := float64(iv.Micros)*f + (days-math.Trunc(days))*float64(types.UsPerDay)
		return types.NewInterval(types.Interval{Months: int32(months), Days: int32(days), Micros: int64(us)}), nil
	}}
)

func intBitOp(op string) *Func {
	return cached("bit"+op, func() *Func {
		return &Func{Name: op, Op: op, Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
			x, y := a[0].I, a[1].I
			switch op {
			case "&":
				return types.NewInt(x & y), nil
			case "|":
				return types.NewInt(x | y), nil
			case "#":
				return types.NewInt(x ^ y), nil
			case "<<":
				return types.NewInt(x << uint(y&63)), nil
			case ">>":
				return types.NewInt(x >> uint(y&63)), nil
			}
			return types.Null, nil
		}}
	})
}

// BinaryOp describes the resolution of a binary operator.
type BinaryOp struct {
	Fn          *Func
	Ret         types.T
	Left, Right types.T // types the operands must be cast to
}

func opNotExist(op string, l, r types.T) error {
	return pgerr.New(pgerr.UndefinedFunction, "operator does not exist: %s %s %s", l, op, r).
		WithHint("No operator matches the given name and argument types. You might need to add explicit type casts.")
}

// ResolveBinary picks the implementation of a binary operator for the
// operand types.
func ResolveBinary(op string, l, r types.T) (BinaryOp, error) {
	lk, rk := l.Kind(), r.Kind()
	unknownL, unknownR := l.Oid == types.OidUnknown, r.Oid == types.OidUnknown
	switch op {
	case "=", "<>", "<", "<=", ">", ">=":
		if lk == types.KArray && unknownR {
			r = l
		} else if rk == types.KArray && unknownL {
			l = r
		}
		ct, ok := CommonType(l, r)
		if !ok {
			// A string literal compared with anything is cast to it.
			return BinaryOp{}, opNotExist(op, l, r)
		}
		return BinaryOp{Fn: CompareFunc(op, ct), Ret: types.Bool, Left: ct, Right: ct}, nil
	case "+", "-", "*", "/", "%":
		// Date/time arithmetic first.
		switch {
		case op == "+" && lk == types.KDate && (rk == types.KInt || unknownR) && !(unknownR && false):
			return BinaryOp{dateAddInt, types.Date, types.Date, types.Int4}, nil
		case op == "+" && (lk == types.KInt) && rk == types.KDate:
			return BinaryOp{Fn: &Func{Name: "integer_pl_date", Op: "+", Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
				return types.NewDate(a[0].I + a[1].I), nil
			}}, Ret: types.Date, Left: types.Int4, Right: types.Date}, nil
		case op == "-" && lk == types.KDate && rk == types.KDate:
			return BinaryOp{dateSubDate, types.Int4, types.Date, types.Date}, nil
		case op == "-" && lk == types.KDate && (rk == types.KInt):
			return BinaryOp{dateSubInt, types.Date, types.Date, types.Int4}, nil
		case op == "-" && lk == types.KDate && unknownR:
			return BinaryOp{dateSubDate, types.Int4, types.Date, types.Date}, nil
		case (op == "+" || op == "-") && (lk == types.KTimestamp || lk == types.KTimestampTZ || lk == types.KDate) && (rk == types.KInterval || (unknownR && lk != types.KDate)):
			ret := types.Timestamp
			lt := l
			if lk == types.KTimestampTZ {
				ret = types.TimestampTZ
			}
			if lk == types.KDate {
				lt = types.Date
			}
			if op == "+" {
				return BinaryOp{tsAddInterval, ret, lt, types.IntervalT}, nil
			}
			return BinaryOp{tsSubInterval, ret, lt, types.IntervalT}, nil
		case op == "+" && lk == types.KInterval && (rk == types.KTimestamp || rk == types.KTimestampTZ || rk == types.KDate):
			ret := types.Timestamp
			if rk == types.KTimestampTZ {
				ret = types.TimestampTZ
			}
			f := &Func{Name: "interval_pl_timestamp", Op: "+", Impl: func(c *Ctx, a []types.Value) (types.Value, error) {
				return tsAddInterval.Impl(c, []types.Value{a[1], a[0]})
			}}
			rt := r
			if rk == types.KDate {
				rt = types.Date
			}
			return BinaryOp{f, ret, types.IntervalT, rt}, nil
		case op == "-" && (lk == types.KTimestamp || lk == types.KTimestampTZ) && (rk == types.KTimestamp || rk == types.KTimestampTZ || unknownR):
			ct, _ := CommonType(l, r)
			return BinaryOp{tsSubTs, types.IntervalT, ct, ct}, nil
		case lk == types.KInterval && (rk == types.KInterval || unknownR) && (op == "+" || op == "-"):
			if op == "+" {
				return BinaryOp{intervalAdd, types.IntervalT, types.IntervalT, types.IntervalT}, nil
			}
			return BinaryOp{intervalSub, types.IntervalT, types.IntervalT, types.IntervalT}, nil
		case lk == types.KInterval && op == "*" && (NumericRank(r) > 0 || unknownR):
			return BinaryOp{intervalMul, types.IntervalT, types.IntervalT, types.Float8}, nil
		case rk == types.KInterval && op == "*" && NumericRank(l) > 0:
			f := &Func{Name: "mul_d_interval", Op: "*", Impl: func(c *Ctx, a []types.Value) (types.Value, error) {
				return intervalMul.Impl(c, []types.Value{a[1], a[0]})
			}}
			return BinaryOp{f, types.IntervalT, types.Float8, types.IntervalT}, nil
		}
		if unknownL && unknownR {
			return BinaryOp{}, pgerr.New(pgerr.UndefinedFunction, "operator is not unique: unknown %s unknown", op)
		}
		lt, rt := l, r
		if unknownL {
			lt = r
		}
		if unknownR {
			rt = l
		}
		if NumericRank(lt) == 0 || NumericRank(rt) == 0 {
			return BinaryOp{}, opNotExist(op, l, r)
		}
		ct, _ := CommonType(lt, rt)
		return BinaryOp{arith(op, ct), ct, ct, ct}, nil
	case "^":
		lt, rt := l, r
		if unknownL {
			lt = types.Float8
		}
		if unknownR {
			rt = types.Float8
		}
		if NumericRank(lt) == 0 || NumericRank(rt) == 0 {
			return BinaryOp{}, opNotExist(op, l, r)
		}
		if lt.Oid == types.OidNumeric || rt.Oid == types.OidNumeric {
			if NumericRank(lt) <= 4 && NumericRank(rt) <= 4 {
				return BinaryOp{numericPowFunc, types.Numeric, types.Numeric, types.Numeric}, nil
			}
		}
		return BinaryOp{powFunc, types.Float8, types.Float8, types.Float8}, nil
	case "||":
		if lk == types.KArray || rk == types.KArray {
			at := l
			if lk != types.KArray {
				at = r
			}
			lt, rt := l, r
			if unknownL {
				lt = at
			}
			if unknownR {
				rt = at
			}
			return BinaryOp{arrayCatFunc, at, lt, rt}, nil
		}
		return BinaryOp{concatFunc, types.Text, types.Text, types.Text}, nil
	case "~~", "!~~", "~~*", "!~~*":
		return BinaryOp{likeFunc(op, strings.Contains(op, "*"), strings.HasPrefix(op, "!")), types.Bool, types.Text, types.Text}, nil
	case "~", "!~", "~*", "!~*":
		return BinaryOp{regexFunc(op, strings.Contains(op, "*"), strings.HasPrefix(op, "!")), types.Bool, types.Text, types.Text}, nil
	case "&", "|", "#", "<<", ">>":
		lt, rt := l, r
		if unknownL {
			lt = r
		}
		if unknownR {
			rt = l
		}
		if !lt.IsInteger() || !rt.IsInteger() {
			return BinaryOp{}, opNotExist(op, l, r)
		}
		ct, _ := CommonType(lt, rt)
		if op == "<<" || op == ">>" {
			return BinaryOp{intBitOp(op), lt, lt, types.Int4}, nil
		}
		return BinaryOp{intBitOp(op), ct, ct, ct}, nil
	}
	return BinaryOp{}, opNotExist(op, l, r)
}

// ResolveUnary resolves unary minus.
func ResolveUnary(op string, t types.T) (*Func, types.T, types.T, error) {
	if op == "-" {
		if t.Oid == types.OidUnknown {
			t = types.Numeric
		}
		if NumericRank(t) > 0 || t.Kind() == types.KInterval {
			return negFunc(t), t, t, nil
		}
	}
	return nil, types.Unknown, types.Unknown, pgerr.New(pgerr.UndefinedFunction, "operator does not exist: %s %s", op, t)
}
