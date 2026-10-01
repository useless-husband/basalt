package types

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/useless-husband/basalt/internal/pgerr"
)

const (
	usPerSec  = int64(1_000_000)
	usPerMin  = 60 * usPerSec
	usPerHour = 60 * usPerMin
	usPerDay  = 24 * usPerHour
)

// pgEpoch is 2000-01-01 00:00:00 UTC, the zero point of PostgreSQL dates
// and timestamps.
var pgEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// DateFromTime converts a time to days since 2000-01-01.
func DateFromTime(t time.Time) int64 {
	y, m, d := t.Date()
	return int64(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Sub(pgEpoch) / (24 * time.Hour))
}

// TimeFromDate converts days since 2000-01-01 to a UTC time.
func TimeFromDate(days int64) time.Time { return pgEpoch.AddDate(0, 0, int(days)) }

// TimestampFromTime converts a time to microseconds since 2000-01-01 UTC.
func TimestampFromTime(t time.Time) int64 {
	days := DateFromTime(t.UTC())
	h, mi, s := t.UTC().Clock()
	return days*usPerDay + int64(h)*usPerHour + int64(mi)*usPerMin + int64(s)*usPerSec + int64(t.Nanosecond()/1000)
}

// TimeFromTimestamp converts microseconds since 2000-01-01 to a UTC time.
func TimeFromTimestamp(us int64) time.Time {
	days := us / usPerDay
	rem := us % usPerDay
	if rem < 0 {
		rem += usPerDay
		days--
	}
	return TimeFromDate(days).Add(time.Duration(rem) * time.Microsecond)
}

func badDatetime(typ, s string) error {
	return pgerr.New(pgerr.InvalidDatetimeFormat, "invalid input syntax for type %s: \"%s\"", typ, s)
}

func atoiStrict(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func parseYMD(s string) (y, m, d int, ok bool) {
	parts := strings.Split(s, "-")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var ok1, ok2, ok3 bool
	y, ok1 = atoiStrict(parts[0])
	m, ok2 = atoiStrict(parts[1])
	d, ok3 = atoiStrict(parts[2])
	if !ok1 || !ok2 || !ok3 || m < 1 || m > 12 || d < 1 || d > 31 {
		return 0, 0, 0, false
	}
	// Reject dates such as February 30th.
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if t.Day() != d {
		return 0, 0, 0, false
	}
	return y, m, d, true
}

// ParseDate parses an ISO date (YYYY-MM-DD).
func ParseDate(s string) (int64, error) {
	t := strings.TrimSpace(s)
	switch strings.ToLower(t) {
	case "epoch":
		return DateFromTime(time.Unix(0, 0).UTC()), nil
	}
	// Accept a timestamp-looking string and keep the date part.
	if i := strings.IndexAny(t, " T"); i > 0 {
		t = t[:i]
	}
	y, m, d, ok := parseYMD(t)
	if !ok {
		return 0, badDatetime("date", s)
	}
	return DateFromTime(time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)), nil
}

// FormatDate formats a date as YYYY-MM-DD.
func FormatDate(days int64) string {
	t := TimeFromDate(days)
	y := t.Year()
	if y <= 0 {
		return fmt.Sprintf("%04d-%02d-%02d BC", 1-y, t.Month(), t.Day())
	}
	return fmt.Sprintf("%04d-%02d-%02d", y, t.Month(), t.Day())
}

