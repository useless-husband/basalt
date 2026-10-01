package expr

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/types"
)

// Env gives functions access to the session and the database.
type Env interface {
	TxnTimestamp() int64
	StatementTimestamp() int64
	User() string
	Database() string
	BackendPID() int64
	Nextval(seq string) (int64, error)
	Currval(seq string) (int64, error)
	Setval(seq string, v int64, isCalled bool) (int64, error)
	// CatalogFunc implements the pg_* introspection functions.
	CatalogFunc(name string, args []types.Value) (types.Value, error)
	Setting(name string) (string, bool)
	TxnID() int64
}

// Builtin is a function signature family.
type Builtin struct {
	Name     string
	Min, Max int // argument count bounds; Max < 0 means variadic
	// Resolve returns the result type and the types to cast the arguments
	// to, given the argument types.
	Resolve func(args []types.T) (types.T, []types.T, error)
	Fn      *Func
}

var builtins = map[string]*Builtin{}

func reg(b *Builtin) {
	if b.Fn.Name == "" {
		b.Fn.Name = b.Name
	}
	builtins[b.Name] = b
}

// HasFunction reports whether a scalar function exists.
func HasFunction(name string) bool { _, ok := builtins[name]; return ok }

// LookupFunc resolves a function call.
func LookupFunc(name string, args []types.T) (*Func, types.T, []types.T, error) {
	b, ok := builtins[name]
	if !ok {
		return nil, types.Unknown, nil, undefinedFunc(name, args)
	}
	if len(args) < b.Min || (b.Max >= 0 && len(args) > b.Max) {
		return nil, types.Unknown, nil, undefinedFunc(name, args)
	}
	ret, casts, err := b.Resolve(args)
	if err != nil {
		return nil, types.Unknown, nil, err
	}
	return b.Fn, ret, casts, nil
}

func undefinedFunc(name string, args []types.T) error {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = a.String()
	}
	return pgerr.New(pgerr.UndefinedFunction, "function %s(%s) does not exist", name, strings.Join(parts, ", ")).
		WithHint("No function matches the given name and argument types. You might need to add explicit type casts.")
}

// fixed returns a resolver for a fixed signature.
func fixed(ret types.T, params ...types.T) func([]types.T) (types.T, []types.T, error) {
	return func(args []types.T) (types.T, []types.T, error) {
		casts := make([]types.T, len(args))
		for i := range args {
			if i < len(params) {
				casts[i] = params[i]
			} else {
				casts[i] = params[len(params)-1]
			}
		}
		return ret, casts, nil
	}
}

// numericOrFloat resolves math functions: numeric in, numeric out;
// anything else goes through float8.
func numericOrFloat(extra ...types.T) func([]types.T) (types.T, []types.T, error) {
	return func(args []types.T) (types.T, []types.T, error) {
		t := args[0]
		casts := make([]types.T, len(args))
		ret := types.Float8
		if t.Oid == types.OidNumeric || t.Oid == types.OidUnknown || (t.IsInteger() && len(extra) > 0) {
			ret = types.Numeric
		} else if NumericRank(t) == 0 {
			return types.Unknown, nil, undefinedFunc("function", args)
		}
		casts[0] = ret
		for i := 1; i < len(args); i++ {
			casts[i] = types.Int4
		}
		return ret, casts, nil
	}
}

func sameType(args []types.T) (types.T, []types.T, error) {
	t := args[0]
	if t.Oid == types.OidUnknown {
		t = types.Text
	}
	casts := make([]types.T, len(args))
	for i := range casts {
		casts[i] = t
	}
	return t, casts, nil
}

func textArgs(ret types.T) func([]types.T) (types.T, []types.T, error) {
	return func(args []types.T) (types.T, []types.T, error) {
		casts := make([]types.T, len(args))
		for i := range casts {
			casts[i] = types.Text
		}
		return ret, casts, nil
	}
}

func fn(impl func(c *Ctx, a []types.Value) (types.Value, error)) *Func { return &Func{Impl: impl} }

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func pgSubstr(s string, start int64, length int64, hasLen bool) (string, error) {
	r := []rune(s)
	if hasLen && length < 0 {
		return "", pgerr.New(pgerr.DataException+"", "negative substring length not allowed")
	}
	// PostgreSQL semantics: characters from start to start+length-1, with
	// positions before 1 counted but producing nothing.
	from := start
	to := int64(len(r)) + 1
	if hasLen {
		to = start + length
	}
	if from < 1 {
		from = 1
	}
	if to > int64(len(r))+1 {
		to = int64(len(r)) + 1
	}
	if to <= from {
		return "", nil
	}
	return string(r[from-1 : to-1]), nil
}

func trimChars(s, chars string, left, right bool) string {
	set := func(r rune) bool { return strings.ContainsRune(chars, r) }
	if left {
		s = strings.TrimLeftFunc(s, set)
	}
	if right {
		s = strings.TrimRightFunc(s, set)
	}
	return s
}

func roundFloat(f float64) float64 {
	// PostgreSQL rounds float8 half to even (rint).
	return math.RoundToEven(f)
}

