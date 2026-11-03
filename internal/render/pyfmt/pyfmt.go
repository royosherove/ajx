// Package pyfmt holds the Python conventions the report renderers need for
// byte-identical output: str()/repr() of JSON-shaped values, float repr and
// format specs, html.escape, the Unicode meaning of \s, \w and \d in Python
// regular expressions, urllib quote, pathlib parent/resolve, and JSON that
// keeps the difference between 1 and 1.0 (json.loads/json.dumps).
package pyfmt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/royosherove/ajx/internal/util"
)

// ---------------------------------------------------------------- characters

// IsSpace is Python's str.isspace() for one character (also what \s matches).
func IsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029,
		0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// IsWord is what \w matches in a Python str pattern: alphanumerics and '_'.
func IsWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// IsDigit is what \d matches in a Python str pattern (Unicode decimal digits).
func IsDigit(r rune) bool { return unicode.Is(unicode.Nd, r) }

// WS is a Go regexp character class body equivalent to Python's \s.
const WS = `\t\n\v\f\r \x{1c}-\x{1f}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

// Digit is a Go regexp class equivalent to Python's \d.
const Digit = `\p{Nd}`

// Strip is Python's str.strip() (no arguments).
func Strip(s string) string { return strings.TrimFunc(s, IsSpace) }

// LStrip is Python's str.lstrip() (no arguments).
func LStrip(s string) string { return strings.TrimLeftFunc(s, IsSpace) }

// RStrip is Python's str.rstrip() (no arguments).
func RStrip(s string) string { return strings.TrimRightFunc(s, IsSpace) }

// CollapseSpace is re.sub(r"\s+", " ", s).
func CollapseSpace(s string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		if IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// Split is Python's str.split() with no arguments.
func Split(s string) []string { return strings.FieldsFunc(s, IsSpace) }

// RuneAt decodes the character at byte offset i (utf8.RuneError, 0 past the end).
func RuneAt(s string, i int) (rune, int) {
	if i < 0 || i >= len(s) {
		return utf8.RuneError, 0
	}
	return utf8.DecodeRuneInString(s[i:])
}

// RuneBefore decodes the character ending at byte offset i.
func RuneBefore(s string, i int) (rune, int) {
	if i <= 0 || i > len(s) {
		return utf8.RuneError, 0
	}
	return utf8.DecodeLastRuneInString(s[:i])
}

// ---------------------------------------------------------------- html

// Escape is Python's html.escape(s, quote).
func Escape(s string, quote bool) string {
	if !strings.ContainsAny(s, "&<>\"'") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '&':
			b.WriteString("&amp;")
		case c == '<':
			b.WriteString("&lt;")
		case c == '>':
			b.WriteString("&gt;")
		case c == '"' && quote:
			b.WriteString("&quot;")
		case c == '\'' && quote:
			b.WriteString("&#x27;")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- str / repr

// Str is Python's str() of a JSON-shaped value (None for nil).
func Str(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		return FloatRepr(t)
	case float32:
		return FloatRepr(float64(t))
	case int64, int, int32, uint64, json.Number:
		return fmt.Sprint(t)
	}
	return Repr(v)
}

// Repr is Python's repr() of a JSON-shaped value. Object keys are sorted
// (Go maps keep no insertion order).
func Repr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return StrRepr(t)
	case bool, float64, float32, int64, int, int32, uint64, json.Number:
		return Str(t)
	case util.M:
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range util.SortedKeys(t) {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(StrRepr(k))
			b.WriteString(": ")
			b.WriteString(Repr(t[k]))
		}
		b.WriteByte('}')
		return b.String()
	case map[string]string:
		return Repr(util.AnyMap(t))
	}
	if l := util.List(v); l != nil {
		parts := make([]string, len(l))
		for i, x := range l {
			parts[i] = Repr(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	if d, err := roundTrip(v); err == nil {
		return Repr(d)
	}
	return fmt.Sprint(v)
}

// StrRepr is Python's repr() of a str.
func StrRepr(s string) string {
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
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == ' ' || (r != utf8.RuneError && unicode.IsPrint(r)):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteRune(q)
	return b.String()
}

func roundTrip(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Decode(raw)
}

// ---------------------------------------------------------------- numbers

// FloatRepr is Python's repr() of a float: the shortest round-tripping
// digits, positional for exponents in [-4, 16), always with a fraction.
func FloatRepr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	return formatShortest(f, 16, true)
}

// formatShortest renders the shortest digits of f positionally when the
// decimal exponent is in [-4, upper), else in Python's exponent form.
func formatShortest(f float64, upper int, keepPoint bool) string {
	e := strconv.FormatFloat(f, 'e', -1, 64) // -d.ddde±XX
	neg := strings.HasPrefix(e, "-")
	e = strings.TrimPrefix(e, "-")
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	var out string
	if exp >= -4 && exp < upper {
		if exp < 0 {
			out = "0." + strings.Repeat("0", -exp-1) + digits
		} else if len(digits) > exp+1 {
			out = digits[:exp+1] + "." + digits[exp+1:]
		} else {
			out = digits + strings.Repeat("0", exp+1-len(digits))
			if keepPoint {
				out += ".0"
			}
		}
	} else {
		out = digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		out += expSuffix(exp)
	}
	if neg {
		out = "-" + out
	}
	return out
}

func expSuffix(exp int) string {
	sign := "+"
	if exp < 0 {
		sign, exp = "-", -exp
	}
	return fmt.Sprintf("e%s%02d", sign, exp)
}

// FormatG is Python's format(f, ".<prec>g") (with thousands separators when
// commas is set, as in "{:,.6g}").
func FormatG(f float64, prec int, commas bool) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	if prec == 0 {
		prec = 1
	}
	e := strconv.FormatFloat(f, 'e', prec-1, 64)
	neg := strings.HasPrefix(e, "-")
	e = strings.TrimPrefix(e, "-")
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	var out string
	if exp >= -4 && exp < prec {
		out = strconv.FormatFloat(math.Abs(f), 'f', prec-1-exp, 64)
		if strings.Contains(out, ".") {
			out = strings.TrimRight(strings.TrimRight(out, "0"), ".")
		}
		if commas {
			intPart, frac, hasFrac := strings.Cut(out, ".")
			out = Commas(intPart)
			if hasFrac {
				out += "." + frac
			}
		}
	} else {
		if strings.Contains(mant, ".") {
			mant = strings.TrimRight(strings.TrimRight(mant, "0"), ".")
		}
		out = mant + expSuffix(exp)
	}
	if neg {
		out = "-" + out
	}
	return out
}

// Commas inserts thousands separators into a string of digits.
func Commas(digits string) string {
	neg := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")
	if len(digits) <= 3 {
		if neg {
			return "-" + digits
		}
		return digits
	}
	var b strings.Builder
	head := len(digits) % 3
	if head > 0 {
		b.WriteString(digits[:head])
	}
	for i := head; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(digits[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Round is Python's round(f, ndigits): correctly rounded, half to even.
func Round(f float64, ndigits int) float64 {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return f
	}
	s := strconv.FormatFloat(f, 'f', ndigits, 64)
	out, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return f
	}
	return out
}

// IsFloat reports whether v is a Python float (not an int).
func IsFloat(v any) bool {
	switch v.(type) {
	case float64, float32:
		return true
	}
	return false
}

// IsInt reports whether v is a Python int (bools excluded).
func IsInt(v any) bool {
	switch v.(type) {
	case int64, int, int32, uint64:
		return true
	case json.Number:
		return !strings.ContainsAny(v.(json.Number).String(), ".eE")
	}
	return false
}

// FloorDiv is Python's a // b for floats; Mod is a % b (sign of the divisor).
func FloorDiv(a, b float64) float64 { return math.Floor(a / b) }

// Mod is Python's float modulo.
func Mod(a, b float64) float64 {
	m := math.Mod(a, b)
	if m != 0 && (m < 0) != (b < 0) {
		m += b
	}
	return m
}

// ---------------------------------------------------------------- JSON

// Decode is json.loads: integer literals become int64 (float64 when they
// overflow), every other number float64, objects util.M, arrays []any.
func Decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var tail any
	if err := dec.Decode(&tail); err != io.EOF {
		return nil, errors.New("Extra data")
	}
	return exact(v), nil
}

func exact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = exact(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = exact(x)
		}
		return t
	case json.Number:
		s := t.String()
		if !strings.ContainsAny(s, ".eE") {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return n
			}
		}
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	return v
}

// ReadJSON is util.read_json with exact numbers: def when the file is missing,
// an error when it cannot be read or parsed.
func ReadJSON(path string, def any) (any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return def, nil
	}
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

// ReadMap reads a JSON object (exact numbers); missing, unreadable, malformed
// or non-object files yield an empty M.
func ReadMap(path string) util.M {
	v, err := ReadJSON(path, nil)
	if err != nil {
		return util.M{}
	}
	return util.MapOr(v)
}

// ReadMapStrict reads a JSON object: missing -> def; malformed -> error;
// a JSON value that is not an object -> (nil, nil) so callers can tell.
func ReadMapStrict(path string, def util.M) (util.M, error) {
	v, err := ReadJSON(path, nil)
	if err != nil {
		return nil, err
	}
	if v == nil {
		if _, statErr := os.Stat(path); statErr != nil {
			return def, nil
		}
		return nil, nil
	}
	m, _ := v.(util.M)
	return m, nil
}

// DumpOpts configure Dumps (Python json.dumps keyword arguments).
type DumpOpts struct {
	Indent  int  // 0 = one line with ", " and ": " separators
	ASCII   bool // ensure_ascii=True (Python's default for json.dumps)
	Ordered bool // keep recorded insertion order (Keys) instead of sorting
}

// Dumps is Python's json.dumps for JSON-shaped values. Object keys are sorted.
func Dumps(v any, o DumpOpts) string {
	var b strings.Builder
	dump(&b, v, o, 0)
	return b.String()
}

// MarshalIndent is json.dumps(data, indent=2, ensure_ascii=False) + "\n".
func MarshalIndent(v any) []byte {
	return []byte(Dumps(v, DumpOpts{Indent: 2}) + "\n")
}

// WriteJSON writes data like util.write_json (atomic, parents created) with
// Python's number spelling: a float that happens to be whole stays 2.0.
func WriteJSON(path string, data any) error {
	return util.WriteFileAtomic(path, MarshalIndent(data), 0o644)
}

func newline(b *strings.Builder, o DumpOpts, depth int) {
	b.WriteByte('\n')
	b.WriteString(strings.Repeat(" ", o.Indent*depth))
}

func dump(b *strings.Builder, v any, o DumpOpts, depth int) {
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
		quote(b, t, o.ASCII)
	case float64:
		dumpFloat(b, t)
	case float32:
		dumpFloat(b, float64(t))
	case int64, int, int32, uint64:
		fmt.Fprint(b, t)
	case json.Number:
		b.WriteString(t.String())
	case util.M:
		if len(t) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		keys := util.SortedKeys(t)
		if o.Ordered {
			keys = Keys(t)
		}
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
				if o.Indent == 0 {
					b.WriteByte(' ')
				}
			}
			if o.Indent > 0 {
				newline(b, o, depth+1)
			}
			quote(b, k, o.ASCII)
			b.WriteString(": ")
			dump(b, t[k], o, depth+1)
		}
		if o.Indent > 0 {
			newline(b, o, depth)
		}
		b.WriteByte('}')
	case map[string]string:
		dump(b, util.AnyMap(t), o, depth)
	default:
		l := util.List(v)
		if l == nil {
			if _, isList := v.([]any); !isList {
				if d, err := roundTrip(v); err == nil {
					dump(b, d, o, depth)
				} else {
					b.WriteString("null")
				}
				return
			}
		}
		if len(l) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, x := range l {
			if i > 0 {
				b.WriteByte(',')
				if o.Indent == 0 {
					b.WriteByte(' ')
				}
			}
			if o.Indent > 0 {
				newline(b, o, depth+1)
			}
			dump(b, x, o, depth+1)
		}
		if o.Indent > 0 {
			newline(b, o, depth)
		}
		b.WriteByte(']')
	}
}

func dumpFloat(b *strings.Builder, f float64) {
	switch {
	case math.IsInf(f, 1):
		b.WriteString("Infinity")
	case math.IsInf(f, -1):
		b.WriteString("-Infinity")
	case math.IsNaN(f):
		b.WriteString("NaN")
	default:
		b.WriteString(FloatRepr(f))
	}
}

func quote(b *strings.Builder, s string, ascii bool) {
	b.WriteByte('"')
	for _, r := range s {
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
			case r < 0x20 || (ascii && r > 0x7e):
				if r >= 0x10000 {
					r1, r2 := utf16Pair(r)
					fmt.Fprintf(b, `\u%04x\u%04x`, r1, r2)
				} else {
					fmt.Fprintf(b, `\u%04x`, r)
				}
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

func utf16Pair(r rune) (rune, rune) {
	r -= 0x10000
	return 0xd800 + (r>>10)&0x3ff, 0xdc00 + r&0x3ff
}

// ---------------------------------------------------------------- urls

// Quote is urllib.parse.quote(s, safe=safe).
func Quote(s, safe string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x80 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexByte("_.-~", c) >= 0 || strings.IndexByte(safe, c) >= 0) {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// ---------------------------------------------------------------- paths

// CleanPath is str(Path(p)) for a POSIX path: repeated separators and "."
// components collapse and a trailing separator goes ("" becomes ".").
func CleanPath(p string) string {
	if p == "" {
		return "."
	}
	lead := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		lead = "//"
	case strings.HasPrefix(p, "/"):
		lead = "/"
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	out := lead + strings.Join(parts, "/")
	if out == "" {
		return "."
	}
	return out
}

// Parent is str(Path(p).parent).
func Parent(p string) string {
	c := CleanPath(p)
	if c == "/" || c == "//" || c == "." {
		return c
	}
	i := strings.LastIndex(c, "/")
	switch {
	case i < 0:
		return "."
	case i == 0:
		return "/"
	case i == 1 && strings.HasPrefix(c, "//"):
		return "//"
	}
	return c[:i]
}

// Name is Path(p).name.
func Name(p string) string {
	c := CleanPath(p)
	if c == "/" || c == "//" || c == "." {
		return ""
	}
	return c[strings.LastIndex(c, "/")+1:]
}

// ExpandUser is Path(p).expanduser().
func ExpandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h := util.Home(); h != "" {
			return h + p[1:]
		}
	}
	return p
}

// Resolve is str(Path(p).resolve()) (strict=False): absolute, symlinks of
// the existing prefix resolved, ".." applied.
func Resolve(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return CleanPath(p)
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	dir, rest := abs, ""
	for {
		parent := filepath.Dir(dir)
		rest = filepath.Join(filepath.Base(dir), rest)
		if parent == dir {
			return abs
		}
		dir = parent
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
	}
}

// SplitLines is Python's str.splitlines() (no keepends).
func SplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, s[start:i])
			if r == '\r' && i+1 < len(s) && s[i+1] == '\n' {
				size = 2
			}
			start = i + size
		}
		i += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// Equal is Python == for JSON-shaped values (1 == 1.0 == True).
func Equal(a, b any) bool {
	an, aok := numeric(a)
	bn, bok := numeric(b)
	if aok || bok {
		return aok && bok && an == bn
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case util.M:
		y, ok := b.(util.M)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, present := y[k]
			if !present || !Equal(v, w) {
				return false
			}
		}
		return true
	}
	if xl, yl := util.List(a), util.List(b); xl != nil || yl != nil {
		if len(xl) != len(yl) {
			return false
		}
		for i := range xl {
			if !Equal(xl[i], yl[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// numeric is a Python number (bools count as 0/1).
func numeric(v any) (float64, bool) {
	if b, ok := v.(bool); ok {
		if b {
			return 1, true
		}
		return 0, true
	}
	return util.Num(v)
}

// Number is numeric(): Python ints, floats and bools as float64.
func Number(v any) (float64, bool) { return numeric(v) }

// ---------------------------------------------------------------- errors

// Error is an error carrying the Python exception class name it ports
// (runner and CLI format f"{type(exc).__name__}: {exc}" through TypeName).
type Error struct {
	Type string
	Msg  string
}

func (e *Error) Error() string    { return e.Msg }
func (e *Error) TypeName() string { return e.Type }

// Errorf builds an Error of the given Python type.
func Errorf(typ, format string, a ...any) error {
	return &Error{Type: typ, Msg: fmt.Sprintf(format, a...)}
}

// KeyError is Python's KeyError(key): str() is the key's repr.
func KeyError(key string) error { return &Error{Type: "KeyError", Msg: StrRepr(key)} }

// TypeName is type(exc).__name__ for an error.
func TypeName(err error) string {
	var tn interface{ TypeName() string }
	if errors.As(err, &tn) {
		return tn.TypeName()
	}
	if util.Interrupted(err) {
		return "KeyboardInterrupt"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "FileNotFoundError"
		case errors.Is(err, fs.ErrExist):
			return "FileExistsError"
		case errors.Is(err, fs.ErrPermission):
			return "PermissionError"
		}
		return "OSError"
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return "JSONDecodeError"
	}
	return "RuntimeError"
}

// TypeOf is type(v).__name__ for a JSON-shaped value.
func TypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case string:
		return "str"
	case bool:
		return "bool"
	case float64, float32:
		return "float"
	case int64, int, int32, uint64:
		return "int"
	case util.M:
		return "dict"
	}
	if util.List(v) != nil {
		return "list"
	}
	return fmt.Sprintf("%T", v)
}

// Index is a strict v[key] for the renderers: it panics with the KeyError or
// TypeError Python raises; Recover turns that panic back into an error.
func Index(v any, key string) any {
	m, ok := v.(util.M)
	if !ok {
		panic(&Error{Type: "TypeError", Msg: "'" + TypeOf(v) + "' object is not subscriptable"})
	}
	x, present := m[key]
	if !present {
		panic(KeyError(key))
	}
	return x
}

// Recover converts a panic raised by Index into *err; other panics propagate.
func Recover(err *error) {
	if r := recover(); r != nil {
		if e, ok := r.(*Error); ok {
			*err = e
			return
		}
		panic(r)
	}
}

// DumpsRaw re-encodes raw JSON text like Python's json.dumps(json.loads(raw)),
// keeping the document's key order (Python dicts preserve it).
func DumpsRaw(raw []byte, o DumpOpts) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b strings.Builder
	var walk func(depth int) error
	walk = func(depth int) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case json.Delim:
			closing := json.Delim('}')
			if t == '[' {
				closing = ']'
			}
			b.WriteRune(rune(t))
			n := 0
			for dec.More() {
				if n > 0 {
					b.WriteByte(',')
					if o.Indent == 0 {
						b.WriteByte(' ')
					}
				}
				if o.Indent > 0 {
					newline(&b, o, depth+1)
				}
				if t == '{' {
					key, err := dec.Token()
					if err != nil {
						return err
					}
					quote(&b, key.(string), o.ASCII)
					b.WriteString(": ")
				}
				if err := walk(depth + 1); err != nil {
					return err
				}
				n++
			}
			if _, err := dec.Token(); err != nil {
				return err
			}
			if n > 0 && o.Indent > 0 {
				newline(&b, o, depth)
			}
			b.WriteRune(rune(closing))
		case json.Number:
			dump(&b, exact(t), o, depth)
		default:
			dump(&b, t, o, depth)
		}
		return nil
	}
	if err := walk(0); err != nil {
		return "", err
	}
	return b.String(), nil
}

// ---------------------------------------------------------------- key order

// Python dicts keep insertion order; Go maps do not. A Scope remembers the
// key order of the objects it decodes (and of copies registered with it) so
// renderers can iterate a dictionary the way Python would. Lookups fall back
// to sorted keys for objects no open scope knows about. A scope keeps its
// objects alive until Close, so a recorded address cannot be reused.
type Scope struct {
	objects []util.M
}

var (
	orderMu  sync.Mutex
	orderTab = map[uintptr][]string{}
)

func mapAddr(m util.M) uintptr { return reflect.ValueOf(m).Pointer() }

// NewScope opens a key-order scope; call Close when rendering is done.
func NewScope() *Scope { return &Scope{} }

// Close forgets the scope's objects.
func (s *Scope) Close() {
	if s == nil {
		return
	}
	orderMu.Lock()
	for _, m := range s.objects {
		delete(orderTab, mapAddr(m))
	}
	orderMu.Unlock()
	s.objects = nil
}

// Register records keys as m's insertion order.
func (s *Scope) Register(m util.M, keys []string) {
	if s == nil || m == nil {
		return
	}
	orderMu.Lock()
	orderTab[mapAddr(m)] = keys
	orderMu.Unlock()
	s.objects = append(s.objects, m)
}

// Keys returns m's keys in recorded insertion order: recorded keys still
// present, then keys added later (sorted). Unknown objects: sorted keys.
func Keys(m util.M) []string {
	if m == nil {
		return nil
	}
	orderMu.Lock()
	recorded, ok := orderTab[mapAddr(m)]
	orderMu.Unlock()
	if !ok {
		return util.SortedKeys(m)
	}
	out := make([]string, 0, len(m))
	seen := make(map[string]bool, len(m))
	for _, k := range recorded {
		if _, present := m[k]; present && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	if len(out) < len(m) {
		var extra []string
		for k := range m {
			if !seen[k] {
				extra = append(extra, k)
			}
		}
		sort.Strings(extra)
		out = append(out, extra...)
	}
	return out
}

// DecodeOrdered is Decode recording every object's key order in s.
func (s *Scope) DecodeOrdered(data []byte) (any, error) {
	if s == nil {
		return Decode(data)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var walk func() (any, error)
	walk = func() (any, error) {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case json.Delim:
			if t == '[' {
				out := []any{}
				for dec.More() {
					v, err := walk()
					if err != nil {
						return nil, err
					}
					out = append(out, v)
				}
				_, err := dec.Token()
				return out, err
			}
			m := util.M{}
			var keys []string
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := kt.(string)
				v, err := walk()
				if err != nil {
					return nil, err
				}
				if _, dup := m[k]; !dup {
					keys = append(keys, k)
				}
				m[k] = v
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			s.Register(m, keys)
			return m, nil
		case json.Number:
			return exact(t), nil
		}
		return tok, nil
	}
	v, err := walk()
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("Extra data")
	}
	return v, nil
}

// ReadJSON is pyfmt.ReadJSON recording key order in s.
func (s *Scope) ReadJSON(path string, def any) (any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return def, nil
	}
	if err != nil {
		return nil, err
	}
	return s.DecodeOrdered(data)
}

// KnownKeys returns m's recorded insertion order, if a scope recorded one.
func KnownKeys(m util.M) ([]string, bool) {
	orderMu.Lock()
	_, ok := orderTab[mapAddr(m)]
	orderMu.Unlock()
	if !ok {
		return nil, false
	}
	return Keys(m), true
}

// IntStr is str(int(f)) for a float (truncation toward zero, any magnitude).
func IntStr(f float64) string {
	t := math.Trunc(f)
	if math.Abs(t) < 9e18 {
		return strconv.FormatInt(int64(t), 10)
	}
	return new(big.Float).SetFloat64(t).Text('f', 0)
}
