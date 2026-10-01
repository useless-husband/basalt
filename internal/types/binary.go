package types

import (
	"encoding/binary"
	"math"
	"math/big"
	"strings"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// HasBinary reports whether basalt implements the binary wire format of t.
func HasBinary(t T) bool {
	switch t.Kind() {
	case KBool, KInt, KFloat, KNumeric, KText, KBytea, KDate, KTimestamp, KTimestampTZ, KInterval:
		return true
	case KArray:
		return t.IsArray() && t.Oid != OidInt2Vector && t.Oid != OidOidVector && HasBinary(t.Elem())
	}
	return false
}

// AppendBinary appends the binary wire encoding of a non-NULL value.
func AppendBinary(dst []byte, v Value, t T) ([]byte, error) {
	switch t.Kind() {
	case KBool:
		if v.Bool() {
			return append(dst, 1), nil
		}
		return append(dst, 0), nil
	case KInt:
		switch t.Oid {
		case OidInt2:
			return binary.BigEndian.AppendUint16(dst, uint16(v.I)), nil
		case OidInt8:
			return binary.BigEndian.AppendUint64(dst, uint64(v.I)), nil
		}
		return binary.BigEndian.AppendUint32(dst, uint32(v.I)), nil
	case KFloat:
		if t.Oid == OidFloat4 {
			return binary.BigEndian.AppendUint32(dst, math.Float32bits(float32(v.Float()))), nil
		}
		return binary.BigEndian.AppendUint64(dst, math.Float64bits(v.Float())), nil
	case KNumeric:
		return appendNumericBinary(dst, v.AsDecimal()), nil
	case KText, KBytea:
		return append(dst, v.S...), nil
	case KDate:
		return binary.BigEndian.AppendUint32(dst, uint32(int32(v.I))), nil
	case KTimestamp, KTimestampTZ:
		return binary.BigEndian.AppendUint64(dst, uint64(v.I)), nil
	case KInterval:
		iv := v.Interval()
		dst = binary.BigEndian.AppendUint64(dst, uint64(iv.Micros))
		dst = binary.BigEndian.AppendUint32(dst, uint32(iv.Days))
		return binary.BigEndian.AppendUint32(dst, uint32(iv.Months)), nil
	case KArray:
		a := v.Arr()
		et := t.Elem()
		hasNull := int32(0)
		for _, e := range a.Vals {
			if e.IsNull() {
				hasNull = 1
			}
		}
		if len(a.Vals) == 0 {
			dst = binary.BigEndian.AppendUint32(dst, 0)
			dst = binary.BigEndian.AppendUint32(dst, 0)
			return binary.BigEndian.AppendUint32(dst, et.Oid), nil
		}
		dst = binary.BigEndian.AppendUint32(dst, 1)
		dst = binary.BigEndian.AppendUint32(dst, uint32(hasNull))
		dst = binary.BigEndian.AppendUint32(dst, et.Oid)
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(a.Vals)))
		dst = binary.BigEndian.AppendUint32(dst, 1)
		for _, e := range a.Vals {
			if e.IsNull() {
				dst = binary.BigEndian.AppendUint32(dst, 0xFFFFFFFF)
				continue
			}
			lenPos := len(dst)
			dst = append(dst, 0, 0, 0, 0)
			var err error
			dst, err = AppendBinary(dst, e, et)
			if err != nil {
				return nil, err
			}
			binary.BigEndian.PutUint32(dst[lenPos:], uint32(len(dst)-lenPos-4))
		}
		return dst, nil
	}
	return nil, pgerr.Unsupported("binary output is not supported for type %s", t.String())
}

func appendNumericBinary(dst []byte, d Decimal) []byte {
	if d.IsNaN() {
		dst = binary.BigEndian.AppendUint16(dst, 0)
		dst = binary.BigEndian.AppendUint16(dst, 0)
		dst = binary.BigEndian.AppendUint16(dst, 0xC000)
		return binary.BigEndian.AppendUint16(dst, 0)
	}
	sign := uint16(0)
	c := new(big.Int).Set(d.Coef())
	if c.Sign() < 0 {
		sign = 0x4000
		c.Neg(c)
	}
	dscale := int(d.Scale())
	// Pad the fractional part to a multiple of 4 digits.
	pad := (4 - dscale%4) % 4
	if pad > 0 {
		c.Mul(c, pow10(int32(pad)))
	}
	fracGroups := (dscale + pad) / 4
	s := c.String()
	if c.Sign() == 0 {
		s = ""
	}
	// Left-pad to a multiple of 4.
	if r := len(s) % 4; r != 0 {
		s = strings.Repeat("0", 4-r) + s
	}
	groups := make([]uint16, 0, len(s)/4)
	for i := 0; i < len(s); i += 4 {
		var g uint16
		for j := 0; j < 4; j++ {
			g = g*10 + uint16(s[i+j]-'0')
		}
		groups = append(groups, g)
	}
	weight := len(groups) - fracGroups - 1
	// Strip leading and trailing zero groups.
	for len(groups) > 0 && groups[0] == 0 {
		groups = groups[1:]
		weight--
	}
	for len(groups) > 0 && groups[len(groups)-1] == 0 {
		groups = groups[:len(groups)-1]
	}
	if len(groups) == 0 {
		weight = 0
	}
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(groups)))
	dst = binary.BigEndian.AppendUint16(dst, uint16(int16(weight)))
	dst = binary.BigEndian.AppendUint16(dst, sign)
	dst = binary.BigEndian.AppendUint16(dst, uint16(dscale))
	for _, g := range groups {
		dst = binary.BigEndian.AppendUint16(dst, g)
	}
	return dst
}