func extractField(field string, v types.Value) (types.Value, error) {
	var t time.Time
	var iv types.Interval
	isIv := false
	switch v.K {
	case types.KDate:
		t = types.TimeFromDate(v.I)
	case types.KTimestamp, types.KTimestampTZ:
		t = types.TimeFromTimestamp(v.I)
	case types.KInterval:
		iv = v.Interval()
		isIv = true
	default:
		return types.Null, pgerr.New(pgerr.DatatypeMismatch, "cannot extract from this type")
	}
	f := strings.ToLower(field)
	num := func(x float64) types.Value {
		d, _ := types.DecimalFromFloat(x)
		return types.NewNumeric(d)
	}
	ival := func(x int64) types.Value { return types.NewNumeric(types.DecimalFromInt(x)) }
	if isIv {
		switch f {
		case "year", "years":
			return ival(int64(iv.Months / 12)), nil
		case "month", "months", "mon":
			return ival(int64(iv.Months % 12)), nil
		case "day", "days":
			return ival(int64(iv.Days)), nil
		case "hour", "hours":
			return ival(iv.Micros / 3_600_000_000), nil
		case "minute", "minutes":
			return ival((iv.Micros / 60_000_000) % 60), nil
		case "second", "seconds":
			return num(float64(iv.Micros%60_000_000) / 1e6), nil
		case "epoch":
			secs := float64(iv.Months)*30*86400 + float64(iv.Days)*86400 + float64(iv.Micros)/1e6
			return num(secs), nil
		}
		return types.Null, pgerr.New(pgerr.FeatureNotSupported, "interval units \"%s\" not supported", field)
	}
	switch f {
	case "year", "years":
		return ival(int64(t.Year())), nil
	case "month", "months", "mon":
		return ival(int64(t.Month())), nil
	case "day", "days":
		return ival(int64(t.Day())), nil
	case "hour", "hours":
		return ival(int64(t.Hour())), nil
	case "minute", "minutes":
		return ival(int64(t.Minute())), nil
	case "second", "seconds":
		return num(float64(t.Second()) + float64(t.Nanosecond())/1e9), nil
	case "milliseconds":
		return num(float64(t.Second())*1000 + float64(t.Nanosecond())/1e6), nil
	case "microseconds":
		return ival(int64(t.Second())*1_000_000 + int64(t.Nanosecond()/1000)), nil
	case "dow":
		return ival(int64(t.Weekday())), nil
	case "isodow":
		d := int64(t.Weekday())
		if d == 0 {
			d = 7
		}
		return ival(d), nil
	case "doy":
		return ival(int64(t.YearDay())), nil
	case "week":
		_, w := t.ISOWeek()
		return ival(int64(w)), nil
	case "quarter":
		return ival(int64((t.Month()-1)/3 + 1)), nil
	case "decade":
		return ival(int64(t.Year() / 10)), nil
	case "century":
		return ival(int64((t.Year() + 99) / 100)), nil
	case "epoch":
		return num(float64(t.UnixNano()) / 1e9), nil
	case "isoyear":
		y, _ := t.ISOWeek()
		return ival(int64(y)), nil
	}
	return types.Null, pgerr.New(pgerr.InvalidParameterValue, "unit \"%s\" not recognized for type timestamp", field)
}

func dateTrunc(field string, v types.Value) (types.Value, error) {
	t := types.TimeFromTimestamp(v.I)
	if v.K == types.KDate {
		t = types.TimeFromDate(v.I)
	}
	switch strings.ToLower(field) {
	case "microseconds":
	case "milliseconds":
		t = t.Truncate(time.Millisecond)
	case "second":
		t = t.Truncate(time.Second)
	case "minute":
		t = t.Truncate(time.Minute)
	case "hour":
		t = t.Truncate(time.Hour)
	case "day":
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case "week":
		wd := (int(t.Weekday()) + 6) % 7
		t = time.Date(t.Year(), t.Month(), t.Day()-wd, 0, 0, 0, 0, time.UTC)
	case "month":
		t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	case "quarter":
		t = time.Date(t.Year(), t.Month()-(t.Month()-1)%3, 1, 0, 0, 0, 0, time.UTC)
	case "year":
		t = time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	case "decade":
		t = time.Date(t.Year()-t.Year()%10, 1, 1, 0, 0, 0, 0, time.UTC)
	case "century":
		t = time.Date((t.Year()-1)/100*100+1, 1, 1, 0, 0, 0, 0, time.UTC)
	default:
		return types.Null, pgerr.New(pgerr.InvalidParameterValue, "unit \"%s\" not recognized for type timestamp", field)
	}
	if v.K == types.KTimestampTZ {
		return types.NewTimestampTZ(types.TimestampFromTime(t)), nil
	}
	return types.NewTimestamp(types.TimestampFromTime(t)), nil
}

