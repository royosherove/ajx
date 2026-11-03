package environments

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/royosherove/ajx/internal/util"
)

// pyDumps is Python json.dumps(v, sort_keys=True, ensure_ascii=True) with the
// given separators. Hashes recorded in plans (profile_sha256, mount sha256)
// must match the Python CLI byte for byte.
func pyDumps(v any, compact bool) string {
	var b strings.Builder
	item, key := ", ", ": "
	if compact {
		item, key = ",", ":"
	}
	pyDump(&b, v, item, key)
	return b.String()
}

func pyDump(b *strings.Builder, v any, item, key string) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		b.WriteString(pyJSONString(t))
	case float64:
		b.WriteString(pyJSONFloat(t))
	case float32:
		b.WriteString(pyJSONFloat(float64(t)))
	case int:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int32:
		b.WriteString(strconv.FormatInt(int64(t), 10))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case uint64:
		b.WriteString(strconv.FormatUint(t, 10))
	case util.M:
		keys := util.SortedKeys(t)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(item)
			}
			b.WriteString(pyJSONString(k))
			b.WriteString(key)
			pyDump(b, t[k], item, key)
		}
		b.WriteByte('}')
	case map[string]string:
		keys := util.SortedKeys(t)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(item)
			}
			b.WriteString(pyJSONString(k))
			b.WriteString(key)
			b.WriteString(pyJSONString(t[k]))
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				b.WriteString(item)
			}
			pyDump(b, x, item, key)
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				b.WriteString(item)
			}
			b.WriteString(pyJSONString(x))
		}
		b.WriteByte(']')
	case []util.M:
		b.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				b.WriteString(item)
			}
			pyDump(b, x, item, key)
		}
		b.WriteByte(']')
	default:
		pyDump(b, util.Normalize(v), item, key)
	}
}

// pyJSONString is a JSON string literal as json.dumps(ensure_ascii=True)
// writes it. Bytes that are not UTF-8 are written as the lone surrogates
// Python's os.fsdecode (surrogateescape) produces.
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&b, `\u%04x`, 0xdc00+int(s[i]))
			i++
			continue
		}
		i += size
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20 || (r > 0x7e && r <= 0xffff):
				if r == 0x7f {
					b.WriteRune(r)
				} else {
					fmt.Fprintf(&b, `\u%04x`, r)
				}
			case r > 0xffff:
				r -= 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyJSONFloat is a float as json.dumps writes it (repr; non-finite as JavaScript names).
func pyJSONFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case math.IsNaN(f):
		return "NaN"
	}
	return pyFloatRepr(f)
}

// pyFloatRepr is Python repr(float) / str(float): the shortest round-trip
// digits, positional for exponents in [-4, 16), otherwise scientific.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	mant, expText, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mant, ".", "", 1)
	if exp >= -4 && exp < 16 {
		var out string
		if exp >= 0 {
			if len(digits) <= exp+1 {
				out = digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
			} else {
				out = digits[:exp+1] + "." + digits[exp+1:]
			}
		} else {
			out = "0." + strings.Repeat("0", -exp-1) + digits
		}
		return sign + out
	}
	e := "+"
	if exp < 0 {
		e, exp = "-", -exp
	}
	return fmt.Sprintf("%s%se%s%02d", sign, mant, e, exp)
}

// pyNumber formats a validation bound as Python's f-string would.
func pyNumber(f float64, integer bool) string {
	if integer || f == math.Trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return pyFloatRepr(f)
}

// pyRepr is Python repr() of a string.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
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
	b.WriteByte(quote)
	return b.String()
}

// sortedEntries sorts tree manifest entries like Python sorted(list_of_lists):
// by the first element (relative paths are unique).
func sortedEntries(entries [][]any) []any {
	sort.SliceStable(entries, func(i, j int) bool {
		return util.S(entries[i][0]) < util.S(entries[j][0])
	})
	out := make([]any, len(entries))
	for i, e := range entries {
		out[i] = e
	}
	return out
}