// ParseTimestamp parses "YYYY-MM-DD[ T]HH:MM[:SS[.ffffff]][zone]". When
// withZone is true a zone suffix is honoured and the result is UTC; when
// false a zone suffix is ignored, as PostgreSQL does for timestamp.
func ParseTimestamp(s string, withZone bool) (int64, error) {
	typ := "timestamp"
	if withZone {
		typ = "timestamp with time zone"
	}
	t := strings.TrimSpace(s)
	switch strings.ToLower(t) {
	case "epoch":
		return TimestampFromTime(time.Unix(0, 0)), nil
	case "now":
		return TimestampFromTime(time.Now()), nil
	}
	datePart, rest := t, ""
	if i := strings.IndexAny(t, " T"); i > 0 {
		datePart, rest = t[:i], strings.TrimSpace(t[i+1:])
	}
	y, m, d, ok := parseYMD(datePart)
	if !ok {
		return 0, badDatetime(typ, s)
	}
	var hh, mm, ss, us int
	offset := 0 // seconds east of UTC
	if rest != "" {
		// Split off a zone: Z, UTC, +hh, +hh:mm, -hhmm.
		zone := ""
		if strings.HasSuffix(strings.ToUpper(rest), "UTC") {
			zone = "+00"
			rest = strings.TrimSpace(rest[:len(rest)-3])
		} else if strings.HasSuffix(rest, "Z") || strings.HasSuffix(rest, "z") {
			zone = "+00"
			rest = rest[:len(rest)-1]
		} else if i := strings.LastIndexAny(rest, "+-"); i > 0 {
			zone = rest[i:]
			rest = strings.TrimSpace(rest[:i])
		}
		clock := strings.Split(rest, ":")
		if len(clock) < 2 || len(clock) > 3 {
			return 0, badDatetime(typ, s)
		}
		var ok1, ok2 bool
		hh, ok1 = atoiStrict(clock[0])
		mm, ok2 = atoiStrict(clock[1])
		if !ok1 || !ok2 {
			return 0, badDatetime(typ, s)
		}
		if len(clock) == 3 {
			sec := clock[2]
			frac := ""
			if j := strings.IndexByte(sec, '.'); j >= 0 {
				sec, frac = sec[:j], sec[j+1:]
			}
			var ok3 bool
			ss, ok3 = atoiStrict(sec)
			if !ok3 {
				return 0, badDatetime(typ, s)
			}
			if frac != "" {
				if len(frac) > 6 {
					// Round to microseconds.
					f, err := strconv.ParseFloat("0."+frac, 64)
					if err != nil {
						return 0, badDatetime(typ, s)
					}
					us = int(f*1e6 + 0.5)
				} else {
					n, ok := atoiStrict(frac)
					if !ok {
						return 0, badDatetime(typ, s)
					}
					for k := len(frac); k < 6; k++ {
						n *= 10
					}
					us = n
				}
			}
		}
		if hh > 24 || mm > 59 || ss > 60 {
			return 0, pgerr.New(pgerr.DatetimeFieldOverflow, "date/time field value out of range: \"%s\"", s)
		}
		if zone != "" && withZone {
			sign := 1
			if zone[0] == '-' {
				sign = -1
			}
			z := strings.ReplaceAll(zone[1:], ":", "")
			var zh, zm int
			switch len(z) {
			case 1, 2:
				zh, ok = atoiStrict(z)
			case 4:
				zh, ok = atoiStrict(z[:2])
				zm, _ = atoiStrict(z[2:])
			default:
				ok = false
			}
			if !ok {
				return 0, badDatetime(typ, s)
			}
			offset = sign * (zh*3600 + zm*60)
		}
	}
	days := DateFromTime(time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC))
	ts := days*usPerDay + int64(hh)*usPerHour + int64(mm)*usPerMin + int64(ss)*usPerSec + int64(us)
	ts -= int64(offset) * usPerSec
	return ts, nil
}

// FormatTimestamp formats microseconds since 2000-01-01 as PostgreSQL does
// in the ISO DateStyle. withZone appends the UTC offset "+00".
func FormatTimestamp(us int64, withZone bool) string {
	t := TimeFromTimestamp(us)
	var b strings.Builder
	y := t.Year()
	bc := y <= 0
	if bc {
		y = 1 - y
	}
	fmt.Fprintf(&b, "%04d-%02d-%02d %02d:%02d:%02d", y, t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())
	if frac := t.Nanosecond() / 1000; frac != 0 {
		f := fmt.Sprintf("%06d", frac)
		b.WriteByte('.')
		b.WriteString(strings.TrimRight(f, "0"))
	}
	if withZone {
		b.WriteString("+00")
	}
	if bc {
		b.WriteString(" BC")
	}
	return b.String()
}

// Interval is a PostgreSQL interval: months, days and microseconds are kept
// separately because their lengths vary.
type Interval struct {
	Months int32
	Days   int32
	Micros int64
}

// approxMicros converts to microseconds with 30-day months, the rule
// PostgreSQL uses to compare intervals.
func (iv Interval) approxMicros() float64 {
	return float64(iv.Months)*30*float64(usPerDay) + float64(iv.Days)*float64(usPerDay) + float64(iv.Micros)
}

// Cmp compares intervals.
func (iv Interval) Cmp(o Interval) int {
	return CompareFloat(iv.approxMicros(), o.approxMicros())
}

// Add returns iv+o.
func (iv Interval) Add(o Interval) Interval {
	return Interval{iv.Months + o.Months, iv.Days + o.Days, iv.Micros + o.Micros}
}

// Neg returns -iv.
func (iv Interval) Neg() Interval { return Interval{-iv.Months, -iv.Days, -iv.Micros} }