func init() {
	// ---- math ----
	reg(&Builtin{Name: "abs", Min: 1, Max: 1, Resolve: func(a []types.T) (types.T, []types.T, error) {
		t := a[0]
		if t.Oid == types.OidUnknown {
			t = types.Float8
		}
		if NumericRank(t) == 0 {
			return types.Unknown, nil, undefinedFunc("abs", a)
		}
		return t, []types.T{t}, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		v := a[0]
		switch v.K {
		case types.KInt:
			if v.I == math.MinInt64 {
				return types.Null, pgerr.New(pgerr.NumericValueOutOfRange, "bigint out of range")
			}
			if v.I < 0 {
				return types.NewInt(-v.I), nil
			}
			return v, nil
		case types.KFloat:
			return types.NewFloat(math.Abs(v.Float())), nil
		}
		return types.NewNumeric(v.Num().Abs()), nil
	})})
	mathF := func(name string, f func(float64) (float64, error), d func(types.Decimal) (types.Decimal, error)) {
		reg(&Builtin{Name: name, Min: 1, Max: 1, Resolve: numericOrFloat(), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
			if a[0].K == types.KNumeric && d != nil {
				r, err := d(a[0].Num())
				if err != nil {
					return types.Null, err
				}
				return types.NewNumeric(r), nil
			}
			r, err := f(a[0].AsFloat())
			if err != nil {
				return types.Null, err
			}
			if a[0].K == types.KNumeric {
				dd, err := types.DecimalFromFloat(r)
				if err != nil {
					return types.Null, err
				}
				return types.NewNumeric(dd), nil
			}
			return types.NewFloat(r), nil
		})})
	}
	ok := func(f func(float64) float64) func(float64) (float64, error) {
		return func(x float64) (float64, error) { return f(x), nil }
	}
	mathF("ceil", ok(math.Ceil), func(d types.Decimal) (types.Decimal, error) { return d.Ceil(), nil })
	builtins["ceiling"] = builtins["ceil"]
	mathF("floor", ok(math.Floor), func(d types.Decimal) (types.Decimal, error) { return d.Floor(), nil })
	mathF("sqrt", func(x float64) (float64, error) {
		if x < 0 {
			return 0, pgerr.New("2201F", "cannot take square root of a negative number")
		}
		return math.Sqrt(x), nil
	}, func(d types.Decimal) (types.Decimal, error) { return d.Sqrt() })
	mathF("exp", ok(math.Exp), nil)
	mathF("ln", func(x float64) (float64, error) {
		if x <= 0 {
			return 0, pgerr.New("2201E", "cannot take logarithm of a non-positive number")
		}
		return math.Log(x), nil
	}, nil)
	mathF("sign", func(x float64) (float64, error) {
		switch {
		case x > 0:
			return 1, nil
		case x < 0:
			return -1, nil
		}
		return 0, nil
	}, func(d types.Decimal) (types.Decimal, error) { return types.DecimalFromInt(int64(d.Sign())), nil })
	for _, n := range []string{"sin", "cos", "tan", "asin", "acos", "atan", "cbrt", "degrees", "radians"} {
		var f func(float64) float64
		switch n {
		case "sin":
			f = math.Sin
		case "cos":
			f = math.Cos
		case "tan":
			f = math.Tan
		case "asin":
			f = math.Asin
		case "acos":
			f = math.Acos
		case "atan":
			f = math.Atan
		case "cbrt":
			f = math.Cbrt
		case "degrees":
			f = func(x float64) float64 { return x * 180 / math.Pi }
		case "radians":
			f = func(x float64) float64 { return x * math.Pi / 180 }
		}
		reg(&Builtin{Name: n, Min: 1, Max: 1, Resolve: fixed(types.Float8, types.Float8), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
			return types.NewFloat(f(a[0].AsFloat())), nil
		})})
	}
	reg(&Builtin{Name: "log", Min: 1, Max: 2, Resolve: numericOrFloat(), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		x := a[len(a)-1].AsFloat()
		if x <= 0 {
			return types.Null, pgerr.New("2201E", "cannot take logarithm of a non-positive number")
		}
		r := math.Log10(x)
		if len(a) == 2 {
			r = math.Log(x) / math.Log(a[0].AsFloat())
		}
		if a[0].K == types.KNumeric {
			d, _ := types.DecimalFromFloat(r)
			return types.NewNumeric(d), nil
		}
		return types.NewFloat(r), nil
	})})
	builtins["log"].Resolve = func(a []types.T) (types.T, []types.T, error) {
		ret := types.Float8
		if a[0].Oid == types.OidNumeric {
			ret = types.Numeric
		}
		c := make([]types.T, len(a))
		for i := range c {
			c[i] = ret
		}
		return ret, c, nil
	}
	reg(&Builtin{Name: "log10", Min: 1, Max: 1, Resolve: fixed(types.Float8, types.Float8), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[0].AsFloat() <= 0 {
			return types.Null, pgerr.New("2201E", "cannot take logarithm of a non-positive number")
		}
		return types.NewFloat(math.Log10(a[0].AsFloat())), nil
	})})
	reg(&Builtin{Name: "power", Min: 2, Max: 2, Resolve: func(a []types.T) (types.T, []types.T, error) {
		if a[0].Oid == types.OidNumeric || a[1].Oid == types.OidNumeric {
			return types.Numeric, []types.T{types.Numeric, types.Numeric}, nil
		}
		return types.Float8, []types.T{types.Float8, types.Float8}, nil
	}, Fn: fn(func(c *Ctx, a []types.Value) (types.Value, error) {
		if a[0].K == types.KNumeric {
			return numericPowFunc.Impl(c, a)
		}
		return powFunc.Impl(c, a)
	})})
	builtins["pow"] = builtins["power"]
	reg(&Builtin{Name: "round", Min: 1, Max: 2, Resolve: func(a []types.T) (types.T, []types.T, error) {
		if len(a) == 2 || a[0].Oid == types.OidNumeric || a[0].Oid == types.OidUnknown || a[0].IsInteger() {
			c := []types.T{types.Numeric}
			if len(a) == 2 {
				c = append(c, types.Int4)
			}
			return types.Numeric, c, nil
		}
		return types.Float8, []types.T{types.Float8}, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[0].K == types.KFloat {
			return types.NewFloat(roundFloat(a[0].Float())), nil
		}
		s := int32(0)
		if len(a) == 2 {
			s = int32(a[1].I)
		}
		return types.NewNumeric(a[0].Num().Round(s)), nil
	})})
	reg(&Builtin{Name: "trunc", Min: 1, Max: 2, Resolve: builtins["round"].Resolve, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[0].K == types.KFloat {
			return types.NewFloat(math.Trunc(a[0].Float())), nil
		}
		s := int32(0)
		if len(a) == 2 {
			s = int32(a[1].I)
		}
		return types.NewNumeric(a[0].Num().Trunc(s)), nil
	})})
	reg(&Builtin{Name: "mod", Min: 2, Max: 2, Resolve: func(a []types.T) (types.T, []types.T, error) {
		ct, ok := CommonType(a[0], a[1])
		if !ok || NumericRank(ct) == 0 {
			return types.Unknown, nil, undefinedFunc("mod", a)
		}
		return ct, []types.T{ct, ct}, nil
	}, Fn: fn(func(c *Ctx, a []types.Value) (types.Value, error) {
		t := types.Int8
		if a[0].K == types.KNumeric {
			t = types.Numeric
		} else if a[0].K == types.KFloat {
			t = types.Float8
		}
		return arith("%", t).Impl(c, a)
	})})
	reg(&Builtin{Name: "div", Min: 2, Max: 2, Resolve: fixed(types.Numeric, types.Numeric, types.Numeric), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		q, err := a[0].Num().DivScale(a[1].Num(), 10)
		if err != nil {
			return types.Null, err
		}
		return types.NewNumeric(q.Trunc(0)), nil
	})})
	reg(&Builtin{Name: "pi", Min: 0, Max: 0, Resolve: fixed(types.Float8), Fn: fn(func(*Ctx, []types.Value) (types.Value, error) {
		return types.NewFloat(math.Pi), nil
	})})
	reg(&Builtin{Name: "random", Min: 0, Max: 0, Resolve: fixed(types.Float8), Fn: &Func{Volatile: true, Impl: func(*Ctx, []types.Value) (types.Value, error) {
		return types.NewFloat(rand.Float64()), nil
	}}})
	reg(&Builtin{Name: "width_bucket", Min: 4, Max: 4, Resolve: fixed(types.Int4, types.Float8, types.Float8, types.Float8, types.Int4), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		x, lo, hi, n := a[0].AsFloat(), a[1].AsFloat(), a[2].AsFloat(), a[3].I
		if n <= 0 || lo == hi {
			return types.Null, pgerr.New("2201G", "invalid argument for width_bucket")
		}
		if x < lo {
			return types.NewInt(0), nil
		}
		if x >= hi {
			return types.NewInt(n + 1), nil
		}
		return types.NewInt(int64((x-lo)/(hi-lo)*float64(n)) + 1), nil
	})})

	// ---- strings ----
	lengthFn := &Builtin{Name: "length", Min: 1, Max: 1, Resolve: func(a []types.T) (types.T, []types.T, error) {
		if a[0].Kind() == types.KBytea {
			return types.Int4, []types.T{types.Bytea}, nil
		}
		return types.Int4, []types.T{types.Text}, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[0].K == types.KBytea {
			return types.NewInt(int64(len(a[0].S))), nil
		}
		return types.NewInt(int64(runeLen(a[0].S))), nil
	})}
	reg(lengthFn)
	builtins["char_length"] = lengthFn
	builtins["character_length"] = lengthFn
	reg(&Builtin{Name: "octet_length", Min: 1, Max: 1, Resolve: fixed(types.Int4, types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		return types.NewInt(int64(len(a[0].S))), nil
	})})
	reg(&Builtin{Name: "lower", Min: 1, Max: 1, Resolve: textArgs(types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		return types.NewText(strings.ToLower(a[0].S)), nil
	})})
	reg(&Builtin{Name: "upper", Min: 1, Max: 1, Resolve: textArgs(types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		return types.NewText(strings.ToUpper(a[0].S)), nil
	})})
	reg(&Builtin{Name: "initcap", Min: 1, Max: 1, Resolve: textArgs(types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		var b strings.Builder
		prev := false
		for _, r := range a[0].S {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				if prev {
					b.WriteRune(unicode.ToLower(r))
				} else {
					b.WriteRune(unicode.ToUpper(r))
				}
				prev = true
			} else {
				b.WriteRune(r)
				prev = false
			}
		}
		return types.NewText(b.String()), nil
	})})
	substr := &Builtin{Name: "substr", Min: 2, Max: 3, Resolve: func(a []types.T) (types.T, []types.T, error) {
		c := []types.T{types.Text, types.Int4, types.Int4}[:len(a)]
		return types.Text, c, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		var l int64
		if len(a) == 3 {
			l = a[2].I
		}
		s, err := pgSubstr(a[0].S, a[1].I, l, len(a) == 3)
		if err != nil {
			return types.Null, err
		}
		return types.NewText(s), nil
	})}
	reg(substr)
	builtins["substring"] = substr
	for _, n := range []string{"btrim", "ltrim", "rtrim"} {
		left, right := n != "rtrim", n != "ltrim"
		reg(&Builtin{Name: n, Min: 1, Max: 2, Resolve: textArgs(types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
			chars := " "
			if len(a) == 2 {
				chars = a[1].S
			}
			return types.NewText(trimChars(a[0].S, chars, left, right)), nil
		})})
	}
	builtins["trim"] = builtins["btrim"]
	reg(&Builtin{Name: "replace", Min: 3, Max: 3, Resolve: textArgs(types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[1].S == "" {
			return a[0], nil
		}
		return types.NewText(strings.ReplaceAll(a[0].S, a[1].S, a[2].S)), nil
	})})
	reg(&Builtin{Name: "translate", Min: 3, Max: 3, Resolve: textArgs(types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		from, to := []rune(a[1].S), []rune(a[2].S)
		var b strings.Builder
		for _, r := range a[0].S {
			i := -1
			for k, f := range from {
				if f == r {
					i = k
					break
				}
			}
			switch {
			case i < 0:
				b.WriteRune(r)
			case i < len(to):
				b.WriteRune(to[i])
			}
		}
		return types.NewText(b.String()), nil
	})})
	strpos := &Builtin{Name: "strpos", Min: 2, Max: 2, Resolve: textArgs(types.Int4), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		i := strings.Index(a[0].S, a[1].S)
		if i < 0 {
			return types.NewInt(0), nil
		}
		return types.NewInt(int64(runeLen(a[0].S[:i]) + 1)), nil
	})}
	reg(strpos)
	builtins["position"] = strpos
	reg(&Builtin{Name: "concat", Min: 0, Max: -1, Resolve: func(a []types.T) (types.T, []types.T, error) {
		c := make([]types.T, len(a))
		for i := range c {
			c[i] = types.Text
		}
		return types.Text, c, nil
	}, Fn: &Func{NonStrict: true, Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		var b strings.Builder
		for _, v := range a {
			if !v.IsNull() {
				b.WriteString(v.S)
			}
		}
		return types.NewText(b.String()), nil
	}}})
	reg(&Builtin{Name: "concat_ws", Min: 1, Max: -1, Resolve: builtins["concat"].Resolve, Fn: &Func{NonStrict: true, Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[0].IsNull() {
			return types.Null, nil
		}
		var parts []string
		for _, v := range a[1:] {
			if !v.IsNull() {
				parts = append(parts, v.S)
			}
		}
		return types.NewText(strings.Join(parts, a[0].S)), nil
	}}})
	reg(&Builtin{Name: "left", Min: 2, Max: 2, Resolve: fixed(types.Text, types.Text, types.Int4), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		r := []rune(a[0].S)
		n := int(a[1].I)
		if n < 0 {
			n = max(len(r)+n, 0)
		}
		return types.NewText(string(r[:min(n, len(r))])), nil
	})})
	reg(&Builtin{Name: "right", Min: 2, Max: 2, Resolve: fixed(types.Text, types.Text, types.Int4), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		r := []rune(a[0].S)
		n := int(a[1].I)
		if n < 0 {
			n = max(len(r)+n, 0)
		}
		n = min(n, len(r))
		return types.NewText(string(r[len(r)-n:])), nil
	})})
	pad := func(left bool) func(_ *Ctx, a []types.Value) (types.Value, error) {
		return func(_ *Ctx, a []types.Value) (types.Value, error) {
			r := []rune(a[0].S)
			n := int(a[1].I)
			fill := []rune(" ")
			if len(a) == 3 {
				fill = []rune(a[2].S)
			}
			if n <= len(r) {
				return types.NewText(string(r[:max(n, 0)])), nil
			}
			if len(fill) == 0 {
				return types.NewText(string(r)), nil
			}
			var p []rune
			for len(p) < n-len(r) {
				p = append(p, fill[len(p)%len(fill)])
			}
			if left {
				return types.NewText(string(p) + string(r)), nil
			}
			return types.NewText(string(r) + string(p)), nil
		}
	}
	reg(&Builtin{Name: "lpad", Min: 2, Max: 3, Resolve: fixed(types.Text, types.Text, types.Int4, types.Text), Fn: fn(pad(true))})
	reg(&Builtin{Name: "rpad", Min: 2, Max: 3, Resolve: fixed(types.Text, types.Text, types.Int4, types.Text), Fn: fn(pad(false))})
	reg(&Builtin{Name: "repeat", Min: 2, Max: 2, Resolve: fixed(types.Text, types.Text, types.Int4), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[1].I <= 0 {
			return types.NewText(""), nil
		}
		if int64(len(a[0].S))*a[1].I > 1<<28 {
			return types.Null, pgerr.New(pgerr.ProgramLimitExceeded, "requested length too large")
		}
		return types.NewText(strings.Repeat(a[0].S, int(a[1].I))), nil
	})})
	reg(&Builtin{Name: "reverse", Min: 1, Max: 1, Resolve: textArgs(types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		r := []rune(a[0].S)
		for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
			r[i], r[j] = r[j], r[i]
		}
		return types.NewText(string(r)), nil
	})})
	reg(&Builtin{Name: "split_part", Min: 3, Max: 3, Resolve: fixed(types.Text, types.Text, types.Text, types.Int4), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		parts := strings.Split(a[0].S, a[1].S)
		n := int(a[2].I)
		if n == 0 {
			return types.Null, pgerr.New(pgerr.InvalidParameterValue, "field position must not be zero")
		}
		if n < 0 {
			n = len(parts) + n + 1
		}
		if n < 1 || n > len(parts) {
			return types.NewText(""), nil
		}
		return types.NewText(parts[n-1]), nil
	})})
	reg(&Builtin{Name: "starts_with", Min: 2, Max: 2, Resolve: textArgs(types.Bool), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		return types.NewBool(strings.HasPrefix(a[0].S, a[1].S)), nil
	})})
	reg(&Builtin{Name: "md5", Min: 1, Max: 1, Resolve: textArgs(types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		h := md5.Sum([]byte(a[0].S))
		return types.NewText(hex.EncodeToString(h[:])), nil
	})})
	reg(&Builtin{Name: "chr", Min: 1, Max: 1, Resolve: fixed(types.Text, types.Int4), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[0].I <= 0 || a[0].I > unicode.MaxRune {
			return types.Null, pgerr.New(pgerr.ProgramLimitExceeded, "requested character too large")
		}
		return types.NewText(string(rune(a[0].I))), nil
	})})
	reg(&Builtin{Name: "ascii", Min: 1, Max: 1, Resolve: textArgs(types.Int4), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[0].S == "" {
			return types.NewInt(0), nil
		}
		r, _ := utf8.DecodeRuneInString(a[0].S)
		return types.NewInt(int64(r)), nil
	})})
	reg(&Builtin{Name: "to_hex", Min: 1, Max: 1, Resolve: fixed(types.Text, types.Int8), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		return types.NewText(strconv.FormatUint(uint64(a[0].I), 16)), nil
	})})
	reg(&Builtin{Name: "quote_ident", Min: 1, Max: 1, Resolve: textArgs(types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		s := a[0].S
		plain := s != ""
		for i, r := range s {
			if !(r == '_' || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9')) {
				plain = false
			}
		}
		if plain {
			return a[0], nil
		}
		return types.NewText(`"` + strings.ReplaceAll(s, `"`, `""`) + `"`), nil
	})})
	reg(&Builtin{Name: "quote_literal", Min: 1, Max: 1, Resolve: textArgs(types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		return types.NewText("'" + strings.ReplaceAll(a[0].S, "'", "''") + "'"), nil
	})})
	reg(&Builtin{Name: "regexp_replace", Min: 3, Max: 4, Resolve: textArgs(types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		flags := ""
		if len(a) == 4 {
			flags = a[3].S
		}
		re, err := compileRegex(a[1].S, strings.Contains(flags, "i"))
		if err != nil {
			return types.Null, err
		}
		repl := strings.ReplaceAll(a[2].S, `\`, `$`)
		if strings.Contains(flags, "g") {
			return types.NewText(re.ReplaceAllString(a[0].S, repl)), nil
		}
		done := false
		return types.NewText(re.ReplaceAllStringFunc(a[0].S, func(m string) string {
			if done {
				return m
			}
			done = true
			return re.ReplaceAllString(m, repl)
		})), nil
	})})
	reg(&Builtin{Name: "format", Min: 1, Max: -1, Resolve: builtins["concat"].Resolve, Fn: &Func{NonStrict: true, Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[0].IsNull() {
			return types.Null, nil
		}
		var b strings.Builder
		arg := 1
		f := a[0].S
		for i := 0; i < len(f); i++ {
			if f[i] != '%' || i+1 >= len(f) {
				b.WriteByte(f[i])
				continue
			}
			i++
			switch f[i] {
			case '%':
				b.WriteByte('%')
			case 's', 'I', 'L':
				if arg >= len(a) {
					return types.Null, pgerr.New(pgerr.InvalidParameterValue, "too few arguments for format()")
				}
				v := a[arg]
				arg++
				switch {
				case f[i] == 'L' && v.IsNull():
					b.WriteString("NULL")
				case f[i] == 'L':
					b.WriteString("'" + strings.ReplaceAll(v.S, "'", "''") + "'")
				case f[i] == 'I':
					b.WriteString(`"` + v.S + `"`)
				case !v.IsNull():
					b.WriteString(v.S)
				}
			default:
				return types.Null, pgerr.New(pgerr.InvalidParameterValue, "unrecognized format() type specifier \"%c\"", f[i])
			}
		}
		return types.NewText(b.String()), nil
	}}})
	reg(&Builtin{Name: "string_to_array", Min: 2, Max: 2, Resolve: fixed(types.T{Oid: types.OidTextArray, Mod: -1}, types.Text, types.Text), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		var vals []types.Value
		if a[0].S != "" {
			for _, p := range strings.Split(a[0].S, a[1].S) {
				vals = append(vals, types.NewText(p))
			}
		}
		return types.NewArray(types.Text, vals), nil
	})})
	reg(&Builtin{Name: "array_to_string", Min: 2, Max: 3, Resolve: func(a []types.T) (types.T, []types.T, error) {
		c := []types.T{a[0], types.Text, types.Text}[:len(a)]
		return types.Text, c, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		arr := a[0].Arr()
		var parts []string
		for _, v := range arr.Vals {
			if v.IsNull() {
				if len(a) == 3 {
					parts = append(parts, a[2].S)
				}
				continue
			}
			parts = append(parts, types.ToText(v, arr.Elem))
		}
		return types.NewText(strings.Join(parts, a[1].S)), nil
	})})

	// ---- arrays ----
	arrLen := func(a []types.Value) int64 { return int64(len(a[0].Arr().Vals)) }
	reg(&Builtin{Name: "array_length", Min: 2, Max: 2, Resolve: func(a []types.T) (types.T, []types.T, error) {
		return types.Int4, []types.T{a[0], types.Int4}, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[1].I != 1 || arrLen(a) == 0 {
			return types.Null, nil
		}
		return types.NewInt(arrLen(a)), nil
	})})
	reg(&Builtin{Name: "array_upper", Min: 2, Max: 2, Resolve: builtins["array_length"].Resolve, Fn: builtins["array_length"].Fn})
	reg(&Builtin{Name: "array_lower", Min: 2, Max: 2, Resolve: builtins["array_length"].Resolve, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[1].I != 1 || arrLen(a) == 0 {
			return types.Null, nil
		}
		return types.NewInt(1), nil
	})})
	reg(&Builtin{Name: "cardinality", Min: 1, Max: 1, Resolve: func(a []types.T) (types.T, []types.T, error) {
		return types.Int4, []types.T{a[0]}, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) { return types.NewInt(arrLen(a)), nil })})
	reg(&Builtin{Name: "array_append", Min: 2, Max: 2, Resolve: func(a []types.T) (types.T, []types.T, error) {
		return a[0], []types.T{a[0], a[0].Elem()}, nil
	}, Fn: &Func{NonStrict: true, Impl: func(_ *Ctx, a []types.Value) (types.Value, error) {
		if a[0].IsNull() {
			return types.NewArray(types.Unknown, []types.Value{a[1]}), nil
		}
		arr := a[0].Arr()
		return types.NewArray(arr.Elem, append(append([]types.Value(nil), arr.Vals...), a[1])), nil
	}}})
	reg(&Builtin{Name: "array_position", Min: 2, Max: 2, Resolve: func(a []types.T) (types.T, []types.T, error) {
		return types.Int4, []types.T{a[0], a[0].Elem()}, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		for i, v := range a[0].Arr().Vals {
			if types.Equal(v, a[1]) {
				return types.NewInt(int64(i + 1)), nil
			}
		}
		return types.Null, nil
	})})

	// ---- date/time ----
	reg(&Builtin{Name: "now", Min: 0, Max: 0, Resolve: fixed(types.TimestampTZ), Fn: &Func{Volatile: true, Impl: func(c *Ctx, _ []types.Value) (types.Value, error) {
		return types.NewTimestampTZ(c.Env.TxnTimestamp()), nil
	}}})
	builtins["current_timestamp"] = builtins["now"]
	builtins["transaction_timestamp"] = builtins["now"]
	reg(&Builtin{Name: "statement_timestamp", Min: 0, Max: 0, Resolve: fixed(types.TimestampTZ), Fn: &Func{Volatile: true, Impl: func(c *Ctx, _ []types.Value) (types.Value, error) {
		return types.NewTimestampTZ(c.Env.StatementTimestamp()), nil
	}}})
	reg(&Builtin{Name: "clock_timestamp", Min: 0, Max: 0, Resolve: fixed(types.TimestampTZ), Fn: &Func{Volatile: true, Impl: func(*Ctx, []types.Value) (types.Value, error) {
		return types.NewTimestampTZ(types.TimestampFromTime(time.Now())), nil
	}}})
	reg(&Builtin{Name: "localtimestamp", Min: 0, Max: 0, Resolve: fixed(types.Timestamp), Fn: &Func{Volatile: true, Impl: func(c *Ctx, _ []types.Value) (types.Value, error) {
		return types.NewTimestamp(c.Env.TxnTimestamp()), nil
	}}})
	reg(&Builtin{Name: "current_date", Min: 0, Max: 0, Resolve: fixed(types.Date), Fn: &Func{Volatile: true, Impl: func(c *Ctx, _ []types.Value) (types.Value, error) {
		us := c.Env.TxnTimestamp()
		d := us / types.UsPerDay
		if us%types.UsPerDay < 0 {
			d--
		}
		return types.NewDate(d), nil
	}}})
	reg(&Builtin{Name: "extract", Min: 2, Max: 2, Resolve: func(a []types.T) (types.T, []types.T, error) {
		t := a[1]
		if t.Oid == types.OidUnknown {
			t = types.Timestamp
		}
		return types.Numeric, []types.T{types.Text, t}, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) { return extractField(a[0].S, a[1]) })})
	reg(&Builtin{Name: "date_part", Min: 2, Max: 2, Resolve: func(a []types.T) (types.T, []types.T, error) {
		t := a[1]
		if t.Oid == types.OidUnknown {
			t = types.Timestamp
		}
		return types.Float8, []types.T{types.Text, t}, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		v, err := extractField(a[0].S, a[1])
		if err != nil {
			return types.Null, err
		}
		return types.NewFloat(v.AsFloat()), nil
	})})
	reg(&Builtin{Name: "date_trunc", Min: 2, Max: 2, Resolve: func(a []types.T) (types.T, []types.T, error) {
		t := a[1]
		if t.Kind() == types.KDate || t.Oid == types.OidUnknown {
			t = types.Timestamp
		}
		return t, []types.T{types.Text, t}, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) { return dateTrunc(a[0].S, a[1]) })})
	reg(&Builtin{Name: "make_date", Min: 3, Max: 3, Resolve: fixed(types.Date, types.Int4, types.Int4, types.Int4), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		d, err := types.ParseDate(fmt.Sprintf("%04d-%02d-%02d", a[0].I, a[1].I, a[2].I))
		if err != nil {
			return types.Null, pgerr.New(pgerr.DatetimeFieldOverflow, "date field value out of range: %d-%02d-%02d", a[0].I, a[1].I, a[2].I)
		}
		return types.NewDate(d), nil
	})})
	reg(&Builtin{Name: "timezone", Min: 2, Max: 2, Resolve: func(a []types.T) (types.T, []types.T, error) {
		if a[1].Oid == types.OidTimestampTZ {
			return types.Timestamp, []types.T{types.Text, types.TimestampTZ}, nil
		}
		return types.TimestampTZ, []types.T{types.Text, types.Timestamp}, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		loc, err := time.LoadLocation(a[0].S)
		if err != nil {
			return types.Null, pgerr.New(pgerr.InvalidParameterValue, "time zone \"%s\" not recognized", a[0].S)
		}
		t := types.TimeFromTimestamp(a[1].I)
		if a[1].K == types.KTimestampTZ {
			l := t.In(loc)
			naive := time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), l.Second(), l.Nanosecond(), time.UTC)
			return types.NewTimestamp(types.TimestampFromTime(naive)), nil
		}
		l := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
		return types.NewTimestampTZ(types.TimestampFromTime(l)), nil
	})})
	reg(&Builtin{Name: "age", Min: 1, Max: 2, Resolve: func(a []types.T) (types.T, []types.T, error) {
		return types.IntervalT, []types.T{types.Timestamp, types.Timestamp}[:len(a)], nil
	}, Fn: fn(func(c *Ctx, a []types.Value) (types.Value, error) {
		x, y := a[0].I, int64(0)
		if len(a) == 2 {
			y = a[1].I
		} else {
			x, y = c.Env.TxnTimestamp()/types.UsPerDay*types.UsPerDay, a[0].I
		}
		return types.NewInterval(types.SubTimestamps(x, y)), nil
	})})
	reg(&Builtin{Name: "to_char", Min: 2, Max: 2, Resolve: func(a []types.T) (types.T, []types.T, error) {
		return types.Text, []types.T{a[0], types.Text}, nil
	}, Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) { return toChar(a[0], a[1].S) })})

	// ---- session and system ----
	strConst := func(name string, f func(c *Ctx) string) {
		reg(&Builtin{Name: name, Min: 0, Max: 0, Resolve: fixed(types.Name), Fn: &Func{Volatile: true, Impl: func(c *Ctx, _ []types.Value) (types.Value, error) {
			return types.NewText(f(c)), nil
		}}})
	}
	strConst("current_user", func(c *Ctx) string { return c.Env.User() })
	strConst("session_user", func(c *Ctx) string { return c.Env.User() })
	strConst("user", func(c *Ctx) string { return c.Env.User() })
	strConst("current_role", func(c *Ctx) string { return c.Env.User() })
	strConst("current_database", func(c *Ctx) string { return c.Env.Database() })
	strConst("current_catalog", func(c *Ctx) string { return c.Env.Database() })
	strConst("current_schema", func(*Ctx) string { return "public" })
	reg(&Builtin{Name: "version", Min: 0, Max: 0, Resolve: fixed(types.Text), Fn: fn(func(c *Ctx, _ []types.Value) (types.Value, error) {
		v, _ := c.Env.Setting("basalt_version")
		return types.NewText(v), nil
	})})
	reg(&Builtin{Name: "current_schemas", Min: 1, Max: 1, Resolve: fixed(types.T{Oid: types.OidNameArray, Mod: -1}, types.Bool), Fn: fn(func(_ *Ctx, a []types.Value) (types.Value, error) {
		vals := []types.Value{types.NewText("public")}
		if a[0].Bool() {
			vals = append([]types.Value{types.NewText("pg_catalog")}, vals...)
		}
		return types.NewArray(types.Name, vals), nil
	})})
	reg(&Builtin{Name: "pg_backend_pid", Min: 0, Max: 0, Resolve: fixed(types.Int4), Fn: fn(func(c *Ctx, _ []types.Value) (types.Value, error) {
		return types.NewInt(c.Env.BackendPID()), nil
	})})
	reg(&Builtin{Name: "txid_current", Min: 0, Max: 0, Resolve: fixed(types.Int8), Fn: &Func{Volatile: true, Impl: func(c *Ctx, _ []types.Value) (types.Value, error) {
		return types.NewInt(c.Env.TxnID()), nil
	}}})
	reg(&Builtin{Name: "current_setting", Min: 1, Max: 2, Resolve: fixed(types.Text, types.Text, types.Bool), Fn: fn(func(c *Ctx, a []types.Value) (types.Value, error) {
		v, ok := c.Env.Setting(strings.ToLower(a[0].S))
		if !ok {
			if len(a) == 2 && a[1].Bool() {
				return types.Null, nil
			}
			return types.Null, pgerr.New(pgerr.UndefinedObject, "unrecognized configuration parameter \"%s\"", a[0].S)
		}
		return types.NewText(v), nil
	})})
	reg(&Builtin{Name: "pg_typeof", Min: 1, Max: 1, Resolve: func(a []types.T) (types.T, []types.T, error) {
		return types.Regtype, []types.T{a[0]}, nil
	}, Fn: &Func{NonStrict: true, Impl: func(*Ctx, []types.Value) (types.Value, error) {
		return types.Null, pgerr.Internal("pg_typeof is resolved by the planner")
	}}})
	reg(&Builtin{Name: "nextval", Min: 1, Max: 1, Resolve: fixed(types.Int8, types.Text), Fn: &Func{Volatile: true, Impl: func(c *Ctx, a []types.Value) (types.Value, error) {
		v, err := c.Env.Nextval(a[0].S)
		return types.NewInt(v), err
	}}})
	reg(&Builtin{Name: "currval", Min: 1, Max: 1, Resolve: fixed(types.Int8, types.Text), Fn: &Func{Volatile: true, Impl: func(c *Ctx, a []types.Value) (types.Value, error) {
		v, err := c.Env.Currval(a[0].S)
		return types.NewInt(v), err
	}}})
	reg(&Builtin{Name: "setval", Min: 2, Max: 3, Resolve: fixed(types.Int8, types.Text, types.Int8, types.Bool), Fn: &Func{Volatile: true, Impl: func(c *Ctx, a []types.Value) (types.Value, error) {
		called := true
		if len(a) == 3 {
			called = a[2].Bool()
		}
		v, err := c.Env.Setval(a[0].S, a[1].I, called)
		return types.NewInt(v), err
	}}})
	reg(&Builtin{Name: "pg_sleep", Min: 1, Max: 1, Resolve: fixed(types.Void, types.Float8), Fn: &Func{Volatile: true, Impl: func(c *Ctx, a []types.Value) (types.Value, error) {
		deadline := time.Now().Add(time.Duration(a[0].AsFloat() * float64(time.Second)))
		for time.Now().Before(deadline) {
			if c.Check != nil {
				if err := c.Check(); err != nil {
					return types.Null, err
				}
			}
			time.Sleep(min(time.Until(deadline), 10*time.Millisecond))
		}
		return types.Null, nil
	}}})
	reg(&Builtin{Name: "gen_random_uuid", Min: 0, Max: 0, Resolve: fixed(types.Text), Fn: &Func{Volatile: true, Impl: func(*Ctx, []types.Value) (types.Value, error) {
		b := make([]byte, 16)
		for i := range b {
			b[i] = byte(rand.IntN(256))
		}
		b[6] = b[6]&0x0f | 0x40
		b[8] = b[8]&0x3f | 0x80
		h := hex.EncodeToString(b)
		return types.NewText(h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]), nil
	}}})

	// ---- catalog introspection (implemented by the engine) ----
	catalogFn := func(name string, ret types.T, min, max int, params ...types.T) {
		reg(&Builtin{Name: name, Min: min, Max: max, Resolve: fixed(ret, params...), Fn: &Func{NonStrict: true, Impl: func(c *Ctx, a []types.Value) (types.Value, error) {
			return c.Env.CatalogFunc(name, a)
		}}})
	}
	catalogFn("pg_get_userbyid", types.Name, 1, 1, types.Oid)
	catalogFn("pg_table_is_visible", types.Bool, 1, 1, types.Oid)
	catalogFn("pg_type_is_visible", types.Bool, 1, 1, types.Oid)
	catalogFn("pg_function_is_visible", types.Bool, 1, 1, types.Oid)
	catalogFn("format_type", types.Text, 2, 2, types.Oid, types.Int4)
	catalogFn("pg_get_expr", types.Text, 2, 3, types.Text, types.Oid, types.Bool)
	catalogFn("pg_get_indexdef", types.Text, 1, 3, types.Oid, types.Int4, types.Bool)
	catalogFn("pg_get_constraintdef", types.Text, 1, 2, types.Oid, types.Bool)
	catalogFn("pg_get_viewdef", types.Text, 1, 2, types.Oid, types.Bool)
	catalogFn("pg_get_serial_sequence", types.Text, 2, 2, types.Text, types.Text)
	catalogFn("obj_description", types.Text, 1, 2, types.Oid, types.Name)
	catalogFn("col_description", types.Text, 2, 2, types.Oid, types.Int4)
	catalogFn("shobj_description", types.Text, 2, 2, types.Oid, types.Name)
	catalogFn("pg_relation_size", types.Int8, 1, 2, types.Regclass, types.Text)
	catalogFn("pg_total_relation_size", types.Int8, 1, 1, types.Regclass)
	catalogFn("pg_table_size", types.Int8, 1, 1, types.Regclass)
	catalogFn("pg_indexes_size", types.Int8, 1, 1, types.Regclass)
	catalogFn("pg_database_size", types.Int8, 1, 1, types.Text)
	catalogFn("pg_size_pretty", types.Text, 1, 1, types.Int8)
	catalogFn("pg_encoding_to_char", types.Name, 1, 1, types.Int4)
	catalogFn("has_table_privilege", types.Bool, 2, 3, types.Text, types.Text, types.Text)
	catalogFn("has_schema_privilege", types.Bool, 2, 3, types.Text, types.Text, types.Text)
	catalogFn("has_database_privilege", types.Bool, 2, 3, types.Text, types.Text, types.Text)
	catalogFn("pg_is_in_recovery", types.Bool, 0, 0)
	catalogFn("pg_postmaster_start_time", types.TimestampTZ, 0, 0)
	catalogFn("pg_get_statisticsobjdef_columns", types.Text, 1, 1, types.Oid)
	catalogFn("pg_get_partkeydef", types.Text, 1, 1, types.Oid)
	catalogFn("pg_get_function_identity_arguments", types.Text, 1, 1, types.Oid)
	catalogFn("pg_get_function_result", types.Text, 1, 1, types.Oid)
	catalogFn("array_to_string_oids", types.Text, 1, 1, types.Text)
	catalogFn("pg_stat_get_numscans", types.Int8, 1, 1, types.Oid)
	catalogFn("basalt_stats", types.Text, 0, 0)
}

// toChar implements a useful subset of to_char.
func toChar(v types.Value, f string) (types.Value, error) {
	if v.K == types.KInt || v.K == types.KFloat || v.K == types.KNumeric {
		// Numbers: count 9/0 digits and an optional decimal point.
		dec := strings.Count(f[strings.IndexByte(f+".", '.'):], "9") + strings.Count(f[strings.IndexByte(f+".", '.'):], "0")
		d := v.AsDecimal().Round(int32(dec))
		s := d.String()
		width := len(strings.TrimSpace(f))
		return types.NewText(fmt.Sprintf("%*s", width, s)), nil
	}
	var t time.Time
	switch v.K {
	case types.KDate:
		t = types.TimeFromDate(v.I)
	case types.KTimestamp, types.KTimestampTZ:
		t = types.TimeFromTimestamp(v.I)
	default:
		return types.Null, pgerr.Unsupported("to_char is not supported for this type")
	}
	repl := []struct{ k, v string }{
		{"YYYY", fmt.Sprintf("%04d", t.Year())}, {"YY", fmt.Sprintf("%02d", t.Year()%100)},
		{"MM", fmt.Sprintf("%02d", t.Month())}, {"DD", fmt.Sprintf("%02d", t.Day())},
		{"HH24", fmt.Sprintf("%02d", t.Hour())}, {"HH12", fmt.Sprintf("%02d", (t.Hour()+11)%12+1)},
		{"HH", fmt.Sprintf("%02d", (t.Hour()+11)%12+1)}, {"MI", fmt.Sprintf("%02d", t.Minute())},
		{"SS", fmt.Sprintf("%02d", t.Second())}, {"Month", fmt.Sprintf("%-9s", t.Month().String())},
		{"Mon", t.Month().String()[:3]}, {"Day", fmt.Sprintf("%-9s", t.Weekday().String())},
		{"Dy", t.Weekday().String()[:3]}, {"MS", fmt.Sprintf("%03d", t.Nanosecond()/1e6)},
	}
	var b strings.Builder
	for i := 0; i < len(f); {
		matched := false
		for _, r := range repl {
			if strings.HasPrefix(f[i:], r.k) {
				b.WriteString(r.v)
				i += len(r.k)
				matched = true
				break
			}
		}
		if !matched {
			b.WriteByte(f[i])
			i++
		}
	}
	return types.NewText(b.String()), nil
}

// FunctionNames lists built-in scalar functions (for pg_proc emulation).
func FunctionNames() []string {
	out := make([]string, 0, len(builtins))
	for n := range builtins {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
