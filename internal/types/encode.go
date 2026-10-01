package types

import (
	"encoding/binary"
	"errors"
	"math"
	"math/big"
)

// Row encoding (heap tuples): uvarint column count, then for each column a
// kind tag followed by a kind-specific payload. The encoding is
// self-describing so that a row can be decoded without the table schema,
// which keeps rows readable after ALTER TABLE ADD COLUMN.

var errCorruptRow = errors.New("corrupt row encoding")

// EncodeRow appends the encoding of vals to dst.
func EncodeRow(dst []byte, vals []Value) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(vals)))
	for _, v := range vals {
		dst = encodeValue(dst, v)
	}
	return dst
}

func encodeValue(dst []byte, v Value) []byte {
	dst = append(dst, byte(v.K))
	switch v.K {
	case KNull:
	case KBool:
		dst = append(dst, byte(v.I))
	case KInt, KDate, KTimestamp, KTimestampTZ:
		dst = binary.AppendVarint(dst, v.I)
	case KFloat:
		dst = binary.BigEndian.AppendUint64(dst, uint64(v.I))
	case KNumeric:
		// scale, then either 0 + zigzag int64 coefficient, 1 + text (NaN or
		// coefficients beyond int64).
		d := v.Num()
		dst = binary.AppendUvarint(dst, uint64(d.scale))
		if d.coef == nil && !d.nan {
			dst = append(dst, 0)
			dst = binary.AppendVarint(dst, d.small)
		} else {
			s := d.String()
			dst = append(dst, 1)
			dst = binary.AppendUvarint(dst, uint64(len(s)))
			dst = append(dst, s...)
		}
	case KText, KBytea:
		dst = binary.AppendUvarint(dst, uint64(len(v.S)))
		dst = append(dst, v.S...)
	case KInterval:
		iv := v.Interval()
		dst = binary.AppendVarint(dst, int64(iv.Months))
		dst = binary.AppendVarint(dst, int64(iv.Days))
		dst = binary.AppendVarint(dst, iv.Micros)
	case KArray:
		a := v.Arr()
		dst = binary.AppendUvarint(dst, uint64(a.Elem.Oid))
		dst = binary.AppendUvarint(dst, uint64(len(a.Vals)))
		for _, e := range a.Vals {
			dst = encodeValue(dst, e)
		}
	}
	return dst
}

// DecodeRowMask is DecodeRow that materializes only the columns whose need
// entry is true (others stay NULL); variable-length values it skips are
// not copied.
func DecodeRowMask(b []byte, ncols int, need []bool) ([]Value, error) {
	n, k := binary.Uvarint(b)
	if k <= 0 {
		return nil, errCorruptRow
	}
	b = b[k:]
	if ncols < int(n) {
		ncols = int(n)
	}
	out := make([]Value, ncols)
	for i := 0; i < int(n); i++ {
		if i < len(need) && !need[i] {
			rest, err := skipValue(b)
			if err != nil {
				return nil, err
			}
			b = rest
			continue
		}
		v, rest, err := decodeValue(b)
		if err != nil {
			return nil, err
		}
		out[i] = v
		b = rest
	}
	return out, nil
}

// skipValue steps over one encoded value without building it.
func skipValue(orig []byte) ([]byte, error) {
	if len(orig) == 0 {
		return nil, errCorruptRow
	}
	k := Kind(orig[0])
	b := orig[1:]
	switch k {
	case KNull:
		return b, nil
	case KBool:
		if len(b) < 1 {
			return nil, errCorruptRow
		}
		return b[1:], nil
	case KInt, KDate, KTimestamp, KTimestampTZ:
		_, n := binary.Varint(b)
		if n <= 0 {
			return nil, errCorruptRow
		}
		return b[n:], nil
	case KFloat:
		if len(b) < 8 {
			return nil, errCorruptRow
		}
		return b[8:], nil
	case KText, KBytea:
		l, n := binary.Uvarint(b)
		if n <= 0 || uint64(len(b)-n) < l {
			return nil, errCorruptRow
		}
		return b[n+int(l):], nil
	case KNumeric:
		_, n := binary.Uvarint(b)
		if n <= 0 || len(b) <= n {
			return nil, errCorruptRow
		}
		b = b[n:]
		if b[0] == 0 {
			_, m := binary.Varint(b[1:])
			if m <= 0 {
				return nil, errCorruptRow
			}
			return b[1+m:], nil
		}
		l, m := binary.Uvarint(b[1:])
		if m <= 0 || uint64(len(b)-1-m) < l {
			return nil, errCorruptRow
		}
		return b[1+m+int(l):], nil
	}
	// Rarer kinds (interval, array): decode and discard.
	_, rest, err := decodeValue(orig)
	return rest, err
}

