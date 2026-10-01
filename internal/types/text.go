package types

import (
	"encoding/hex"
	"math"
	"strconv"
	"strings"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// FormatFloat formats a float the way PostgreSQL 12+ does with the default
// extra_float_digits: the shortest string that reads back to the same
// value, in fixed notation for decimal exponents in [-4, 15) (float8) or
// [-4, 6) (float4) and in exponential notation otherwise.
func FormatFloat(f float64, bits int) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0"
		}
		return "0"
	}
	e := strconv.FormatFloat(f, 'e', -1, bits)
	i := strings.LastIndexByte(e, 'e')
	exp, _ := strconv.Atoi(e[i+1:])
	limit := 15
	if bits == 32 {
		limit = 6
	}
	if exp >= -4 && exp < limit {
		return strconv.FormatFloat(f, 'f', -1, bits)
	}
	return e
}

// ToText formats a non-NULL value of type t in PostgreSQL's text format.
func ToText(v Value, t T) string {
	switch v.K {
	case KNull:
		return ""
	case KBool:
		if v.Bool() {
			return "t"
		}
		return "f"
	case KInt:
		return strconv.FormatInt(v.I, 10)
	case KFloat:
		if t.Oid == OidFloat4 {
			return FormatFloat(float64(float32(v.Float())), 32)
		}
		return FormatFloat(v.Float(), 64)
	case KNumeric:
		return v.Num().String()
	case KText:
		return v.S
	case KBytea:
		return `\x` + hex.EncodeToString([]byte(v.S))
	case KDate:
		return FormatDate(v.I)
	case KTimestamp:
		return FormatTimestamp(v.I, false)
	case KTimestampTZ:
		return FormatTimestamp(v.I, true)
	case KInterval:
		return v.Interval().String()
	case KArray:
		return formatArray(v.Arr(), t)
	}
	return ""
}

func formatArray(a *Array, t T) string {
	elem := a.Elem
	if t.IsArray() {
		elem = t.Elem()
	}
	sep := ","
	if t.Oid == OidInt2Vector || t.Oid == OidOidVector {
		parts := make([]string, len(a.Vals))
		for i, e := range a.Vals {
			parts[i] = ToText(e, elem)
		}
		return strings.Join(parts, " ")
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, e := range a.Vals {
		if i > 0 {
			b.WriteString(sep)
		}
		if e.IsNull() {
			b.WriteString("NULL")
			continue
		}
		s := ToText(e, elem)
		if needsArrayQuote(s) {
			b.WriteByte('"')
			for j := 0; j < len(s); j++ {
				if s[j] == '"' || s[j] == '\\' {
					b.WriteByte('\\')
				}
				b.WriteByte(s[j])
			}
			b.WriteByte('"')
		} else {
			b.WriteString(s)
		}
	}
	b.WriteByte('}')
	return b.String()
}

func needsArrayQuote(s string) bool {
	if s == "" || strings.EqualFold(s, "null") {
		return true
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\', '{', '}', ',', ' ', '\t', '\n':
			return true
		}
	}
	return false
}

func invalidText(t T, s string) error {
	return pgerr.New(pgerr.InvalidTextRepresentation, "invalid input syntax for type %s: \"%s\"", t.String(), s)
}

// ParseBool parses PostgreSQL boolean input.
func ParseBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "t", "true", "y", "yes", "on", "1":
		return true, true
	case "f", "false", "n", "no", "off", "0":
		return false, true
	}
	return false, false
}

// IntRange returns the bounds of an integer type.
func IntRange(t T) (int64, int64) {
	switch t.Oid {
	case OidInt2:
		return math.MinInt16, math.MaxInt16
	case OidInt4:
		return math.MinInt32, math.MaxInt32
	case OidOid, OidRegclass, OidRegtype, OidRegproc, OidRegnamespace, OidRegrole:
		return 0, math.MaxUint32
	}
	return math.MinInt64, math.MaxInt64
}

