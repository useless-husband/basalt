package types

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// Decimal is an arbitrary precision decimal number: coefficient *
// 10^-scale. Scale is the display scale (PostgreSQL's dscale), so 1.50 has
// coefficient 150 and scale 2. The zero value is 0. Decimals are
// immutable.
//
// A coefficient that fits in an int64 is kept in small (coef is nil), and
// the common operations work on it directly; coef holds larger ones.
type Decimal struct {
	coef  *big.Int
	small int64
	scale int32
	nan   bool
}

var pow10i64 = [19]int64{1, 10, 100, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10, 1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18}

// fromBig builds a decimal, keeping the coefficient small when it fits.
func fromBig(c *big.Int, scale int32) Decimal {
	if c.IsInt64() {
		return Decimal{small: c.Int64(), scale: scale}
	}
	return Decimal{coef: c, scale: scale}
}

// rescaled returns the coefficient at scale s >= d.scale if it fits.
func (d Decimal) rescaled(s int32) (int64, bool) {
	if d.coef != nil {
		return 0, false
	}
	k := s - d.scale
	if k == 0 {
		return d.small, true
	}
	if k < 0 || k > 18 {
		return 0, false
	}
	p := pow10i64[k]
	if d.small > math.MaxInt64/p || d.small < math.MinInt64/p {
		return 0, false
	}
	return d.small * p, true
}

var (
	bigTen     = big.NewInt(10)
	bigOne     = big.NewInt(1)
	bigZero    = big.NewInt(0)
	pow10Cache [40]*big.Int
)

func init() {
	p := big.NewInt(1)
	for i := range pow10Cache {
		pow10Cache[i] = new(big.Int).Set(p)
		p.Mul(p, bigTen)
	}
}

func pow10(n int32) *big.Int {
	if n < int32(len(pow10Cache)) {
		return pow10Cache[n]
	}
	return new(big.Int).Exp(bigTen, big.NewInt(int64(n)), nil)
}

// NaNDecimal is numeric 'NaN'.
var NaNDecimal = Decimal{nan: true}

func (d Decimal) c() *big.Int {
	if d.coef == nil {
		if d.small == 0 {
			return bigZero
		}
		return big.NewInt(d.small)
	}
	return d.coef
}

// IsNaN reports whether d is NaN.
func (d Decimal) IsNaN() bool { return d.nan }

// Scale returns the display scale.
func (d Decimal) Scale() int32 { return d.scale }

// Sign returns -1, 0 or 1.
func (d Decimal) Sign() int {
	if d.coef == nil {
		switch {
		case d.small > 0:
			return 1
		case d.small < 0:
			return -1
		}
		return 0
	}
	return d.coef.Sign()
}

// DecimalFromInt converts an integer.
func DecimalFromInt(i int64) Decimal { return Decimal{small: i} }

// DecimalFromBig builds a decimal from a coefficient and scale.
func DecimalFromBig(coef *big.Int, scale int32) Decimal {
	return fromBig(new(big.Int).Set(coef), scale)
}

// DecimalFromFloat converts a float using 15 significant digits, like
// PostgreSQL's float8 to numeric cast.
func DecimalFromFloat(f float64) (Decimal, error) {
	if math.IsNaN(f) {
		return NaNDecimal, nil
	}
	if math.IsInf(f, 0) {
		return Decimal{}, pgerr.New(pgerr.FeatureNotSupported, "cannot convert infinity to numeric")
	}
	return ParseDecimal(strconv.FormatFloat(f, 'g', 15, 64))
}