// DecodeRow decodes a row produced by EncodeRow. If ncols is larger than
// the stored column count the missing trailing columns are NULL.
func DecodeRow(b []byte, ncols int) ([]Value, error) {
	n, k := binary.Uvarint(b)
	if k <= 0 {
		return nil, errCorruptRow
	}
	b = b[k:]
	if ncols < int(n) {
		ncols = int(n)
	}
	out := make([]Value, ncols)
	for i := 0; i < int(n); i++ {
		v, rest, err := decodeValue(b)
		if err != nil {
			return nil, err
		}
		out[i] = v
		b = rest
	}
	return out, nil
}

func decodeValue(b []byte) (Value, []byte, error) {
	if len(b) == 0 {
		return Null, nil, errCorruptRow
	}
	k := Kind(b[0])
	b = b[1:]
	switch k {
	case KNull:
		return Null, b, nil
	case KBool:
		if len(b) < 1 {
			return Null, nil, errCorruptRow
		}
		return NewBool(b[0] != 0), b[1:], nil
	case KInt, KDate, KTimestamp, KTimestampTZ:
		i, n := binary.Varint(b)
		if n <= 0 {
			return Null, nil, errCorruptRow
		}
		return Value{K: k, I: i}, b[n:], nil
	case KFloat:
		if len(b) < 8 {
			return Null, nil, errCorruptRow
		}
		return Value{K: KFloat, I: int64(binary.BigEndian.Uint64(b))}, b[8:], nil
	case KNumeric:
		scale, n := binary.Uvarint(b)
		if n <= 0 || len(b) <= n {
			return Null, nil, errCorruptRow
		}
		b = b[n:]
		if b[0] == 0 {
			c, m := binary.Varint(b[1:])
			if m <= 0 {
				return Null, nil, errCorruptRow
			}
			return NewNumeric(Decimal{small: c, scale: int32(scale)}), b[1+m:], nil
		}
		l, m := binary.Uvarint(b[1:])
		if m <= 0 || uint64(len(b)-1-m) < l {
			return Null, nil, errCorruptRow
		}
		d, err := ParseDecimal(string(b[1+m : 1+m+int(l)]))
		if err != nil {
			return Null, nil, errCorruptRow
		}
		return NewNumeric(d), b[1+m+int(l):], nil
	case KText, KBytea:
		l, n := binary.Uvarint(b)
		if n <= 0 || uint64(len(b)-n) < l {
			return Null, nil, errCorruptRow
		}
		return Value{K: k, S: string(b[n : n+int(l)])}, b[n+int(l):], nil
	case KInterval:
		m, n1 := binary.Varint(b)
		if n1 <= 0 {
			return Null, nil, errCorruptRow
		}
		d, n2 := binary.Varint(b[n1:])
		if n2 <= 0 {
			return Null, nil, errCorruptRow
		}
		us, n3 := binary.Varint(b[n1+n2:])
		if n3 <= 0 {
			return Null, nil, errCorruptRow
		}
		return NewInterval(Interval{Months: int32(m), Days: int32(d), Micros: us}), b[n1+n2+n3:], nil
	case KArray:
		oid, n1 := binary.Uvarint(b)
		if n1 <= 0 {
			return Null, nil, errCorruptRow
		}
		cnt, n2 := binary.Uvarint(b[n1:])
		if n2 <= 0 || cnt > uint64(len(b)) {
			return Null, nil, errCorruptRow
		}
		b = b[n1+n2:]
		vals := make([]Value, cnt)
		for i := range vals {
			v, rest, err := decodeValue(b)
			if err != nil {
				return Null, nil, err
			}
			vals[i] = v
			b = rest
		}
		return NewArray(T{uint32(oid), -1}, vals), b, nil
	}
	return Null, nil, errCorruptRow
}

