package types

import (
	"bytes"
	"math"
	"math/rand"
	"sort"
	"testing"
)

func mustDec(t *testing.T, s string) Decimal {
	t.Helper()
	d, err := ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return d
}

func TestDecimalParseFormat(t *testing.T) {
	cases := map[string]string{
		"0": "0", "1.50": "1.50", "-0.001": "-0.001", ".5": "0.5", "1e3": "1000",
		"1.5e-3": "0.0015", "-12345678901234567890.12": "-12345678901234567890.12", "NaN": "NaN",
	}
	for in, want := range cases {
		if got := mustDec(t, in).String(); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "1.2.3", "--1", "1e"} {
		if _, err := ParseDecimal(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestDecimalDivisionMatchesPostgres(t *testing.T) {
	// Expected values were produced by PostgreSQL 16.
	cases := []struct{ a, b, want string }{
		{"1", "3", "0.33333333333333333333"},
		{"10", "3", "3.3333333333333333"},
		{"3", "2", "1.5000000000000000"},
		{"1", "1", "1.00000000000000000000"},
		{"2", "3", "0.66666666666666666667"},
		{"-7", "2", "-3.5000000000000000"},
		{"100000", "7", "14285.714285714286"},
		{"1.50", "0.5", "3.0000000000000000"},
	}
	for _, c := range cases {
		q, err := mustDec(t, c.a).Div(mustDec(t, c.b))
		if err != nil {
			t.Fatal(err)
		}
		if q.String() != c.want {
			t.Errorf("%s/%s = %s, want %s", c.a, c.b, q, c.want)
		}
	}
	if _, err := mustDec(t, "1").Div(mustDec(t, "0")); err == nil {
		t.Error("division by zero: expected error")
	}
}

func TestDecimalRound(t *testing.T) {
	cases := []struct {
		in    string
		scale int32
		want  string
	}{
		{"2.5", 0, "3"}, {"-2.5", 0, "-3"}, {"1.2345", 2, "1.23"}, {"1.235", 2, "1.24"}, {"1234", -2, "1200"}, {"1.5", 3, "1.500"},
	}
	for _, c := range cases {
		if got := mustDec(t, c.in).Round(c.scale).String(); got != c.want {
			t.Errorf("round(%s,%d) = %s want %s", c.in, c.scale, got, c.want)
		}
	}
}

func TestFormatFloat(t *testing.T) {
	cases := []struct {
		f    float64
		bits int
		want string
	}{
		{0.1, 64, "0.1"}, {1e15, 64, "1e+15"}, {1e14, 64, "100000000000000"}, {0.0001, 64, "0.0001"},
		{0.00001, 64, "1e-05"}, {1.0 / 3, 64, "0.3333333333333333"}, {math.NaN(), 64, "NaN"},
		{math.Inf(-1), 64, "-Infinity"}, {100000, 32, "100000"}, {1e6, 32, "1e+06"}, {-2.5, 64, "-2.5"},
	}
	for _, c := range cases {
		if got := FormatFloat(c.f, c.bits); got != c.want {
			t.Errorf("FormatFloat(%v,%d) = %q want %q", c.f, c.bits, got, c.want)
		}
	}
}

func TestTimestampRoundTrip(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2024-02-29 13:45:01.5", "2024-02-29 13:45:01.5"},
		{"1999-12-31 23:59:59", "1999-12-31 23:59:59"},
		{"2000-01-01T00:00:00.000001", "2000-01-01 00:00:00.000001"},
	}
	for _, c := range cases {
		us, err := ParseTimestamp(c.in, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := FormatTimestamp(us, false); got != c.want {
			t.Errorf("%q -> %q want %q", c.in, got, c.want)
		}
	}
	us, err := ParseTimestamp("2024-01-01 10:00:00+02", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatTimestamp(us, true); got != "2024-01-01 08:00:00+00" {
		t.Errorf("timestamptz: got %q", got)
	}
	if _, err := ParseDate("2023-02-29"); err == nil {
		t.Error("2023-02-29 should be rejected")
	}
	d, _ := ParseDate("1970-01-01")
	if FormatDate(d) != "1970-01-01" || d != -10957 {
		t.Errorf("date epoch: %d %s", d, FormatDate(d))
	}
}

func TestIntervalParseFormat(t *testing.T) {
	cases := map[string]string{
		"1 day":                         "1 day",
		"2 hours 30 minutes":            "02:30:00",
		"1 year 2 mons 3 days 04:05:06": "1 year 2 mons 3 days 04:05:06",
		"-01:00:00":                     "-01:00:00",
		"3 days ago":                    "-3 days",
	}
	for in, want := range cases {
		iv, err := ParseInterval(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if iv.String() != want {
			t.Errorf("%q -> %q want %q", in, iv.String(), want)
		}
	}
}

func randomValue(r *rand.Rand, k Kind) Value {
	if r.Intn(10) == 0 {
		return Null
	}
	switch k {
	case KInt:
		switch r.Intn(4) {
		case 0:
			return NewInt(r.Int63() - r.Int63())
		case 1:
			return NewInt(math.MinInt64 + int64(r.Intn(3)))
		}
		return NewInt(int64(r.Intn(200) - 100))
	case KFloat:
		switch r.Intn(6) {
		case 0:
			return NewFloat(math.Inf(1 - 2*r.Intn(2)))
		case 1:
			return NewFloat(math.Copysign(0, -1))
		}
		return NewFloat(r.NormFloat64() * math.Pow(10, float64(r.Intn(20)-10)))
	case KNumeric:
		scale := r.Intn(5)
		s := []byte{}
		if r.Intn(2) == 0 {
			s = append(s, '-')
		}
		for i := 0; i < 1+r.Intn(12); i++ {
			s = append(s, byte('0'+r.Intn(10)))
		}
		if scale > 0 {
			s = append(s, '.')
			for i := 0; i < scale; i++ {
				s = append(s, byte('0'+r.Intn(10)))
			}
		}
		d, _ := ParseDecimal(string(s))
		return NewNumeric(d)
	case KText:
		b := make([]byte, r.Intn(6))
		for i := range b {
			b[i] = []byte{0, 1, 'a', 'b', 0xff}[r.Intn(5)]
		}
		return NewText(string(b))
	}
	return Null
}

// TestKeyEncodingPreservesOrder checks the central property of index keys:
// byte order of encodings equals SQL order of values, and equal values have
// equal encodings.
func TestKeyEncodingPreservesOrder(t *testing.T) {
	seed := int64(20261001)
	r := rand.New(rand.NewSource(seed))
	for _, k := range []Kind{KInt, KFloat, KNumeric, KText} {
		for i := 0; i < 5000; i++ {
			a, b := randomValue(r, k), randomValue(r, k)
			ka, kb := EncodeKey(nil, a), EncodeKey(nil, b)
			want := CompareNullsLast(a, b)
			got := bytes.Compare(ka, kb)
			if want != got {
				t.Fatalf("seed %d kind %v: compare(%v,%v)=%d but key order %d", seed, k, ToText(a, Text), ToText(b, Text), want, got)
			}
		}
	}
}

func TestCompositeKeyOrder(t *testing.T) {
	seed := int64(7)
	r := rand.New(rand.NewSource(seed))
	type row struct {
		vals []Value
		key  []byte
	}
	rows := make([]row, 2000)
	for i := range rows {
		vals := []Value{randomValue(r, KText), randomValue(r, KInt)}
		rows[i] = row{vals, HashKey(nil, vals)}
	}
	sort.Slice(rows, func(i, j int) bool { return bytes.Compare(rows[i].key, rows[j].key) < 0 })
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1].vals, rows[i].vals
		c := CompareNullsLast(a[0], b[0])
		if c == 0 {
			c = CompareNullsLast(a[1], b[1])
		}
		if c > 0 {
			t.Fatalf("seed %d: rows out of order at %d", seed, i)
		}
	}
}

func TestRowEncodingRoundTrip(t *testing.T) {
	iv, _ := ParseInterval("1 day 01:00:00")
	vals := []Value{
		Null, NewBool(true), NewInt(-42), NewFloat(3.25), NewNumeric(mustDec(t, "-1.050")),
		NewText("héllo"), NewBytea([]byte{0, 1, 2}), NewDate(123), NewTimestamp(-5), NewTimestampTZ(99),
		NewInterval(iv), NewArray(Int4, []Value{NewInt(1), Null}),
	}
	enc := EncodeRow(nil, vals)
	got, err := DecodeRow(enc, len(vals)+2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(vals)+2 || !got[len(vals)].IsNull() {
		t.Fatalf("missing trailing columns should be NULL")
	}
	for i := range vals {
		if !Equal(vals[i], got[i]) || vals[i].K != got[i].K {
			t.Errorf("column %d: got %v want %v", i, got[i], vals[i])
		}
	}
	if got[4].Num().String() != "-1.050" {
		t.Errorf("numeric scale lost: %s", got[4].Num())
	}
	if _, err := DecodeRow(enc[:len(enc)-3], len(vals)); err == nil {
		t.Error("truncated row should fail to decode")
	}
}

func TestNumericBinaryRoundTrip(t *testing.T) {
	for _, s := range []string{"0", "1", "-1", "12345.6789", "0.0001", "100000000", "-0.5", "1.50", "99999999999999999999.999"} {
		d := mustDec(t, s)
		b := appendNumericBinary(nil, d)
		back, err := decodeNumericBinary(b)
		if err != nil {
			t.Fatal(err)
		}
		if back.String() != d.String() {
			t.Errorf("%s -> %s", s, back)
		}
	}
}

func TestCasts(t *testing.T) {
	v, err := Cast(NewText("12"), Text, Int4, true)
	if err != nil || v.I != 12 {
		t.Fatalf("text->int4: %v %v", v, err)
	}
	if _, err := Cast(NewInt(1<<40), Int8, Int4, true); err == nil {
		t.Error("int8->int4 overflow should fail")
	}
	v, _ = Cast(NewFloat(2.5), Float8, Int4, true)
	if v.I != 2 {
		t.Errorf("2.5::int = %d, want 2 (round half to even)", v.I)
	}
	v, err = Cast(NewText("abcdef"), Text, VarcharN(3), true)
	if err != nil || v.S != "abc" {
		t.Errorf("explicit varchar(3) truncates: %v %v", v, err)
	}
	if _, err := Cast(NewText("abcdef"), Text, VarcharN(3), false); err == nil {
		t.Error("assignment to varchar(3) should fail")
	}
	v, _ = Cast(NewText("ab"), Text, BpcharN(4), false)
	if v.S != "ab  " {
		t.Errorf("char(4) pads: %q", v.S)
	}
	v, err = Cast(NewNumeric(mustDec(t, "123.456")), Numeric, NumericPS(5, 2), false)
	if err != nil || v.Num().String() != "123.46" {
		t.Errorf("numeric(5,2): %v %v", v, err)
	}
	if _, err := Cast(NewNumeric(mustDec(t, "1234.5")), Numeric, NumericPS(5, 2), false); err == nil {
		t.Error("numeric(5,2) overflow should fail")
	}
	v, _ = Cast(True, Bool, Text, true)
	if v.S != "true" {
		t.Errorf("bool::text = %q", v.S)
	}
}

func TestArrayText(t *testing.T) {
	v, err := Parse(`{1,NULL,3}`, T{OidInt4Array, -1})
	if err != nil {
		t.Fatal(err)
	}
	if got := ToText(v, T{OidInt4Array, -1}); got != "{1,NULL,3}" {
		t.Errorf("got %s", got)
	}
	v, _ = Parse(`{"a b","c\"d",e}`, T{OidTextArray, -1})
	if got := ToText(v, T{OidTextArray, -1}); got != `{"a b","c\"d",e}` {
		t.Errorf("got %s", got)
	}
}
