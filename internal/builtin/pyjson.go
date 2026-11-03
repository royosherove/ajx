package builtin

// Python-faithful value handling for the normalizers. Normalized telemetry
// must match what the Python adapters produce for the same raw stream, so:
//   - JSON integer literals decode to int64 and others to float64 (Python
//     json.loads keeps int and float apart; util.DecodeJSON yields float64),
//   - object key order is remembered per decode scope, so json.dumps-style
//     summaries and "first key" lookups see the source order,
//   - text helpers follow Python's str.strip/split, str() and repr().

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/royosherove/ajx/internal/util"
)

type M = util.M

// scope remembers the key order of the objects it decoded or built. It lives
// for one normalize/progress/report call, so map identities cannot be reused
// while it is alive.
type scope struct {
	order map[uintptr][]string
}

func newScope() *scope { return &scope{order: map[uintptr][]string{}} }

func mapID(m M) uintptr { return reflect.ValueOf(m).Pointer() }

// keys returns m's keys in source order when known, else sorted.
func (s *scope) keys(m M) []string {
	if s != nil {
		if ks, ok := s.order[mapID(m)]; ok && len(ks) == len(m) {
			return ks
		}
	}
	return util.SortedKeys(m)
}

// obj builds an M from ordered key/value pairs and remembers the order.
func (s *scope) obj(pairs ...any) M {
	m := M{}
	var ks []string
	for i := 0; i+1 < len(pairs); i += 2 {
		k := pairs[i].(string)
		if _, dup := m[k]; !dup {
			ks = append(ks, k)
		}
		m[k] = pairs[i+1]
	}
	if s != nil {
		s.order[mapID(m)] = ks
	}
	return m
}

var errExtra = errors.New("extra data")

// decode parses one JSON document like json.loads.
func (s *scope) decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := s.value(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errExtra
	}
	return v, nil
}

func (s *scope) value(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			m := M{}
			var ks []string
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, errors.New("invalid object key")
				}
				v, err := s.value(dec)
				if err != nil {
					return nil, err
				}
				if _, dup := m[k]; !dup {
					ks = append(ks, k)
				}
				m[k] = v
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			if s != nil {
				s.order[mapID(m)] = ks
			}
			return m, nil
		case '[':
			out := []any{}
			for dec.More() {
				v, err := s.value(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return out, nil
		}
		return nil, errors.New("unexpected delimiter")
	case json.Number:
		return pyNumber(string(t)), nil
	default:
		return t, nil // string, bool, nil
	}
}

func pyNumber(lit string) any {
	if !strings.ContainsAny(lit, ".eE") {
		if i, err := strconv.ParseInt(lit, 10, 64); err == nil {
			return i
		}
	}
	f, _ := strconv.ParseFloat(lit, 64)
	return f
}

// readJSONL is util.read_jsonl: blank lines skipped, unparsable lines become
// {"_unparsed": line}; a missing file yields nil.
func (s *scope) readJSONL(path string) []any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rows []any
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			if pyStrip(line) != "" {
				v, derr := s.decode([]byte(line))
				if derr != nil {
					v = M{"_unparsed": line}
				}
				rows = append(rows, v)
			}
		}
		if err != nil {
			break
		}
	}
	return rows
}

// rec is one stream_records item: (arrival ts, parsed JSON or nil, raw line).
type rec struct {
	at   any
	ev   any
	line string
}

// evMap is the parsed event when it is an object (Python `ev` dict), else nil.
func (r rec) evMap() M { return util.Map(r.ev) }

// records is base.stream_records over a run_streaming capture.
func (s *scope) records(rawPath string) []rec {
	var out []rec
	for _, row := range s.readJSONL(rawPath) {
		m := util.MapOr(row)
		line, isStr := m["line"].(string)
		var parsed any
		if isStr {
			if v, err := s.decode([]byte(line)); err == nil {
				parsed = v
			}
		}
		out = append(out, rec{at: m["at"], ev: parsed, line: line})
	}
	return out
}

// ---------------------------------------------------------------- dumps

