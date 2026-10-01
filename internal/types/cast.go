package types

import (
	"math"
	"strings"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// CanCast reports whether a cast from one type to another exists and
// whether it may be applied implicitly (in assignments and when resolving
// operators) or only explicitly.
func CanCast(from, to T) (ok, implicit bool) {
	if from.Oid == to.Oid {
		return true, true
	}
	fk, tk := from.Kind(), to.Kind()
	if from.Oid == OidUnknown {
		return true, true
	}
	switch {
	case to.Oid == OidText || to.Oid == OidVarchar || to.Oid == OidBpchar || to.Oid == OidName:
		// Everything has a text form; string-to-string casts are implicit.
		return true, fk == KText
	case fk == KText:
		return true, false
	case fk == KInt && tk == KInt:
		return true, true
	case fk == KInt && (tk == KFloat || tk == KNumeric):
		return true, true
	case fk == KNumeric && tk == KFloat:
		return true, true
	case fk == KFloat && tk == KFloat:
		return true, true
	case (fk == KFloat || fk == KNumeric) && (tk == KInt || tk == KNumeric):
		return true, false
	case fk == KInt && tk == KBool, fk == KBool && tk == KInt:
		return true, false
	case fk == KDate && (tk == KTimestamp || tk == KTimestampTZ):
		return true, true
	case fk == KTimestamp && tk == KTimestampTZ:
		return true, true
	case (fk == KTimestamp || fk == KTimestampTZ) && (tk == KDate || tk == KTimestamp):
		return true, false
	case fk == KArray && tk == KArray:
		return true, false
	}
	return false, false
}

func cannotCast(from, to T) error {
	return pgerr.New(pgerr.CannotCoerce, "cannot cast type %s to %s", from.String(), to.String())
}

// Cast converts v from type from to type to. explicit selects CAST
// semantics (truncation of varchar) over assignment semantics.
func Cast(v Value, from, to T, explicit bool) (Value, error) {
	if v.IsNull() {
		return Null, nil
	}
	if from.Oid == to.Oid {
		switch to.Kind() {
		case KText:
			return ApplyCharMod(v, to, explicit)
		case KNumeric:
			return ApplyNumericMod(v, to)
		}
		return v, nil
	}
	tk := to.Kind()
	// Anything to a string type: use the text output function.
	if tk == KText && to.Oid != OidChar {
		s := ToText(v, from)
		if v.K == KBool && to.Oid != OidText && explicit {
			// PostgreSQL casts boolean to text as "true"/"false".
			s = map[bool]string{true: "true", false: "false"}[v.Bool()]
		} else if v.K == KBool {
			s = map[bool]string{true: "true", false: "false"}[v.Bool()]
		}
		if from.Oid == OidBpchar {
			s = strings.TrimRight(s, " ")
		}
		return ApplyCharMod(NewText(s), to, explicit)
	}
	if to.Oid == OidChar {
		s := ToText(v, from)
		if len(s) > 1 {
			s = s[:1]
		}
		return NewText(s), nil
	}
	// From a string: use the input function of the target.
	if v.K == KText {
		s := v.S
		if from.Oid == OidBpchar {
			s = strings.TrimRight(s, " ")
		}
		return Parse(s, to)
	}
	switch tk {
	case KInt:
		switch v.K {
		case KInt:
			if err := CheckIntRange(v.I, to); err != nil {
				return Null, err
			}
			return v, nil
		case KFloat:
			f := v.Float()
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return Null, IntOutOfRange(to)
			}
			r := math.RoundToEven(f)
			if r < -9.223372036854775808e18 || r >= 9.223372036854775808e18 {
				return Null, IntOutOfRange(to)
			}
			i := int64(r)
			if err := CheckIntRange(i, to); err != nil {
				return Null, err
			}
			return NewInt(i), nil
		case KNumeric:
			i, ok := v.Num().Int64()
			if !ok {
				if v.Num().IsNaN() {
					return Null, pgerr.New(pgerr.FeatureNotSupported, "cannot convert NaN to %s", to.String())
				}
				return Null, IntOutOfRange(to)
			}
			if err := CheckIntRange(i, to); err != nil {
				return Null, err
			}
			return NewInt(i), nil
		case KBool:
			return NewInt(v.I), nil
		}
	case KFloat:
		switch v.K {
		case KInt, KNumeric, KFloat:
			f := v.AsFloat()
			if to.Oid == OidFloat4 {
				f32 := float32(f)
				if math.IsInf(float64(f32), 0) && !math.IsInf(f, 0) {
					return Null, pgerr.New(pgerr.NumericValueOutOfRange, "value out of range: overflow")
				}
				f = float64(f32)
			}
			return NewFloat(f), nil
		}
	case KNumeric:
		switch v.K {
		case KInt:
			return ApplyNumericMod(NewNumeric(DecimalFromInt(v.I)), to)
		case KFloat:
			d, err := DecimalFromFloat(v.Float())
			if err != nil {
				return Null, err
			}
			return ApplyNumericMod(NewNumeric(d), to)
		case KNumeric:
			return ApplyNumericMod(v, to)
		}
	case KBool:
		if v.K == KInt {
			return NewBool(v.I != 0), nil
		}
	case KDate:
		switch v.K {
		case KTimestamp, KTimestampTZ:
			d := v.I / usPerDay
			if v.I%usPerDay < 0 {
				d--
			}
			return NewDate(d), nil
		}
	case KTimestamp:
		switch v.K {
		case KDate:
			return NewTimestamp(v.I * usPerDay), nil
		case KTimestampTZ:
			return NewTimestamp(v.I), nil
		}
	case KTimestampTZ:
		switch v.K {
		case KDate:
			return NewTimestampTZ(v.I * usPerDay), nil
		case KTimestamp:
			return NewTimestampTZ(v.I), nil
		}
	case KArray:
		if v.K == KArray {
			a := v.Arr()
			et := to.Elem()
			out := make([]Value, len(a.Vals))
			for i, e := range a.Vals {
				c, err := Cast(e, a.Elem, et, explicit)
				if err != nil {
					return Null, err
				}
				out[i] = c
			}
			return NewArray(et, out), nil
		}
	}
	return Null, cannotCast(from, to)
}