// String formats the interval in PostgreSQL's "postgres" IntervalStyle.
func (iv Interval) String() string {
	var parts []string
	plural := func(n int64, unit string) string {
		if n == 1 || n == -1 {
			return fmt.Sprintf("%d %s", n, unit)
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	years, months := iv.Months/12, iv.Months%12
	if years != 0 {
		parts = append(parts, plural(int64(years), "year"))
	}
	if months != 0 {
		parts = append(parts, plural(int64(months), "mon"))
	}
	if iv.Days != 0 {
		parts = append(parts, plural(int64(iv.Days), "day"))
	}
	if iv.Micros != 0 || len(parts) == 0 {
		us := iv.Micros
		sign := ""
		if us < 0 {
			sign = "-"
			us = -us
		}
		h := us / usPerHour
		m := (us % usPerHour) / usPerMin
		s := (us % usPerMin) / usPerSec
		f := us % usPerSec
		clock := fmt.Sprintf("%s%02d:%02d:%02d", sign, h, m, s)
		if f != 0 {
			clock += "." + strings.TrimRight(fmt.Sprintf("%06d", f), "0")
		}
		parts = append(parts, clock)
	}
	return strings.Join(parts, " ")
}

// ParseInterval parses forms such as "1 day", "2 hours 30 minutes",
// "1 year 2 mons 3 days 04:05:06" and "-01:00:00".
func ParseInterval(s string) (Interval, error) {
	var iv Interval
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(s)))
	bad := func() (Interval, error) {
		return Interval{}, pgerr.New(pgerr.InvalidDatetimeFormat, "invalid input syntax for type interval: \"%s\"", s)
	}
	if len(fields) == 0 {
		return bad()
	}
	if fields[0] == "@" {
		fields = fields[1:]
	}
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "ago" {
			continue
		}
		if strings.Contains(f, ":") {
			neg := strings.HasPrefix(f, "-")
			f = strings.TrimLeft(f, "+-")
			c := strings.Split(f, ":")
			var h, m int
			var sec float64
			var ok bool
			if h, ok = atoiStrict(c[0]); !ok {
				return bad()
			}
			if len(c) > 1 {
				if m, ok = atoiStrict(c[1]); !ok {
					return bad()
				}
			}
			if len(c) > 2 {
				v, err := strconv.ParseFloat(c[2], 64)
				if err != nil {
					return bad()
				}
				sec = v
			}
			us := int64(h)*usPerHour + int64(m)*usPerMin + int64(sec*1e6+0.5)
			if neg {
				us = -us
			}
			iv.Micros += us
			continue
		}
		n, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return bad()
		}
		unit := "second"
		if i+1 < len(fields) {
			unit = fields[i+1]
			i++
		}
		if unit != "ms" && unit != "us" {
			unit = strings.TrimSuffix(unit, "s")
		}
		switch unit {
		case "year", "yr", "y":
			iv.Months += int32(n * 12)
		case "mon", "month", "mo":
			iv.Months += int32(n)
		case "week", "w":
			iv.Days += int32(n * 7)
		case "day", "d":
			iv.Days += int32(n)
			if frac := n - float64(int64(n)); frac != 0 {
				iv.Micros += int64(frac * float64(usPerDay))
			}
		case "hour", "hr", "h":
			iv.Micros += int64(n * float64(usPerHour))
		case "minute", "min", "m":
			iv.Micros += int64(n * float64(usPerMin))
		case "second", "sec", "":
			iv.Micros += int64(n * float64(usPerSec))
		case "millisecond", "msec", "ms":
			iv.Micros += int64(n * 1000)
		case "microsecond", "usec", "us":
			iv.Micros += int64(n)
		default:
			return bad()
		}
	}
	if len(fields) > 0 && fields[len(fields)-1] == "ago" {
		iv = iv.Neg()
	}
	return iv, nil
}

// AddIntervalToTimestamp adds an interval to a timestamp (months first,
// then days, then the time part), as PostgreSQL does.
func AddIntervalToTimestamp(us int64, iv Interval) int64 {
	if iv.Months != 0 || iv.Days != 0 {
		t := TimeFromTimestamp(us)
		t = t.AddDate(0, int(iv.Months), int(iv.Days))
		us = TimestampFromTime(t)
	}
	return us + iv.Micros
}

// SubTimestamps returns a-b as an interval of days and microseconds.
func SubTimestamps(a, b int64) Interval {
	d := a - b
	days := d / usPerDay
	return Interval{Days: int32(days), Micros: d - days*usPerDay}
}

// UsPerDay is exported for the executor's date arithmetic.
const UsPerDay = usPerDay
