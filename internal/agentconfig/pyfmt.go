package agentconfig

// Python-compatible text helpers. Hashes, configuration files, launch
// arguments and error messages must match the Python implementation byte for
// byte, so these reproduce json.dumps, repr, fnmatch,
// str.splitlines/strip/casefold/isprintable and urllib.parse behavior.

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/royosherove/ajx/internal/util"
)

// jsonOpts mirrors the json.dumps arguments used by agent_configuration.py.
type jsonOpts struct {
	ascii   bool // ensure_ascii
	indent  int  // 0 = no newlines
	compact bool // separators=(",", ":")
	sorted  bool // sort_keys
	order   []string
}

// pyDumps is json.dumps for JSON-shaped values. Unsorted maps fall back to
// opts.order and then sorted keys (Go maps have no insertion order).
func pyDumps(v any, o jsonOpts) string {
	var b strings.Builder
	o.write(&b, v, 0)
	return b.String()
}

func (o jsonOpts) seps() (item, key string) {
	switch {
	case o.compact:
		return ",", ":"
	case o.indent > 0:
		return ",", ": "
	}
	return ", ", ": "
}

func (o jsonOpts) newline(b *strings.Builder, level int) {
	if o.indent > 0 {
		b.WriteByte('\n')
		b.WriteString(strings.Repeat(" ", o.indent*level))
	}
}

func (o jsonOpts) keys(m map[string]any) []string {
	if o.sorted {
		return util.SortedKeys(m)
	}
	return orderedKeys(m, o.order)
}