// ParseDecimal parses a numeric literal such as "-12.50", "1e-3" or "NaN".
func ParseDecimal(s string) (Decimal, error) {
	t := strings.TrimSpace(s)
	if strings.EqualFold(t, "nan") {
		return NaNDecimal, nil
	}
	bad := func() (Decimal, error) {
		return Decimal{}, pgerr.New(pgerr.InvalidTextRepresentation, "invalid input syntax for type numeric: \"%s\"", s)
	}
	if t == "" {
		return bad()
	}
	neg := false
	if t[0] == '+' || t[0] == '-' {
		neg = t[0] == '-'
		t = t[1:]
	}
	exp := int64(0)
	if i := strings.IndexAny(t, "eE"); i >= 0 {
		e, err := strconv.ParseInt(t[i+1:], 10, 32)
		if err != nil {
			return bad()
		}
		exp = e
		t = t[:i]
	}
	intPart, frac := t, ""
	if i := strings.IndexByte(t, '.'); i >= 0 {
		intPart, frac = t[:i], t[i+1:]
	}
	if intPart == "" && frac == "" {
		return bad()
	}
	digits := intPart + frac
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return bad()
		}
	}
	if scale := int64(len(frac)) - exp; len(digits) > 0 && len(digits) <= 18 && scale >= 0 && scale <= 16383 {
		v, err := strconv.ParseInt(digits, 10, 64)
		if err == nil {
			if neg {
				v = -v
			}
			return Decimal{small: v, scale: int32(scale)}, nil
		}
	}
	coef, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		if digits == "" {
			coef = new(big.Int)
		} else {
			return bad()
		}
	}
	scale := int64(len(frac)) - exp
	if scale < 0 {
		coef.Mul(coef, pow10(int32(-scale)))
		scale = 0
	}
	if scale > 16383 {
		return Decimal{}, pgerr.New(pgerr.NumericValueOutOfRange, "value overflows numeric format")
	}
	if neg {
		coef.Neg(coef)
	}
	return fromBig(coef, int32(scale)), nil
}

// String formats d with exactly scale digits after the decimal point.
func (d Decimal) String() string {
	if d.nan {
		return "NaN"
	}
	var s string
	var neg bool
	if d.coef == nil && d.small != math.MinInt64 {
		v := d.small
		if v < 0 {
			neg, v = true, -v
		}
		s = strconv.FormatInt(v, 10)
	} else {
		c := d.c()
		neg = c.Sign() < 0
		s = new(big.Int).Abs(c).String()
	}
	if d.scale > 0 {
		if int32(len(s)) <= d.scale {
			s = strings.Repeat("0", int(d.scale)-len(s)+1) + s
		}
		s = s[:len(s)-int(d.scale)] + "." + s[len(s)-int(d.scale):]
	}
	if neg {
		return "-" + s
	}
	return s
}

// rescale returns the coefficient of d expressed with the given scale
// (which must be >= d.scale).
func (d Decimal) rescale(scale int32) *big.Int {
	if scale == d.scale {
		return d.c()
	}
	return new(big.Int).Mul(d.c(), pow10(scale-d.scale))
}

// Cmp compares two decimals. NaN is greater than every other value.
func (d Decimal) Cmp(e Decimal) int {
	if d.nan || e.nan {
		switch {
		case d.nan && e.nan:
			return 0
		case d.nan:
			return 1
		}
		return -1
	}
	s := d.scale
	if e.scale > s {
		s = e.scale
	}
	if a, ok := d.rescaled(s); ok {
		if b, ok := e.rescaled(s); ok {
			return cmpInt(a, b)
		}
	}
	return d.rescale(s).Cmp(e.rescale(s))
}

// Add returns d+e.
func (d Decimal) Add(e Decimal) Decimal {
	if d.nan || e.nan {
		return NaNDecimal
	}
	s := d.scale
	if e.scale > s {
		s = e.scale
	}
	if a, ok := d.rescaled(s); ok {
		if b, ok := e.rescaled(s); ok {
			if r := a + b; (b >= 0) == (r >= a) {
				return Decimal{small: r, scale: s}
			}
		}
	}
	return fromBig(new(big.Int).Add(d.rescale(s), e.rescale(s)), s)
}

// Sub returns d-e.
func (d Decimal) Sub(e Decimal) Decimal {
	if d.nan || e.nan {
		return NaNDecimal
	}
	s := d.scale
	if e.scale > s {
		s = e.scale
	}
	if a, ok := d.rescaled(s); ok {
		if b, ok := e.rescaled(s); ok {
			if r := a - b; (b >= 0) == (r <= a) {
				return Decimal{small: r, scale: s}
			}
		}
	}
	return fromBig(new(big.Int).Sub(d.rescale(s), e.rescale(s)), s)
}

// Mul returns d*e with scale d.scale+e.scale.
func (d Decimal) Mul(e Decimal) Decimal {
	if d.nan || e.nan {
		return NaNDecimal
	}
	if d.coef == nil && e.coef == nil {
		a, b := d.small, e.small
		if a == 0 || b == 0 {
			return Decimal{scale: d.scale + e.scale}
		}
		if r := a * b; r/b == a && !(a == -1 && b == math.MinInt64) && !(b == -1 && a == math.MinInt64) {
			return Decimal{small: r, scale: d.scale + e.scale}
		}
	}
	return fromBig(new(big.Int).Mul(d.c(), e.c()), d.scale+e.scale)
}