// IntOutOfRange returns the error PostgreSQL reports for overflow of t.
func IntOutOfRange(t T) error {
	switch t.Oid {
	case OidInt2:
		return pgerr.New(pgerr.NumericValueOutOfRange, "smallint out of range")
	case OidInt4:
		return pgerr.New(pgerr.NumericValueOutOfRange, "integer out of range")
	}
	return pgerr.New(pgerr.NumericValueOutOfRange, "bigint out of range")
}

// CheckIntRange verifies that i fits in the integer type t.
func CheckIntRange(i int64, t T) error {
	lo, hi := IntRange(t)
	if i < lo || i > hi {
		return IntOutOfRange(t)
	}
	return nil
}

// Parse converts PostgreSQL text input to a value of type t.
func Parse(s string, t T) (Value, error) {
	switch t.Kind() {
	case KBool:
		b, ok := ParseBool(s)
		if !ok {
			return Null, pgerr.New(pgerr.InvalidTextRepresentation, "invalid input syntax for type boolean: \"%s\"", s)
		}
		return NewBool(b), nil
	case KInt:
		str := strings.TrimSpace(s)
		i, err := strconv.ParseInt(str, 10, 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				return Null, pgerr.New(pgerr.NumericValueOutOfRange, "value \"%s\" is out of range for type %s", s, t.String())
			}
			return Null, invalidText(t, s)
		}
		if err := CheckIntRange(i, t); err != nil {
			return Null, pgerr.New(pgerr.NumericValueOutOfRange, "value \"%s\" is out of range for type %s", s, t.String())
		}
		return NewInt(i), nil
	case KFloat:
		str := strings.TrimSpace(s)
		var f float64
		switch strings.ToLower(str) {
		case "nan":
			f = math.NaN()
		case "infinity", "+infinity", "inf", "+inf":
			f = math.Inf(1)
		case "-infinity", "-inf":
			f = math.Inf(-1)
		default:
			var err error
			bits := 64
			if t.Oid == OidFloat4 {
				bits = 32
			}
			f, err = strconv.ParseFloat(str, bits)
			if err != nil {
				if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
					return Null, pgerr.New(pgerr.NumericValueOutOfRange, "\"%s\" is out of range for type %s", s, t.String())
				}
				return Null, invalidText(t, s)
			}
		}
		return NewFloat(f), nil
	case KNumeric:
		d, err := ParseDecimal(s)
		if err != nil {
			return Null, err
		}
		return ApplyNumericMod(NewNumeric(d), t)
	case KText:
		return ApplyCharMod(NewText(s), t, false)
	case KBytea:
		b, err := ParseBytea(s)
		if err != nil {
			return Null, err
		}
		return NewBytea(b), nil
	case KDate:
		d, err := ParseDate(s)
		if err != nil {
			return Null, err
		}
		return NewDate(d), nil
	case KTimestamp:
		ts, err := ParseTimestamp(s, false)
		if err != nil {
			return Null, err
		}
		return NewTimestamp(ts), nil
	case KTimestampTZ:
		ts, err := ParseTimestamp(s, true)
		if err != nil {
			return Null, err
		}
		return NewTimestampTZ(ts), nil
	case KInterval:
		iv, err := ParseInterval(s)
		if err != nil {
			return Null, err
		}
		return NewInterval(iv), nil
	case KArray:
		return ParseArray(s, t)
	}
	return Null, pgerr.Unsupported("cannot parse input for type %s", t.String())
}

// ParseBytea accepts the hex format (\x0a0b) and the escape format.
func ParseBytea(s string) ([]byte, error) {
	if strings.HasPrefix(s, `\x`) || strings.HasPrefix(s, `\X`) {
		b, err := hex.DecodeString(s[2:])
		if err != nil {
			return nil, pgerr.New(pgerr.InvalidTextRepresentation, "invalid hexadecimal data")
		}
		return b, nil
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out = append(out, s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\\' {
			out = append(out, '\\')
			i++
			continue
		}
		if i+3 < len(s) {
			n, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
			if err == nil {
				out = append(out, byte(n))
				i += 3
				continue
			}
		}
		return nil, pgerr.New(pgerr.InvalidTextRepresentation, "invalid input syntax for type bytea")
	}
	return out, nil
}

