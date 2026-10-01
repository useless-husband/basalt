package types

import (
	"bytes"
	"math"
	"strings"
)

// Kind is the runtime representation of a value. Several SQL types share a
// kind: int2, int4, int8 and oid are all KInt, for example. The static SQL
// type of an expression is tracked by the planner, not by the value.
type Kind uint8

const (
	KNull Kind = iota
	KBool
	KInt
	KFloat
	KNumeric
	KText
	KBytea
	KDate
	KTimestamp
	KTimestampTZ
	KInterval
	KArray
)

func (k Kind) String() string {
	switch k {
	case KNull:
		return "null"
	case KBool:
		return "bool"
	case KInt:
		return "int"
	case KFloat:
		return "float"
	case KNumeric:
		return "numeric"
	case KText:
		return "text"
	case KBytea:
		return "bytea"
	case KDate:
		return "date"
	case KTimestamp:
		return "timestamp"
	case KTimestampTZ:
		return "timestamptz"
	case KInterval:
		return "interval"
	case KArray:
		return "array"
	}
	return "?"
}

// Value is a single SQL value. The zero Value is NULL.
//
// I holds integers, booleans (0/1), float bits, dates (days since
// 2000-01-01) and timestamps (microseconds since 2000-01-01). S holds text
// and bytea. X holds the rarer variable-size payloads: Decimal, Interval and
// *Array.
type Value struct {
	K Kind
	I int64
	S string
	X any
}

// Array is a one-dimensional SQL array.
type Array struct {
	Elem T
	Vals []Value
}

// Null is the SQL NULL.
var Null = Value{}

// NewBool returns a boolean value.
func NewBool(b bool) Value {
	if b {
		return Value{K: KBool, I: 1}
	}
	return Value{K: KBool}
}

// True and False are the boolean constants.
var (
	True  = NewBool(true)
	False = NewBool(false)
)

// NewInt returns an integer value.
func NewInt(i int64) Value { return Value{K: KInt, I: i} }

// NewFloat returns a floating point value.
func NewFloat(f float64) Value { return Value{K: KFloat, I: int64(math.Float64bits(f))} }

// NewText returns a text value.
func NewText(s string) Value { return Value{K: KText, S: s} }

// NewBytea returns a bytea value.
func NewBytea(b []byte) Value { return Value{K: KBytea, S: string(b)} }

// NewNumeric returns a numeric value.
func NewNumeric(d Decimal) Value { return Value{K: KNumeric, X: d} }

// NewDate returns a date value from days since 2000-01-01.
func NewDate(days int64) Value { return Value{K: KDate, I: days} }

// NewTimestamp returns a timestamp from microseconds since 2000-01-01.
func NewTimestamp(us int64) Value { return Value{K: KTimestamp, I: us} }

// NewTimestampTZ returns a timestamptz from microseconds since 2000-01-01 UTC.
func NewTimestampTZ(us int64) Value { return Value{K: KTimestampTZ, I: us} }

// NewInterval returns an interval value.
func NewInterval(iv Interval) Value { return Value{K: KInterval, X: iv} }

// NewArray returns an array value.
func NewArray(elem T, vals []Value) Value {
	return Value{K: KArray, X: &Array{Elem: elem, Vals: vals}}
}

// IsNull reports whether v is NULL.
func (v Value) IsNull() bool { return v.K == KNull }

// Bool returns the boolean payload.
func (v Value) Bool() bool { return v.I != 0 }

// Int returns the integer payload.
func (v Value) Int() int64 { return v.I }

// Float returns the float payload.
func (v Value) Float() float64 { return math.Float64frombits(uint64(v.I)) }

// Str returns the text or bytea payload.
func (v Value) Str() string { return v.S }

// Num returns the numeric payload.
func (v Value) Num() Decimal { return v.X.(Decimal) }

// Interval returns the interval payload.
func (v Value) Interval() Interval { return v.X.(Interval) }

// Arr returns the array payload.
func (v Value) Arr() *Array { return v.X.(*Array) }

// AsFloat converts any numeric kind to float64.
func (v Value) AsFloat() float64 {
	switch v.K {
	case KInt:
		return float64(v.I)
	case KFloat:
		return v.Float()
	case KNumeric:
		return v.Num().Float64()
	case KBool:
		return float64(v.I)
	}
	return 0
}

