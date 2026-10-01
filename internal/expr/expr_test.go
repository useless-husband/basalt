package expr

import (
	"testing"

	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/types"
)

func c(v types.Value, t types.T) *Const { return &Const{V: v, T: t} }

var (
	tru  = c(types.True, types.Bool)
	fls  = c(types.False, types.Bool)
	null = c(types.Null, types.Bool)
)

func eval(t *testing.T, e Expr) types.Value {
	t.Helper()
	v, err := Eval(&Ctx{}, e)
	if err != nil {
		t.Fatalf("%s: %v", e, err)
	}
	return v
}

func show(v types.Value) string {
	if v.IsNull() {
		return "NULL"
	}
	return types.ToText(v, types.Text)
}

func TestThreeValuedLogic(t *testing.T) {
	cases := []struct {
		e    Expr
		want string
	}{
		{&And{Args: []Expr{tru, null}}, "NULL"},
		{&And{Args: []Expr{fls, null}}, "f"},
		{&Or{Args: []Expr{tru, null}}, "t"},
		{&Or{Args: []Expr{fls, null}}, "NULL"},
		{&Not{Arg: null}, "NULL"},
		{&IsNull{Arg: null}, "t"},
		{&IsBool{Arg: null, What: TestUnknown}, "t"},
		{&IsBool{Arg: null, What: TestTrue, Not: true}, "t"},
		{&Distinct{L: c(types.Null, types.Int4), R: c(types.Null, types.Int4)}, "f"},
		{&InList{Arg: c(types.NewInt(1), types.Int4), List: []Expr{c(types.NewInt(2), types.Int4), c(types.Null, types.Int4)}}, "NULL"},
		{&InList{Arg: c(types.NewInt(2), types.Int4), List: []Expr{c(types.NewInt(2), types.Int4), c(types.Null, types.Int4)}}, "t"},
		{&InList{Arg: c(types.NewInt(1), types.Int4), List: []Expr{c(types.NewInt(2), types.Int4), c(types.Null, types.Int4)}, Not: true}, "NULL"},
		{&Coalesce{Args: []Expr{c(types.Null, types.Int4), c(types.NewInt(3), types.Int4)}, T: types.Int4}, "3"},
		{&NullIf{A: c(types.NewInt(3), types.Int4), B: c(types.NewInt(3), types.Int4), T: types.Int4}, "NULL"},
		{&MinMax{Args: []Expr{c(types.NewInt(3), types.Int4), c(types.Null, types.Int4), c(types.NewInt(7), types.Int4)}, Greatest: true, T: types.Int4}, "7"},
	}
	for _, x := range cases {
		if got := show(eval(t, x.e)); got != x.want {
			t.Errorf("%s = %s, want %s", x.e, got, x.want)
		}
	}
}

func TestArithmeticOverflowAndDivision(t *testing.T) {
	big := c(types.NewInt(2147483647), types.Int4)
	one := c(types.NewInt(1), types.Int4)
	if _, err := Eval(&Ctx{}, &Call{Fn: arith("+", types.Int4), Args: []Expr{big, one}, T: types.Int4}); pgerr.Code(err) != pgerr.NumericValueOutOfRange {
		t.Fatalf("int4 overflow: %v", err)
	}
	if v := eval(t, &Call{Fn: arith("+", types.Int8), Args: []Expr{big, one}, T: types.Int8}); v.I != 2147483648 {
		t.Fatalf("int8 sum: %d", v.I)
	}
	if _, err := Eval(&Ctx{}, &Call{Fn: arith("/", types.Int4), Args: []Expr{one, c(types.NewInt(0), types.Int4)}, T: types.Int4}); pgerr.Code(err) != pgerr.DivisionByZero {
		t.Fatalf("division by zero: %v", err)
	}
	if v := eval(t, &Call{Fn: arith("/", types.Int4), Args: []Expr{c(types.NewInt(-7), types.Int4), c(types.NewInt(2), types.Int4)}, T: types.Int4}); v.I != -3 {
		t.Fatalf("-7/2 = %d, want -3 (truncation)", v.I)
	}
	if v := eval(t, &Call{Fn: arith("%", types.Int4), Args: []Expr{c(types.NewInt(-7), types.Int4), c(types.NewInt(2), types.Int4)}, T: types.Int4}); v.I != -1 {
		t.Fatalf("-7%%2 = %d, want -1", v.I)
	}
}