// dumps is json.dumps(v) with Python's default separators; ascii selects
// ensure_ascii (True is Python's default).
func (s *scope) dumps(v any, ascii bool) string {
	var b strings.Builder
	s.dump(&b, v, ascii)
	return b.String()
}

func (s *scope) dump(b *strings.Builder, v any, ascii bool) {
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
		b.WriteString(pyQuote(t, ascii))
	case float64:
		b.WriteString(util.PyFloat(t))
	case int64, int, int32:
		b.WriteString(fmt.Sprint(t))
	case M:
		b.WriteByte('{')
		for i, k := range s.keys(t) {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyQuote(k, ascii))
			b.WriteString(": ")
			s.dump(b, t[k], ascii)
		}
		b.WriteByte('}')
	case map[string]string:
		b.WriteByte('{')
		for i, k := range util.SortedKeys(t) {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyQuote(k, ascii) + ": " + pyQuote(t[k], ascii))
		}
		b.WriteByte('}')
	default:
		if l := util.List(v); l != nil {
			b.WriteByte('[')
			for i, x := range l {
				if i > 0 {
					b.WriteString(", ")
				}
				s.dump(b, x, ascii)
			}
			b.WriteByte(']')
			return
		}
		b.WriteString(util.PyJSON(v))
	}
}