// AsDecimal converts an integer or numeric value to Decimal.
func (v Value) AsDecimal() Decimal {
	switch v.K {
	case KInt:
		return DecimalFromInt(v.I)
	case KNumeric:
		return v.Num()
	case KFloat:
		d, _ := DecimalFromFloat(v.Float())
		return d
	}
	return Decimal{}
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// CompareFloat orders floats the way PostgreSQL does: NaN is equal to
// itself and greater than every other value.
func CompareFloat(a, b float64) int {
	an, bn := math.IsNaN(a), math.IsNaN(b)
	switch {
	case an && bn:
		return 0
	case an:
		return 1
	case bn:
		return -1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Compare orders two non-NULL values. Values of different numeric kinds are
// compared numerically; dates compare with timestamps by promoting the date.
// Text compares byte-wise (the "C" collation).
func Compare(a, b Value) int {
	if a.K == b.K {
		switch a.K {
		case KBool, KInt, KDate, KTimestamp, KTimestampTZ:
			return cmpInt(a.I, b.I)
		case KFloat:
			return CompareFloat(a.Float(), b.Float())
		case KNumeric:
			return a.Num().Cmp(b.Num())
		case KText, KBytea:
			return strings.Compare(a.S, b.S)
		case KInterval:
			return a.Interval().Cmp(b.Interval())
		case KArray:
			return compareArrays(a.Arr(), b.Arr())
		case KNull:
			return 0
		}
	}
	switch {
	case a.K == KInt && b.K == KFloat, a.K == KFloat && b.K == KInt, a.K == KFloat && b.K == KNumeric, a.K == KNumeric && b.K == KFloat:
		return CompareFloat(a.AsFloat(), b.AsFloat())
	case a.K == KInt && b.K == KNumeric, a.K == KNumeric && b.K == KInt:
		return a.AsDecimal().Cmp(b.AsDecimal())
	case a.K == KDate && (b.K == KTimestamp || b.K == KTimestampTZ):
		return cmpInt(a.I*usPerDay, b.I)
	case (a.K == KTimestamp || a.K == KTimestampTZ) && b.K == KDate:
		return cmpInt(a.I, b.I*usPerDay)
	case a.K == KTimestamp && b.K == KTimestampTZ, a.K == KTimestampTZ && b.K == KTimestamp:
		return cmpInt(a.I, b.I)
	case a.K == KText && b.K == KBytea, a.K == KBytea && b.K == KText:
		return strings.Compare(a.S, b.S)
	case a.K == KBool && b.K == KInt, a.K == KInt && b.K == KBool:
		return cmpInt(a.I, b.I)
	}
	// Incomparable kinds: order by kind so that sorting is at least total.
	return cmpInt(int64(a.K), int64(b.K))
}

func compareArrays(a, b *Array) int {
	n := len(a.Vals)
	if len(b.Vals) < n {
		n = len(b.Vals)
	}
	for i := 0; i < n; i++ {
		x, y := a.Vals[i], b.Vals[i]
		switch {
		case x.IsNull() && y.IsNull():
			continue
		case x.IsNull():
			return 1
		case y.IsNull():
			return -1
		}
		if c := Compare(x, y); c != 0 {
			return c
		}
	}
	return cmpInt(int64(len(a.Vals)), int64(len(b.Vals)))
}

// CompareNullsLast orders values with NULL after every non-NULL value, the
// PostgreSQL default for ascending sorts.
func CompareNullsLast(a, b Value) int {
	switch {
	case a.IsNull() && b.IsNull():
		return 0
	case a.IsNull():
		return 1
	case b.IsNull():
		return -1
	}
	return Compare(a, b)
}

// Equal reports whether two values are equal, treating NULL as equal to
// NULL (the semantics of IS NOT DISTINCT FROM, GROUP BY and DISTINCT).
func Equal(a, b Value) bool {
	if a.IsNull() || b.IsNull() {
		return a.IsNull() && b.IsNull()
	}
	return Compare(a, b) == 0
}

// RowsEqual compares two rows with Equal.
func RowsEqual(a, b []Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// HashKey appends a byte string to dst that is equal for two rows exactly
// when the rows are Equal column by column (for values of the same kinds).
// It is used by hash joins, hash aggregation and DISTINCT.
func HashKey(dst []byte, vals []Value) []byte {
	for _, v := range vals {
		dst = EncodeKey(dst, v)
	}
	return dst
}

// Clone returns a copy of v that does not share mutable memory. Values are
// immutable once built, so this only matters for arrays.
func (v Value) Clone() Value {
	if v.K == KArray {
		a := v.Arr()
		vals := make([]Value, len(a.Vals))
		copy(vals, a.Vals)
		return NewArray(a.Elem, vals)
	}
	return v
}

// BytesEqual is a helper for tests.
func BytesEqual(a, b []byte) bool { return bytes.Equal(a, b) }
