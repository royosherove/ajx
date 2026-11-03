package spec

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/royosherove/ajx/internal/util"
)

// Repr is Python's repr() for JSON-shaped values: 'str', None, True,
// [1, 'a'], {'k': 'v'} (keys sorted). Used for `{x!r}` in messages.
func Repr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return reprStr(t)
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		return FloatRepr(t)
	case float32:
		return FloatRepr(float64(t))
	case int64, int, int32, uint64:
		return fmt.Sprint(t)
	case util.M:
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range util.SortedKeys(t) {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(reprStr(k))
			b.WriteString(": ")
			b.WriteString(Repr(t[k]))
		}
		b.WriteByte('}')
		return b.String()
	case map[string]string:
		return Repr(util.AnyMap(t))
	case []string:
		return Repr(util.List(t))
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			parts[i] = Repr(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	if l := util.List(v); l != nil {
		return Repr(l)
	}
	return fmt.Sprint(v)
}

// PyStr is Python's str() (an f-string `{x}`): strings verbatim, everything
// else as Repr.
func PyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return Repr(v)
}

// ReprList is repr() of a list of strings: ['a', 'b'].
func ReprList(items []string) string {
	return Repr(util.List(items))
}

// FloatRepr is Python's repr() of a float: 1.0, 0.25, 1e-05, 1e+16, inf.
func FloatRepr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	abs := math.Abs(f)
	if abs != 0 && (abs < 1e-4 || abs >= 1e16) {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[:1]
		digits := strings.TrimLeft(exp[1:], "0")
		if len(digits) < 2 {
			digits = strings.Repeat("0", 2-len(digits)) + digits
		}
		return mant + "e" + sign + digits
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

func reprStr(s string) string {
	q := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteRune(q)
	for _, r := range s {
		switch {
		case r == q || r == '\\':
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
		case r < 0xa0 && r > 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r > 0x7f && !unicode.IsPrint(r) && r != ' ':
			if r > 0xffff {
				fmt.Fprintf(&b, `\U%08x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteRune(q)
	return b.String()
}