// Key encoding: an order-preserving ("memcomparable") byte string. For two
// values a and b of the same type, bytes.Compare(EncodeKey(a), EncodeKey(b))
// has the sign of CompareNullsLast(a, b). Equal values (including 1.5 and
// 1.50, or 0 and -0) produce identical encodings, so the encoding also
// serves as a hash key.

const (
	keyNotNull = 0x01
	keyNull    = 0x02
)

// EncodeKey appends the key encoding of v to dst.
func EncodeKey(dst []byte, v Value) []byte {
	if v.IsNull() {
		return append(dst, keyNull)
	}
	dst = append(dst, keyNotNull)
	switch v.K {
	case KBool, KInt, KDate, KTimestamp, KTimestampTZ:
		return binary.BigEndian.AppendUint64(dst, uint64(v.I)^(1<<63))
	case KFloat:
		return appendFloatKey(dst, v.Float())
	case KNumeric:
		return appendNumericKey(dst, v.Num())
	case KText, KBytea:
		return appendBytesKey(dst, v.S)
	case KInterval:
		return appendFloatKey(dst, v.Interval().approxMicros())
	case KArray:
		a := v.Arr()
		for _, e := range a.Vals {
			dst = append(dst, 0x01)
			dst = EncodeKey(dst, e)
		}
		return append(dst, 0x00)
	}
	return dst
}

func appendFloatKey(dst []byte, f float64) []byte {
	if f == 0 {
		f = 0 // collapse -0 into +0
	}
	var u uint64
	switch {
	case math.IsNaN(f):
		u = math.MaxUint64
	case f >= 0:
		u = math.Float64bits(f) ^ (1 << 63)
	default:
		u = ^math.Float64bits(f)
	}
	return binary.BigEndian.AppendUint64(dst, u)
}

func appendBytesKey(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		dst = append(dst, c)
		if c == 0 {
			dst = append(dst, 0xFF)
		}
	}
	return append(dst, 0x00, 0x01)
}

// appendNumericKey encodes a decimal as: a class byte (negative, zero,
// positive, NaN), then for non-zero values the decimal exponent of the
// leading digit and the significant digits; for negative numbers the
// exponent and digits are inverted so that larger magnitudes sort first.
func appendNumericKey(dst []byte, d Decimal) []byte {
	if d.IsNaN() {
		return append(dst, 0x04)
	}
	n := d.Normalize()
	sign := n.Sign()
	if sign == 0 {
		return append(dst, 0x02)
	}
	digits := new(big.Int).Abs(n.Coef()).String()
	// value = 0.d1d2d3... * 10^exp
	exp := int64(len(digits)) - int64(n.Scale())
	// Strip trailing zeros of the integer coefficient (scale 0 case).
	end := len(digits)
	for end > 1 && digits[end-1] == '0' {
		end--
	}
	digits = digits[:end]
	e := uint16(int16(exp)) ^ 0x8000
	if sign > 0 {
		dst = append(dst, 0x03)
		dst = binary.BigEndian.AppendUint16(dst, e)
		dst = append(dst, digits...)
		return append(dst, 0x00)
	}
	dst = append(dst, 0x01)
	dst = binary.BigEndian.AppendUint16(dst, ^e)
	for i := 0; i < len(digits); i++ {
		dst = append(dst, ^digits[i])
	}
	return append(dst, 0xFF)
}