// Neg returns -d.
func (d Decimal) Neg() Decimal {
	if d.nan {
		return d
	}
	if d.coef == nil && d.small != math.MinInt64 {
		return Decimal{small: -d.small, scale: d.scale}
	}
	return fromBig(new(big.Int).Neg(d.c()), d.scale)
}

// Abs returns |d|.
func (d Decimal) Abs() Decimal {
	if d.nan || d.Sign() >= 0 {
		return d
	}
	return d.Neg()
}

// weightAndFirst returns the base-10000 weight of the most significant
// non-zero digit group and that group's value, as PostgreSQL's numeric
// stores them. Used to choose the result scale of division.
func (d Decimal) weightAndFirst() (int, int64) {
	c := new(big.Int).Abs(d.c())
	if c.Sign() == 0 {
		return 0, 0
	}
	// Align the coefficient to a multiple of 4 decimal places.
	scale := int(d.scale)
	pad := (4 - scale%4) % 4
	if pad > 0 {
		c.Mul(c, pow10(int32(pad)))
	}
	scale += pad
	digits := c.String()
	groupsAfterPoint := scale / 4
	// Number of base-10000 digits.
	n := (len(digits) + 3) / 4
	firstLen := len(digits) - (n-1)*4
	first, _ := strconv.ParseInt(digits[:firstLen], 10, 64)
	weight := n - 1 - groupsAfterPoint
	return weight, first
}

func maxI32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// DivScale returns the result scale PostgreSQL chooses for d/e.
func DivScale(d, e Decimal) int32 {
	w1, f1 := d.weightAndFirst()
	w2, f2 := e.weightAndFirst()
	qweight := w1 - w2
	if f1 <= f2 {
		qweight--
	}
	rscale := int32(16 - qweight*4)
	rscale = maxI32(rscale, d.scale)
	rscale = maxI32(rscale, e.scale)
	rscale = maxI32(rscale, 0)
	if rscale > 1000 {
		rscale = 1000
	}
	return rscale
}

// Div returns d/e rounded to PostgreSQL's choice of scale.
func (d Decimal) Div(e Decimal) (Decimal, error) {
	return d.DivScale(e, DivScale(d, e))
}

// DivScale returns d/e rounded half away from zero to the given scale.
func (d Decimal) DivScale(e Decimal, scale int32) (Decimal, error) {
	if d.nan || e.nan {
		return NaNDecimal, nil
	}
	if e.Sign() == 0 {
		return Decimal{}, pgerr.New(pgerr.DivisionByZero, "division by zero")
	}
	// d/e = (dc * 10^-ds) / (ec * 10^-es); want q * 10^-scale.
	// q = dc * 10^(scale - ds + es) / ec
	num := new(big.Int).Set(d.c())
	den := new(big.Int).Set(e.c())
	shift := scale - d.scale + e.scale
	if shift >= 0 {
		num.Mul(num, pow10(shift))
	} else {
		den.Mul(den, pow10(-shift))
	}
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	// Round half away from zero.
	r2 := new(big.Int).Abs(r)
	r2.Lsh(r2, 1)
	if r2.Cmp(new(big.Int).Abs(den)) >= 0 {
		if (num.Sign() < 0) != (den.Sign() < 0) {
			q.Sub(q, bigOne)
		} else {
			q.Add(q, bigOne)
		}
	}
	return fromBig(q, scale), nil
}

// Mod returns the remainder of d/e with the sign of d.
func (d Decimal) Mod(e Decimal) (Decimal, error) {
	if d.nan || e.nan {
		return NaNDecimal, nil
	}
	if e.Sign() == 0 {
		return Decimal{}, pgerr.New(pgerr.DivisionByZero, "division by zero")
	}
	s := maxI32(d.scale, e.scale)
	r := new(big.Int).Rem(d.rescale(s), e.rescale(s))
	return fromBig(r, s), nil
}