func badBinary(t T) error {
	return pgerr.New(pgerr.InvalidBinaryRepresentation, "incorrect binary data format for type %s", t.String())
}

// DecodeBinary decodes a binary wire value of type t.
func DecodeBinary(b []byte, t T) (Value, error) {
	switch t.Kind() {
	case KBool:
		if len(b) != 1 {
			return Null, badBinary(t)
		}
		return NewBool(b[0] != 0), nil
	case KInt:
		switch len(b) {
		case 2:
			return NewInt(int64(int16(binary.BigEndian.Uint16(b)))), nil
		case 4:
			if t.Oid == OidOid || t.Oid == OidRegclass || t.Oid == OidRegtype {
				return NewInt(int64(binary.BigEndian.Uint32(b))), nil
			}
			return NewInt(int64(int32(binary.BigEndian.Uint32(b)))), nil
		case 8:
			return NewInt(int64(binary.BigEndian.Uint64(b))), nil
		}
		return Null, badBinary(t)
	case KFloat:
		switch len(b) {
		case 4:
			return NewFloat(float64(math.Float32frombits(binary.BigEndian.Uint32(b)))), nil
		case 8:
			return NewFloat(math.Float64frombits(binary.BigEndian.Uint64(b))), nil
		}
		return Null, badBinary(t)
	case KNumeric:
		d, err := decodeNumericBinary(b)
		if err != nil {
			return Null, badBinary(t)
		}
		return ApplyNumericMod(NewNumeric(d), t)
	case KText:
		return ApplyCharMod(NewText(string(b)), t, false)
	case KBytea:
		return NewBytea(b), nil
	case KDate:
		if len(b) != 4 {
			return Null, badBinary(t)
		}
		return NewDate(int64(int32(binary.BigEndian.Uint32(b)))), nil
	case KTimestamp, KTimestampTZ:
		if len(b) != 8 {
			return Null, badBinary(t)
		}
		us := int64(binary.BigEndian.Uint64(b))
		if t.Kind() == KTimestamp {
			return NewTimestamp(us), nil
		}
		return NewTimestampTZ(us), nil
	case KInterval:
		if len(b) != 16 {
			return Null, badBinary(t)
		}
		return NewInterval(Interval{
			Micros: int64(binary.BigEndian.Uint64(b)),
			Days:   int32(binary.BigEndian.Uint32(b[8:])),
			Months: int32(binary.BigEndian.Uint32(b[12:])),
		}), nil
	case KArray:
		if len(b) < 12 {
			return Null, badBinary(t)
		}
		ndim := binary.BigEndian.Uint32(b)
		elemOid := binary.BigEndian.Uint32(b[8:])
		et := T{elemOid, -1}
		if t.IsArray() {
			et = t.Elem()
		}
		if ndim == 0 {
			return NewArray(et, nil), nil
		}
		if ndim != 1 || len(b) < 20 {
			return Null, pgerr.Unsupported("multidimensional arrays are not supported")
		}
		n := int(binary.BigEndian.Uint32(b[12:]))
		p := b[20:]
		vals := make([]Value, 0, n)
		for i := 0; i < n; i++ {
			if len(p) < 4 {
				return Null, badBinary(t)
			}
			l := int32(binary.BigEndian.Uint32(p))
			p = p[4:]
			if l < 0 {
				vals = append(vals, Null)
				continue
			}
			if int(l) > len(p) {
				return Null, badBinary(t)
			}
			v, err := DecodeBinary(p[:l], et)
			if err != nil {
				return Null, err
			}
			vals = append(vals, v)
			p = p[l:]
		}
		return NewArray(et, vals), nil
	}
	return Null, pgerr.Unsupported("binary input is not supported for type %s", t.String())
}

func decodeNumericBinary(b []byte) (Decimal, error) {
	if len(b) < 8 {
		return Decimal{}, badBinary(Numeric)
	}
	ndigits := int(binary.BigEndian.Uint16(b))
	weight := int(int16(binary.BigEndian.Uint16(b[2:])))
	sign := binary.BigEndian.Uint16(b[4:])
	dscale := int32(binary.BigEndian.Uint16(b[6:]))
	if sign == 0xC000 {
		return NaNDecimal, nil
	}
	if len(b) < 8+2*ndigits {
		return Decimal{}, badBinary(Numeric)
	}
	c := new(big.Int)
	tenK := big.NewInt(10000)
	for i := 0; i < ndigits; i++ {
		c.Mul(c, tenK)
		c.Add(c, big.NewInt(int64(binary.BigEndian.Uint16(b[8+2*i:]))))
	}
	// Value = c * 10000^(weight - ndigits + 1).
	exp := 4 * (weight - ndigits + 1)
	var d Decimal
	if exp >= 0 {
		c.Mul(c, pow10(int32(exp)))
		d = Decimal{coef: c}
	} else {
		d = Decimal{coef: c, scale: int32(-exp)}
	}
	if sign == 0x4000 {
		d = d.Neg()
	}
	if d.scale > dscale {
		d = d.Round(dscale)
	} else if d.scale < dscale {
		d = Decimal{coef: d.rescale(dscale), scale: dscale}
	}
	return d, nil
}