// orderedKeys lists the keys named in order first (when present), then the rest sorted.
func orderedKeys[V any](m map[string]V, order []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range order {
		if _, ok := m[k]; ok && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	for _, k := range util.SortedKeys(m) {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

func (o jsonOpts) write(b *strings.Builder, v any, level int) {
	item, key := o.seps()
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
		b.WriteString(pyJSONString(t, o.ascii))
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case uint64:
		b.WriteString(strconv.FormatUint(t, 10))
	case float64:
		b.WriteString(util.PyFloat(t))
	case map[string]string:
		m := make(map[string]any, len(t))
		for k, s := range t {
			m[k] = s
		}
		o.write(b, m, level)
	case map[string]any:
		if len(t) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, k := range o.keys(t) {
			if i > 0 {
				b.WriteString(item)
			}
			o.newline(b, level+1)
			b.WriteString(pyJSONString(k, o.ascii))
			b.WriteString(key)
			o.write(b, t[k], level+1)
		}
		o.newline(b, level)
		b.WriteByte('}')
	default:
		l := util.List(v)
		if l == nil {
			if ms, ok := v.([]util.M); ok {
				l = util.List(ms)
			} else {
				b.WriteString(util.PyJSON(v))
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
				b.WriteString(item)
			}
			o.newline(b, level+1)
			o.write(b, x, level+1)
		}
		o.newline(b, level)
		b.WriteByte(']')
	}
}

// pyJSONString is a JSON string literal as Python's json encoder writes it.
func pyJSONString(s string, ascii bool) string {
	var b strings.Builder
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
				if r > 0xffff {
					r -= 0x10000
					fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800|((r>>10)&0x3ff), 0xdc00|(r&0x3ff))
				} else {
					fmt.Fprintf(&b, `\u%04x`, r)
				}
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// pyRepr is Python's repr() of a str.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteByte(quote)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x7f && r >= 0x20:
			b.WriteRune(r)
		case pyPrintable(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// pyListRepr is repr() of a list of str.
func pyListRepr(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = pyRepr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// pyPrintable is str.isprintable() for one character.
func pyPrintable(r rune) bool {
	if r == ' ' {
		return true
	}
	return unicode.IsPrint(r)
}

func pyIsPrintable(s string) bool {
	for _, r := range s {
		if !pyPrintable(r) {
			return false
		}
	}
	return true
}

// pyIsSpace is str.isspace() for one character.
func pyIsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// pyStrip is str.strip() without arguments.
func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pyLstrip is str.lstrip() without arguments.
func pyLstrip(s string) string { return strings.TrimLeftFunc(s, pyIsSpace) }

// pyWS is Python's \s (Unicode whitespace) as a regexp character-class body.
const pyWS = `\t\n\v\f\r \x{1c}-\x{1f}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

// pyRegexp compiles a pattern written with Python's Unicode \s as `\s`.
func pyRegexp(pattern string) *regexp.Regexp {
	pattern = strings.ReplaceAll(pattern, `[^\s`, `[^`+pyWS)
	pattern = strings.ReplaceAll(pattern, `\s`, `[`+pyWS+`]`)
	return regexp.MustCompile(pattern)
}

// pySplitlines is str.splitlines() without keepends.
func pySplitlines(s string) []string {
	var out []string
	start := 0
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\r':
			out = append(out, string(runes[start:i]))
			if i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
			start = i + 1
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			out = append(out, string(runes[start:i]))
			start = i + 1
		}
	}
	if start < len(runes) {
		out = append(out, string(runes[start:]))
	}
	return out
}

// fullFold holds the multi-character case foldings that str.casefold applies
// to characters common in file names.
var fullFold = map[rune]string{
	'ß': "ss", 'ẞ': "ss", 'ŉ': "ʼn", 'ǰ': "ǰ", 'ΐ': "ΐ", 'ΰ': "ΰ", 'և': "եւ",
	'ﬀ': "ff", 'ﬁ': "fi", 'ﬂ': "fl", 'ﬃ': "ffi", 'ﬄ': "ffl", 'ﬅ': "st", 'ﬆ': "st", 'İ': "i̇",
}

// casefold approximates str.casefold(): full folding for the characters in
// fullFold, simple Unicode folding otherwise.
func casefold(s string) string {
	var b strings.Builder
	for _, r := range s {
		if f, ok := fullFold[r]; ok {
			b.WriteString(f)
			continue
		}
		b.WriteRune(unicode.ToLower(unicode.ToUpper(r)))
	}
	return b.String()
}

// fnmatch is fnmatch.fnmatch on POSIX (case-sensitive; '*' also matches '/').
func fnmatch(name, pattern string) bool {
	return globMatch([]rune(name), []rune(pattern))
}

func globMatch(name, pat []rune) bool {
	for len(pat) > 0 {
		switch pat[0] {
		case '*':
			for len(pat) > 0 && pat[0] == '*' {
				pat = pat[1:]
			}
			if len(pat) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if globMatch(name[i:], pat) {
					return true
				}
			}
			return false
		case '?':
			if len(name) == 0 {
				return false
			}
			name, pat = name[1:], pat[1:]
		case '[':
			end, ok := classEnd(pat)
			if !ok { // an unterminated '[' is literal
				if len(name) == 0 || name[0] != '[' {
					return false
				}
				name, pat = name[1:], pat[1:]
				continue
			}
			if len(name) == 0 || !classMatch(name[0], pat[1:end]) {
				return false
			}
			name, pat = name[1:], pat[end+1:]
		default:
			if len(name) == 0 || name[0] != pat[0] {
				return false
			}
			name, pat = name[1:], pat[1:]
		}
	}
	return len(name) == 0
}

// classEnd finds the ']' closing the set that starts at pat[0] == '['.
func classEnd(pat []rune) (int, bool) {
	j := 1
	if j < len(pat) && pat[j] == '!' {
		j++
	}
	if j < len(pat) && pat[j] == ']' {
		j++
	}
	for j < len(pat) && pat[j] != ']' {
		j++
	}
	return j, j < len(pat)
}

func classMatch(c rune, set []rune) bool {
	negate := len(set) > 0 && set[0] == '!'
	if negate {
		set = set[1:]
	}
	matched := false
	for i := 0; i < len(set); i++ {
		if i+2 < len(set) && set[i+1] == '-' {
			if set[i] <= c && c <= set[i+2] {
				matched = true
			}
			i += 2
			continue
		}
		if set[i] == c {
			matched = true
		}
	}
	return matched != negate
}

// pyUnquote is urllib.parse.unquote (UTF-8, errors="replace").
func pyUnquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var out strings.Builder
	var pending []byte
	flush := func() {
		for len(pending) > 0 {
			r, size := utf8.DecodeRune(pending)
			out.WriteRune(r) // invalid bytes become U+FFFD
			pending = pending[size:]
		}
	}
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			v, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			pending = append(pending, byte(v))
			i += 3
			continue
		}
		flush()
		_, size := utf8.DecodeRuneInString(s[i:])
		out.WriteString(s[i : i+size])
		i += size
	}
	flush()
	return out.String()
}

func isHex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

// splitURL is the scheme/netloc/path part of urllib.parse.urlsplit; an error
// means Python's urlsplit raises ValueError.
type splitURL struct{ scheme, netloc, path string }

const schemeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

func urlsplit(url string) (splitURL, error) {
	url = strings.TrimLeft(url, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\v\f\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	for _, c := range []string{"\t", "\r", "\n"} {
		url = strings.ReplaceAll(url, c, "")
	}
	var u splitURL
	if i := strings.IndexByte(url, ':'); i > 0 && isASCIIAlpha(url[0]) {
		ok := true
		for _, c := range url[:i] {
			if !strings.ContainsRune(schemeChars, c) {
				ok = false
				break
			}
		}
		if ok {
			u.scheme, url = strings.ToLower(url[:i]), url[i+1:]
		}
	}
	if strings.HasPrefix(url, "//") {
		delim := len(url)
		for _, c := range "/?#" {
			if j := strings.IndexRune(url[2:], c); j >= 0 && j+2 < delim {
				delim = j + 2
			}
		}
		u.netloc, url = url[2:delim], url[delim:]
		open, closed := strings.Contains(u.netloc, "["), strings.Contains(u.netloc, "]")
		if open != closed {
			return u, fmt.Errorf("Invalid IPv6 URL")
		}
		if open && closed {
			if err := checkBracketedNetloc(u.netloc); err != nil {
				return u, err
			}
		}
	}
	if i := strings.IndexByte(url, '#'); i >= 0 {
		url = url[:i]
	}
	if i := strings.IndexByte(url, '?'); i >= 0 {
		url = url[:i]
	}
	u.path = url
	return u, nil
}

func isASCIIAlpha(c byte) bool { return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') }

var futureHost = regexp.MustCompile(`^v[a-fA-F0-9]+\..+$`)

func checkBracketedNetloc(netloc string) error {
	host := netloc[strings.LastIndex(netloc, "@")+1:]
	before, bracketed, ok := strings.Cut(host, "[")
	if !ok {
		return nil
	}
	if before != "" {
		return fmt.Errorf("Invalid IPv6 URL")
	}
	hostname, port, _ := strings.Cut(bracketed, "]")
	if port != "" && !strings.HasPrefix(port, ":") {
		return fmt.Errorf("Invalid IPv6 URL")
	}
	if strings.HasPrefix(hostname, "v") {
		if !futureHost.MatchString(hostname) {
			return fmt.Errorf("IPvFuture address is invalid")
		}
		return nil
	}
	addr, err := netip.ParseAddr(hostname)
	if err != nil || addr.Is4() {
		return fmt.Errorf("An IPv4 address cannot be in brackets")
	}
	return nil
}

// sortedStrings returns a sorted copy.
func sortedStrings(items []string) []string {
	out := append([]string(nil), items...)
	sort.Strings(out)
	return out
}