// ParseArray parses a one-dimensional array literal such as {1,2,NULL}.
func ParseArray(s string, t T) (Value, error) {
	elem := t.Elem()
	if t.Oid == OidInt2Vector || t.Oid == OidOidVector {
		var vals []Value
		for _, f := range strings.Fields(s) {
			v, err := Parse(f, elem)
			if err != nil {
				return Null, err
			}
			vals = append(vals, v)
		}
		return NewArray(elem, vals), nil
	}
	str := strings.TrimSpace(s)
	if len(str) < 2 || str[0] != '{' || str[len(str)-1] != '}' {
		return Null, pgerr.New(pgerr.InvalidTextRepresentation, "malformed array literal: \"%s\"", s)
	}
	body := str[1 : len(str)-1]
	var vals []Value
	if strings.TrimSpace(body) == "" {
		return NewArray(elem, vals), nil
	}
	i := 0
	for {
		for i < len(body) && body[i] == ' ' {
			i++
		}
		var item string
		quoted := false
		if i < len(body) && body[i] == '"' {
			quoted = true
			i++
			var b strings.Builder
			for i < len(body) && body[i] != '"' {
				if body[i] == '\\' && i+1 < len(body) {
					i++
				}
				b.WriteByte(body[i])
				i++
			}
			i++ // closing quote
			item = b.String()
		} else {
			j := i
			for j < len(body) && body[j] != ',' {
				j++
			}
			item = strings.TrimSpace(body[i:j])
			i = j
		}
		if !quoted && strings.EqualFold(item, "null") {
			vals = append(vals, Null)
		} else {
			v, err := Parse(item, elem)
			if err != nil {
				return Null, err
			}
			vals = append(vals, v)
		}
		for i < len(body) && body[i] == ' ' {
			i++
		}
		if i >= len(body) {
			break
		}
		if body[i] != ',' {
			return Null, pgerr.New(pgerr.InvalidTextRepresentation, "malformed array literal: \"%s\"", s)
		}
		i++
	}
	return NewArray(elem, vals), nil
}

// ApplyCharMod enforces varchar(n) and char(n). An explicit cast truncates;
// an assignment of a longer value is an error, except that trailing spaces
// may be cut. char(n) pads with spaces.
func ApplyCharMod(v Value, t T, explicit bool) (Value, error) {
	n := t.CharLen()
	if v.IsNull() || n < 0 {
		return v, nil
	}
	s := v.S
	if l := len([]rune(s)); l > n {
		r := []rune(s)
		if !explicit && strings.TrimRight(string(r[n:]), " ") != "" {
			return Null, pgerr.New(pgerr.StringDataRightTruncation, "value too long for type %s", t.String())
		}
		s = string(r[:n])
	} else if t.Oid == OidBpchar && l < n {
		s += strings.Repeat(" ", n-l)
	}
	return NewText(s), nil
}

// ApplyNumericMod rounds a numeric value to the declared scale and checks
// the declared precision.
func ApplyNumericMod(v Value, t T) (Value, error) {
	p, s, ok := t.NumericPrecScale()
	if !ok || v.IsNull() || v.Num().IsNaN() {
		return v, nil
	}
	d := v.Num().Round(int32(s))
	if d.IntDigits() > p-s {
		return Null, pgerr.New(pgerr.NumericValueOutOfRange, "numeric field overflow").
			WithDetail("A field with precision %d, scale %d must round to an absolute value less than 10^%d.", p, s, p-s)
	}
	return NewNumeric(d), nil
}