func TestCommonType(t *testing.T) {
	cases := []struct{ a, b, want types.T }{
		{types.Int2, types.Int4, types.Int4},
		{types.Int4, types.Int8, types.Int8},
		{types.Int8, types.Numeric, types.Numeric},
		{types.Numeric, types.Float8, types.Float8},
		{types.Unknown, types.Date, types.Date},
		{types.VarcharN(3), types.Text, types.Text},
		{types.Date, types.Timestamp, types.Timestamp},
	}
	for _, x := range cases {
		got, ok := CommonType(x.a, x.b)
		if !ok || got.Oid != x.want.Oid {
			t.Errorf("CommonType(%s, %s) = %s %v, want %s", x.a, x.b, got, ok, x.want)
		}
	}
	if _, ok := CommonType(types.Bool, types.Int4); ok {
		t.Error("bool and int have no common type")
	}
}

func TestLike(t *testing.T) {
	cases := []struct {
		s, p string
		want bool
	}{
		{"hello", "h%o", true}, {"hello", "h_llo", true}, {"hello", "%ell%", true}, {"hello", "hel", false},
		{"", "%", true}, {"a%b", `a\%b`, true}, {"axb", `a\%b`, false}, {"日本語", "日_語", true}, {"aaa", "%a%a%a%", true},
	}
	for _, x := range cases {
		got, err := Like(x.s, x.p, '\\', false)
		if err != nil || got != x.want {
			t.Errorf("Like(%q, %q) = %v %v", x.s, x.p, got, err)
		}
	}
	if ok, _ := Like("HeLLo", "hello", '\\', true); !ok {
		t.Error("ILIKE should ignore case")
	}
}

func TestAggregates(t *testing.T) {
	run := func(name string, argT types.T, vals ...types.Value) string {
		def, _ := LookupAgg(name)
		ret, _, err := def.Resolve([]types.T{argT})
		if err != nil {
			t.Fatal(err)
		}
		st := def.New(ret)
		for _, v := range vals {
			if v.IsNull() && !def.NullOK {
				continue
			}
			if err := st.Step([]types.Value{v}); err != nil {
				t.Fatal(err)
			}
		}
		r, err := st.Result()
		if err != nil {
			t.Fatal(err)
		}
		return show(r)
	}
	i := func(n int64) types.Value { return types.NewInt(n) }
	if got := run("sum", types.Int8, i(1<<62), i(1<<62), i(1<<62)); got != "13835058055282163712" {
		t.Errorf("sum(int8) should not overflow (numeric result): %s", got)
	}
	if got := run("avg", types.Int4, i(1), i(2)); got != "1.5000000000000000" {
		t.Errorf("avg(int) = %s", got)
	}
	if got := run("sum", types.Int4); got != "NULL" {
		t.Errorf("sum of nothing = %s", got)
	}
	if got := run("count", types.Int4, i(1), types.Null, i(3)); got != "2" {
		t.Errorf("count skips NULL: %s", got)
	}
	if got := run("max", types.Text, types.NewText("b"), types.NewText("a")); got != "b" {
		t.Errorf("max(text) = %s", got)
	}
}

func TestFunctions(t *testing.T) {
	call := func(name string, args ...Expr) string {
		var ts []types.T
		for _, a := range args {
			ts = append(ts, a.Type())
		}
		fn, ret, _, err := LookupFunc(name, ts)
		if err != nil {
			t.Fatal(err)
		}
		return show(eval(t, &Call{Fn: fn, Args: args, T: ret}))
	}
	txt := func(s string) Expr { return c(types.NewText(s), types.Text) }
	in := func(n int64) Expr { return c(types.NewInt(n), types.Int4) }
	cases := []struct{ got, want string }{
		{call("substr", txt("hello"), in(2), in(3)), "ell"},
		{call("substr", txt("hello"), in(0), in(2)), "h"},
		{call("upper", txt("abc")), "ABC"},
		{call("length", txt("日本")), "2"},
		{call("replace", txt("a-b-c"), txt("-"), txt("+")), "a+b+c"},
		{call("split_part", txt("a,b,c"), txt(","), in(2)), "b"},
		{call("lpad", txt("7"), in(3), txt("0")), "007"},
		{call("abs", in(-5)), "5"},
		{call("md5", txt("abc")), "900150983cd24fb0d6963f7d28e17f72"},
	}
	for i, x := range cases {
		if x.got != x.want {
			t.Errorf("case %d: got %q want %q", i, x.got, x.want)
		}
	}
}