// Round rounds half away from zero to the given number of decimal places.
// A negative scale rounds to tens, hundreds and so on.
func (d Decimal) Round(scale int32) Decimal {
	if d.nan {
		return d
	}
	if scale >= d.scale {
		if v, ok := d.rescaled(scale); ok {
			return Decimal{small: v, scale: scale}
		}
		return fromBig(d.rescale(scale), scale)
	}
	drop := d.scale - scale
	if d.coef == nil && drop <= 18 && scale >= 0 {
		p := pow10i64[drop]
		q, r := d.small/p, d.small%p
		if r < 0 {
			r = -r
		}
		if r >= p-r { // r*2 >= p without overflow
			if d.small < 0 {
				q--
			} else {
				q++
			}
		}
		return Decimal{small: q, scale: scale}
	}
	p := pow10(drop)
	q, r := new(big.Int).QuoRem(d.c(), p, new(big.Int))
	r.Abs(r)
	r.Lsh(r, 1)
	if r.Cmp(p) >= 0 {
		if d.Sign() < 0 {
			q.Sub(q, bigOne)
		} else {
			q.Add(q, bigOne)
		}
	}
	if scale < 0 {
		q.Mul(q, pow10(-scale))
		return fromBig(q, 0)
	}
	return fromBig(q, scale)
}

// Trunc truncates toward zero to the given scale.
func (d Decimal) Trunc(scale int32) Decimal {
	if d.nan || scale >= d.scale {
		return d
	}
	if scale < 0 {
		scale = 0
	}
	q := new(big.Int).Quo(d.c(), pow10(d.scale-scale))
	return fromBig(q, scale)
}

// Floor returns the greatest integer <= d.
func (d Decimal) Floor() Decimal {
	if d.nan || d.scale == 0 {
		return d
	}
	p := pow10(d.scale)
	q, m := new(big.Int).DivMod(d.c(), p, new(big.Int))
	_ = m
	return fromBig(q, 0)
}

// Ceil returns the least integer >= d.
func (d Decimal) Ceil() Decimal {
	return d.Neg().Floor().Neg()
}

// Normalize removes trailing fractional zeros, so equal values have equal
// representations (1.50 and 1.5 both become 1.5).
func (d Decimal) Normalize() Decimal {
	if d.nan || d.scale == 0 {
		return d
	}
	if d.coef == nil {
		v, sc := d.small, d.scale
		for sc > 0 && v%10 == 0 {
			v /= 10
			sc--
		}
		return Decimal{small: v, scale: sc}
	}
	c := new(big.Int).Set(d.c())
	s := d.scale
	r := new(big.Int)
	for s > 0 {
		q, rem := new(big.Int).QuoRem(c, bigTen, r)
		if rem.Sign() != 0 {
			break
		}
		c = q
		s--
	}
	return fromBig(c, s)
}

// Float64 converts d to the nearest float64.
func (d Decimal) Float64() float64 {
	if d.nan {
		return math.NaN()
	}
	// Exact operands give a correctly rounded quotient.
	if d.coef == nil && d.small < 1<<53 && d.small > -(1<<53) && d.scale <= 22 {
		return float64(d.small) / math.Pow10(int(d.scale))
	}
	f, _ := strconv.ParseFloat(d.String(), 64)
	return f
}

// Int64 rounds d half away from zero to an integer.
func (d Decimal) Int64() (int64, bool) {
	if d.nan {
		return 0, false
	}
	r := d.Round(0)
	if !r.c().IsInt64() {
		return 0, false
	}
	return r.c().Int64(), true
}

// IsInteger reports whether d has no fractional part.
func (d Decimal) IsInteger() bool {
	if d.nan {
		return false
	}
	return d.Normalize().scale == 0
}

// Coef returns the coefficient (do not modify).
func (d Decimal) Coef() *big.Int { return d.c() }

// Precision returns the number of significant decimal digits before the
// decimal point (used for numeric(p,s) overflow checks).
func (d Decimal) IntDigits() int {
	c := new(big.Int).Abs(d.c())
	if d.scale > 0 {
		c.Quo(c, pow10(d.scale))
	}
	if c.Sign() == 0 {
		return 0
	}
	return len(c.String())
}

// Sqrt returns the square root with PostgreSQL-like scale.
func (d Decimal) Sqrt() (Decimal, error) {
	if d.nan {
		return d, nil
	}
	if d.Sign() < 0 {
		return Decimal{}, pgerr.New("2201F", "cannot take square root of a negative number")
	}
	scale := maxI32(d.scale, 16)
	// sqrt(c * 10^-s) at scale S: isqrt(c * 10^(2S - s)).
	n := new(big.Int).Set(d.c())
	n.Mul(n, pow10(2*scale-d.scale))
	return fromBig(new(big.Int).Sqrt(n), scale), nil
}