// pyQuote is a JSON string literal as Python's json.dumps writes it.
func pyQuote(str string, ascii bool) string {
	if !ascii {
		return util.PyQuote(str)
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range str {
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
			case r >= 0x20 && r <= 0x7e:
				b.WriteRune(r)
			case r < 0x10000:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				r -= 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ---------------------------------------------------------------- str/repr

// pyStr is Python str(v).
func (s *scope) pyStr(v any) string {
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
		return pyFloatStr(t)
	case int64, int, int32:
		return fmt.Sprint(t)
	}
	return s.repr(v)
}

func pyFloatStr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	return util.PyFloat(f)
}

// repr is Python repr(v) for JSON-shaped values.
func (s *scope) repr(v any) string {
	switch t := v.(type) {
	case nil, bool, float64, int64, int, int32:
		return s.pyStr(v)
	case string:
		return pyReprStr(t)
	case M:
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range s.keys(t) {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyReprStr(k) + ": " + s.repr(t[k]))
		}
		b.WriteByte('}')
		return b.String()
	}
	if l := util.List(v); l != nil || v == nil {
		parts := make([]string, len(l))
		for i, x := range l {
			parts[i] = s.repr(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprint(v)
}

func pyReprStr(str string) string {
	quote := byte('\'')
	if strings.Contains(str, "'") && !strings.Contains(str, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range str {
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
		case !unicode.IsPrint(r): // Python str.isprintable
			switch {
			case r < 0x100:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r < 0x10000:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// ---------------------------------------------------------------- text

func pySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// pyStrip is str.strip().
func pyStrip(s string) string { return strings.TrimFunc(s, pySpace) }

// pyRStrip is str.rstrip().
func pyRStrip(s string) string { return strings.TrimRightFunc(s, pySpace) }

// pySplit is str.split() (whitespace runs, no empty fields).
func pySplit(s string) []string { return strings.FieldsFunc(s, pySpace) }

// excerpt is base.excerpt: JSON (ensure_ascii=False) for non-strings, stripped, clipped.
func (s *scope) excerpt(value any, limit int) string {
	if value == nil {
		return ""
	}
	str, ok := value.(string)
	if !ok {
		str = s.dumps(value, false)
	}
	str = pyStrip(str)
	n := util.RuneLen(str)
	if n <= limit {
		return str
	}
	return util.Clip(str, limit) + fmt.Sprintf("... [%d more chars]", n-limit)
}

var summaryKeys = []string{"command", "cmd", "file_path", "path", "absolute_path", "pattern", "url", "query", "description"}

// summarize is base.summarize_input.
func (s *scope) summarize(data any) string {
	if m, ok := data.(M); ok {
		for _, key := range summaryKeys {
			val, present := m[key]
			if !present {
				continue
			}
			var str string
			if l, isList := val.([]any); isList {
				parts := make([]string, len(l))
				for i, x := range l {
					parts[i] = s.pyStr(x)
				}
				str = strings.Join(parts, " ")
			} else {
				str = s.pyStr(val)
			}
			return s.excerpt(key+": "+str, 300)
		}
	}
	return s.excerpt(data, 300)
}

// toolEvent is base.tool_event.
func (s *scope) toolEvent(tid any, name any, rawInput any, at any, messageID any) M {
	return M{"kind": "tool", "tool_use_id": tid, "name": name, "input": s.summarize(rawInput),
		"input_raw": rawInput, "at": at, "end": nil, "is_error": nil, "output": "", "message_id": messageID}
}

// textEvent is base.text_event.
func (s *scope) textEvent(text string, at any, messageID any) M {
	return M{"kind": "text", "text": s.excerpt(text, 1200), "at": at, "message_id": messageID}
}

// finishTool is base.finish_tool (non-string output as json.dumps, ensure_ascii).
func (s *scope) finishTool(call M, at any, isError bool, output any) {
	if call == nil {
		return
	}
	call["end"] = at
	call["is_error"] = isError
	if util.Truthy(output) {
		str, ok := output.(string)
		if !ok {
			str = s.dumps(output, true)
		}
		call["output"] = s.excerpt(str, 1500)
	}
}

// oneLine is claude_code._one_line.
func oneLine(text string, limit int) string {
	t := strings.Join(pySplit(text), " ")
	if util.RuneLen(t) <= limit {
		return t
	}
	return pyRStrip(util.Clip(t, limit-3)) + "..."
}

// ---------------------------------------------------------------- numbers

// isNum is claude_code._number: int or float, not bool.
func isNum(v any) bool {
	switch v.(type) {
	case int64, int, int32, float64, float32:
		return true
	}
	return false
}

func isInt(v any) bool {
	switch v.(type) {
	case int64, int, int32:
		return true
	}
	return false
}

func toInt64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case int32:
		return int64(t)
	}
	return 0
}

func toFloat(v any) float64 {
	f, _ := util.Num(v)
	return f
}

// pySum is Python 3.12+ sum(values) over numbers: exact int arithmetic while
// every value is an int, then Neumaier-compensated float summation.
func pySum(values []any) any {
	var i int64
	idx := 0
	for ; idx < len(values); idx++ {
		if !isInt(values[idx]) {
			break
		}
		i += toInt64(values[idx])
	}
	if idx == len(values) {
		return i
	}
	f := float64(i) + toFloat(values[idx])
	c := 0.0
	for _, v := range values[idx+1:] {
		x := toFloat(v)
		t := f + x
		if math.Abs(f) >= math.Abs(x) {
			c += (f - t) + x
		} else {
			c += (x - t) + f
		}
		f = t
	}
	if c != 0 && !math.IsInf(c, 0) && !math.IsNaN(c) {
		f += c
	}
	return f
}

// pyAdd is Python a + b for two numbers.
func pyAdd(a, b any) any {
	if isInt(a) && isInt(b) {
		return toInt64(a) + toInt64(b)
	}
	return toFloat(a) + toFloat(b)
}

// pyLess is a < b for two numbers.
func pyLess(a, b any) bool {
	if isInt(a) && isInt(b) {
		return toInt64(a) < toInt64(b)
	}
	return toFloat(a) < toFloat(b)
}

// pyIntOf is Python int(v or 0) for token counts.
func pyIntOf(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case int64, int, int32:
		return toInt64(t)
	case float64:
		return int64(t)
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// pyFloatOf is Python float(v or 0).
func pyFloatOf(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		if t == "" {
			return 0
		}
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	}
	return toFloat(v)
}

// ---------------------------------------------------------------- misc

// uuid4 is str(uuid.uuid4()).
func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func anyList(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

func sortedStrings(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validUTF8 replaces invalid bytes (Python errors="replace").
func validUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}
