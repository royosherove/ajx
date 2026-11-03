package measure

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseISO parses value the way Python's datetime.fromisoformat does after
// replacing "Z" with "+00:00" (util.parse_ts). aware reports whether the
// value carried a UTC offset; naive values are returned in UTC.
// Fractional seconds beyond microseconds are truncated, as in Python.
func ParseISO(value string) (t time.Time, aware bool, err error) {
	v := strings.ReplaceAll(value, "Z", "+00:00")
	bad := fmt.Errorf("Invalid isoformat string: %s", pyQuote(v))
	if len(v) < 7 {
		return time.Time{}, false, bad
	}
	var year, month, day int
	rest := ""
	switch {
	case len(v) >= 10 && v[4] == '-' && v[7] == '-':
		y, ok1 := digits(v[0:4])
		m, ok2 := digits(v[5:7])
		d, ok3 := digits(v[8:10])
		if !ok1 || !ok2 || !ok3 {
			return time.Time{}, false, bad
		}
		year, month, day, rest = y, m, d, v[10:]
	case len(v) >= 8:
		y, ok1 := digits(v[0:4])
		m, ok2 := digits(v[4:6])
		d, ok3 := digits(v[6:8])
		if !ok1 || !ok2 || !ok3 {
			return time.Time{}, false, bad
		}
		year, month, day, rest = y, m, d, v[8:]
	default:
		return time.Time{}, false, bad
	}
	hour, minute, sec, micro := 0, 0, 0, 0
	loc := time.UTC
	if rest != "" {
		// any single separator character, then a time
		r := []rune(rest)
		tpart := string(r[1:])
		if tpart == "" {
			return time.Time{}, false, bad
		}
		tz := ""
		if i := strings.IndexAny(tpart, "+-"); i >= 0 {
			tpart, tz = tpart[:i], tpart[i:]
		}
		h, m, s, us, ok := parseClock(tpart)
		if !ok {
			return time.Time{}, false, bad
		}
		hour, minute, sec, micro = h, m, s, us
		if tz != "" {
			off, ok := parseOffset(tz)
			if !ok {
				return time.Time{}, false, bad
			}
			aware = true
			loc = time.FixedZone("", off)
		}
	}
	if year < 1 {
		return time.Time{}, false, fmt.Errorf("year must be in 1..9999, not %d", year)
	}
	if month < 1 || month > 12 {
		return time.Time{}, false, fmt.Errorf("month must be in 1..12, not %d", month)
	}
	if dim := daysIn(year, month); day < 1 || day > dim {
		return time.Time{}, false, fmt.Errorf("day %d must be in range 1..%d for month %d in year %d", day, dim, month, year)
	}
	extraDay := false
	if hour == 24 {
		if minute != 0 || sec != 0 || micro != 0 {
			return time.Time{}, false, fmt.Errorf("minute, second, and microsecond must be 0 when hour is 24")
		}
		hour, extraDay = 0, true
	}
	switch {
	case hour > 23:
		return time.Time{}, false, fmt.Errorf("hour must be in 0..23, not %d", hour)
	case minute > 59:
		return time.Time{}, false, fmt.Errorf("minute must be in 0..59, not %d", minute)
	case sec > 59:
		return time.Time{}, false, fmt.Errorf("second must be in 0..59, not %d", sec)
	}
	t = time.Date(year, time.Month(month), day, hour, minute, sec, micro*1000, loc)
	if extraDay {
		t = t.AddDate(0, 0, 1)
	}
	return t, aware, nil
}

func daysIn(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func digits(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// parseClock parses HH[:MM[:SS[.f]]] or HH[MM[SS[.f]]].
func parseClock(s string) (h, m, sec, us int, ok bool) {
	frac := ""
	if i := strings.IndexAny(s, ".,"); i >= 0 {
		s, frac = s[:i], s[i+1:]
		if frac == "" {
			return 0, 0, 0, 0, false
		}
		if _, okf := digits(frac); !okf {
			return 0, 0, 0, 0, false
		}
	}
	var parts []string
	if strings.Contains(s, ":") {
		parts = strings.Split(s, ":")
	} else {
		if len(s)%2 != 0 {
			return 0, 0, 0, 0, false
		}
		for i := 0; i < len(s); i += 2 {
			parts = append(parts, s[i:i+2])
		}
	}
	if len(parts) == 0 || len(parts) > 3 {
		return 0, 0, 0, 0, false
	}
	vals := []int{0, 0, 0}
	for i, p := range parts {
		if len(p) != 2 {
			return 0, 0, 0, 0, false
		}
		n, okd := digits(p)
		if !okd {
			return 0, 0, 0, 0, false
		}
		vals[i] = n
	}
	if frac != "" {
		if len(parts) != 3 {
			return 0, 0, 0, 0, false
		}
		if len(frac) > 6 {
			frac = frac[:6]
		}
		frac += strings.Repeat("0", 6-len(frac))
		us, _ = strconv.Atoi(frac)
	}
	return vals[0], vals[1], vals[2], us, true
}

// parseOffset parses +HH, +HHMM, +HH:MM, +HH:MM:SS[.ffffff] into seconds.
func parseOffset(s string) (int, bool) {
	sign := 1
	if s[0] == '-' {
		sign = -1
	}
	h, m, sec, us, ok := parseClock(s[1:])
	if !ok || h > 23 || m > 59 || sec > 59 {
		return 0, false
	}
	_ = us
	return sign * (h*3600 + m*60 + sec), true
}

// ISOFormat is Python's datetime.isoformat(): microseconds only when nonzero,
// the UTC offset only for aware values.
func ISOFormat(t time.Time, aware bool) string {
	out := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		out += fmt.Sprintf(".%06d", us)
	}
	if aware {
		_, off := t.Zone()
		sign := "+"
		if off < 0 {
			sign, off = "-", -off
		}
		out += fmt.Sprintf("%s%02d:%02d", sign, off/3600, off%3600/60)
		if s := off % 60; s != 0 {
			out += fmt.Sprintf(":%02d", s)
		}
	}
	return out
}

// pyQuote is Python's repr() of a str.
func pyQuote(s string) string {
	q := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == rune(q) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}
