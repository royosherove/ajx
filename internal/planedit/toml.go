package planedit

import (
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/royosherove/ajx/internal/render/pyfmt"
)

// ---------------------------------------------------------------- ordered TOML

// ParseTOML decodes TOML text the way tomllib.loads does: tables are Dicts
// with their keys in document order, arrays []any, integers int64, floats
// float64, and dates and times time.Time. The order comes from the decoder's
// key list (every key in document order).
func ParseTOML(text string) (*Dict, error) {
	raw := map[string]any{}
	md, err := toml.Decode(text, &raw)
	if err != nil {
		return nil, err
	}
	o := &orderer{order: map[uintptr][]string{}, noted: map[uintptr]map[string]bool{},
		count: map[arrayKey]int{}, pick: map[arrayKey]*picker{}}
	for _, key := range md.Keys() {
		o.visit(raw, key, md.Type(key...))
	}
	return o.dict(raw), nil
}

func validTOML(text string) bool {
	var raw map[string]any
	_, err := toml.Decode(text, &raw)
	return err == nil
}

type arrayKey struct {
	parent uintptr
	name   string
}

type orderer struct {
	order map[uintptr][]string
	noted map[uintptr]map[string]bool
	count map[arrayKey]int     // [[x]] headers seen so far, per array of tables
	pick  map[arrayKey]*picker // the element an inline array of tables is filling
}

// picker follows an inline array of tables: each element's keys come in one
// run, so a key the current element lacks (or already had) starts the next.
type picker struct {
	idx  int
	used map[int]map[string]bool
}

func (p *picker) element(tables []map[string]any, key string, direct bool) map[string]any {
	if !direct {
		if p.idx < len(tables) {
			return tables[p.idx]
		}
		return nil
	}
	for p.idx < len(tables) {
		e := tables[p.idx]
		if _, ok := e[key]; ok && !p.used[p.idx][key] {
			if p.used == nil {
				p.used = map[int]map[string]bool{}
			}
			if p.used[p.idx] == nil {
				p.used[p.idx] = map[string]bool{}
			}
			p.used[p.idx][key] = true
			return e
		}
		p.idx++
	}
	return nil
}

func addr(m map[string]any) uintptr { return reflect.ValueOf(m).Pointer() }

func (o *orderer) note(m map[string]any, k string) {
	p := addr(m)
	if o.noted[p] == nil {
		o.noted[p] = map[string]bool{}
	}
	if !o.noted[p][k] {
		o.noted[p][k] = true
		o.order[p] = append(o.order[p], k)
	}
}

func tablesOf(v any) []map[string]any {
	switch t := v.(type) {
	case []map[string]any:
		return t
	case []any:
		if len(t) == 0 {
			return nil
		}
		out := make([]map[string]any, len(t))
		for i, x := range t {
			m, ok := x.(map[string]any)
			if !ok {
				return nil
			}
			out[i] = m
		}
		return out
	}
	return nil
}

func (o *orderer) visit(root map[string]any, key toml.Key, typ string) {
	cur := root
	for i, seg := range key {
		o.note(cur, seg)
		v := cur[seg]
		tables := tablesOf(v)
		ak := arrayKey{addr(cur), seg}
		if i == len(key)-1 {
			if tables != nil {
				if typ == "ArrayHash" {
					o.count[ak]++
				} else {
					o.pick[ak] = &picker{}
				}
			}
			return
		}
		if tables != nil {
			var next map[string]any
			if n := o.count[ak]; n > 0 {
				if n <= len(tables) {
					next = tables[n-1]
				}
			} else {
				if o.pick[ak] == nil {
					o.pick[ak] = &picker{}
				}
				next = o.pick[ak].element(tables, key[i+1], i+2 == len(key))
			}
			if next == nil {
				return
			}
			cur = next
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		cur = m
	}
}

func (o *orderer) value(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return o.dict(t)
	case []map[string]any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = o.dict(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = o.value(x)
		}
		return out
	case int:
		return int64(t)
	}
	return v
}

func (o *orderer) dict(m map[string]any) *Dict {
	d := &Dict{vals: make(map[string]any, len(m))}
	for _, k := range o.order[addr(m)] {
		if v, ok := m[k]; ok {
			d.Set(k, o.value(v))
		}
	}
	if d.Len() < len(m) {
		var rest []string
		for k := range m {
			if !d.Has(k) {
				rest = append(rest, k)
			}
		}
		sort.Strings(rest)
		for _, k := range rest {
			d.Set(k, o.value(m[k]))
		}
	}
	return d
}

// ---------------------------------------------------------------- TOML text editing

var (
	headerRe = regexp.MustCompile(`^[` + pyfmt.WS + `]*(\[\[?)([^\[\]]+?)(\]\]?)[` + pyfmt.WS + `]*(#.*)?$`)
	bareRe   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// stringEnd is the index just past the multi-line string delimiter closing at or after i, or -1.
func stringEnd(line string, i int, delim string) int {
	for {
		if i > len(line) {
			return -1
		}
		j := strings.Index(line[i:], delim)
		if j < 0 {
			return -1
		}
		j += i
		if delim == `"""` {
			k := j - 1
			for k >= 0 && line[k] == '\\' {
				k--
			}
			if (j-1-k)%2 == 1 { // an escaped quote does not close the string
				i = j + 1
				continue
			}
		}
		end := j + 3
		for end < len(line) && line[end] == delim[0] && end-j < 5 { // up to two quotes may precede it
			end++
		}
		return end
	}
}

// StringLines reports, for each line, whether it begins inside a multi-line
// string (it is then neither a key nor a header).
func StringLines(lines []string) []bool {
	inside := make([]bool, len(lines))
	state := ""
	for n, line := range lines {
		inside[n] = state != ""
		i := 0
		for i < len(line) {
			if state != "" {
				end := stringEnd(line, i, state)
				if end < 0 {
					break
				}
				state, i = "", end
				continue
			}
			ch := line[i]
			if ch == '#' {
				break
			}
			if strings.HasPrefix(line[i:], `"""`) || strings.HasPrefix(line[i:], "'''") {
				state, i = line[i:i+3], i+3
				continue
			}
			if ch == '"' {
				j := i + 1
				for j < len(line) && line[j] != '"' {
					if line[j] == '\\' {
						j += 2
					} else {
						j++
					}
				}
				i = j + 1
				continue
			}
			if ch == '\'' {
				j := strings.IndexByte(line[i+1:], '\'')
				if j < 0 {
					i = len(line)
				} else {
					i = i + 1 + j + 1
				}
				continue
			}
			i++
		}
	}
	return inside
}

// Section is one table of a trial file: its header line and the line after
// its last one. The root section (before any header) has Root set.
type Section struct {
	Name   string
	Root   bool
	Array  bool
	Header int
	End    int
}

// Sections lists the file's sections. Quoted or dotted headers end a section
// but never match a plain table name; lines inside strings are never headers.
func Sections(lines []string) []Section {
	var out []Section
	cur := Section{Root: true, Header: -1}
	for i, quoted := range StringLines(lines) {
		if quoted {
			continue
		}
		m := headerRe.FindStringSubmatch(lines[i])
		if m != nil && (m[1] == "[[") == (m[3] == "]]") {
			cur.End = i
			out = append(out, cur)
			cur = Section{Name: pyfmt.Strip(m[2]), Array: m[1] == "[[", Header: i}
		}
	}
	cur.End = len(lines)
	return append(out, cur)
}

// Key is a TOML key: bare when it can be, else quoted.
func Key(name string) string {
	if bareRe.MatchString(name) {
		return name
	}
	return pyfmt.Dumps(name, pyfmt.DumpOpts{ASCII: true})
}

// Value is the TOML for a value: scalars, arrays and inline tables.
func Value(v any) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return pyfmt.FloatRepr(t)
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			parts[i] = Value(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *Dict:
		if t.Len() == 0 {
			return "{}"
		}
		parts := make([]string, 0, t.Len())
		for _, k := range t.keys {
			parts = append(parts, Key(k)+" = "+Value(t.vals[k]))
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	text := pyStr(v)
	control := func(allowed string) bool {
		for i := 0; i < len(text); i++ {
			if text[i] < 32 && strings.IndexByte(allowed, text[i]) < 0 {
				return true
			}
		}
		return false
	}
	if strings.Contains(text, "\n") && !strings.Contains(text, "'''") && !strings.HasSuffix(text, "'") &&
		!strings.Contains(text, "\r") && !control("\n\t") {
		return "'''\n" + text + "'''" // literal multi-line string: commands stay readable and exact
	}
	if strings.Contains(text, `"`) && !strings.Contains(text, "'") && !control("") {
		return "'" + text + "'"
	}
	return pyfmt.Dumps(text, pyfmt.DumpOpts{})
}

func trailingComment(line, name string) string {
	for i := 0; i < len(line); i++ {
		if line[i] != '#' {
			continue
		}
		if parsed, err := ParseTOML(line[:i]); err == nil && parsed.Has(name) {
			return "  " + pyfmt.Strip(line[i:])
		}
	}
	return ""
}

// valueEnd is the index after the last line of the key/value that starts at
// start (arrays and strings can span lines).
func valueEnd(lines []string, start, end int) int {
	for j := start + 1; j <= end; j++ {
		if validTOML(strings.Join(lines[start:j], "\n")) {
			return j
		}
	}
	return start + 1
}

func isBlank(s string) bool { return pyfmt.Strip(s) == "" }

func isComment(s string) bool { return strings.HasPrefix(pyfmt.LStrip(s), "#") }

// SetValue sets (or with nil, removes) name in [table], or in the index-th
// [[cells]], keeping the rest of the file as written. The result is parsed
// and compared before it is returned; problems are *Error.
func SetValue(text, table, name string, value any, index int) (string, error) {
	var out string
	err := catch(func() { out = setValue(text, table, name, value, index) })
	if pe, ok := err.(*pyError); ok {
		return "", &Error{Msg: pe.msg}
	}
	return out, err
}

func setValue(text, table, name string, value any, index int) string {
	lines := strings.Split(text, "\n")
	quoted := StringLines(lines)
	var found []Section
	for _, s := range Sections(lines) {
		if !s.Root && s.Name == table && s.Array == (table == "cells") {
			found = append(found, s)
		}
	}
	rendered, remove := "", value == nil
	if !remove {
		rendered = Key(name) + " = " + Value(value)
	}
	if table == "cells" && index >= len(found) {
		fail("cells[%d] not found", index)
	}
	if len(found) > 0 {
		s := found[0]
		if table == "cells" {
			s = found[index]
		}
		start, end := s.Header+1, s.End
		live := regexp.MustCompile(`^[` + pyfmt.WS + `]*` + regexp.QuoteMeta(Key(name)) + `[` + pyfmt.WS + `]*=`)
		commented := regexp.MustCompile(`^[` + pyfmt.WS + `]*#[` + pyfmt.WS + `]*` + regexp.QuoteMeta(name) + `[` + pyfmt.WS + `]*=`)
		hit := -1
		for i := start; i < end; i++ {
			if !quoted[i] && live.MatchString(lines[i]) {
				hit = i
				break
			}
		}
		switch {
		case hit >= 0:
			stop := valueEnd(lines, hit, end)
			comment := ""
			if stop == hit+1 {
				comment = trailingComment(lines[hit], name)
			}
			repl := []string{}
			if !remove {
				repl = []string{rendered + comment}
			}
			lines = append(lines[:hit:hit], append(repl, lines[stop:]...)...)
		case !remove:
			hint := -1
			for i := start; i < end; i++ {
				if !quoted[i] && commented.MatchString(lines[i]) {
					hint = i
					break
				}
			}
			if hint >= 0 {
				lines[hint] = rendered // replace the scaffold's commented-out example in place
			} else {
				at := end
				for at > start && (isBlank(lines[at-1]) || isComment(lines[at-1])) && !quoted[at-1] {
					at--
				}
				lines = append(lines[:at:at], append([]string{rendered}, lines[at:]...)...)
			}
		}
	} else if !remove {
		for len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "", "["+table+"]", rendered, "")
	}
	result := strings.Join(lines, "\n")
	parsed, err := ParseTOML(result)
	if err != nil {
		fail("could not update %s.%s safely (%s); edit trial.toml directly", table, name, err.Error())
	}
	var holder any
	if table == "cells" {
		cells := orList(parsed.Get("cells"))
		if len(cells) == 0 {
			cells = []any{NewDict()}
		}
		if index >= len(cells) {
			raise("IndexError", "list index out of range")
		}
		holder = cells[index]
	} else {
		holder = orDict(parsed.Get(table))
	}
	if (remove && has(holder, name)) || (!remove && !Equal(get(holder, name), value)) {
		fail("could not update %s.%s safely; edit trial.toml directly", table, name)
	}
	return result
}

// block is a table's lines: its header, its keys (None values left out) and a blank line.
func block(header string, mapping *Dict, array bool) []string {
	lines := []string{"[" + header + "]"}
	if array {
		lines[0] = "[[" + header + "]]"
	}
	for _, k := range mapping.Keys() {
		if v := mapping.Get(k); v != nil {
			lines = append(lines, Key(k)+" = "+Value(v))
		}
	}
	return append(lines, "")
}

// ReplaceSections replaces every section for which belongs(name, isArray)
// holds with newLines, placed where the first of them was (or at the end).
// Comment lines that end a removed section stay: they usually introduce the
// section that follows.
func ReplaceSections(text string, belongs func(name string, array bool) bool, newLines []string) string {
	lines := strings.Split(text, "\n")
	quoted := StringLines(lines)
	var doomed []Section
	for _, s := range Sections(lines) {
		if !s.Root && belongs(s.Name, s.Array) {
			doomed = append(doomed, s)
		}
	}
	if len(doomed) == 0 && len(newLines) == 0 {
		return text
	}
	drop := map[int]bool{}
	for _, s := range doomed {
		last := s.Header
		for i := s.Header + 1; i < s.End; i++ {
			if quoted[i] || (!isBlank(lines[i]) && !isComment(lines[i])) {
				last = i
			}
		}
		for i := s.Header; i <= last; i++ {
			drop[i] = true
		}
		if last+1 < s.End && isBlank(lines[last+1]) {
			drop[last+1] = true
		}
	}
	var out []string
	placed := false
	for i, line := range lines {
		if len(doomed) > 0 && i == doomed[0].Header && !placed {
			out = append(out, newLines...)
			placed = true
		}
		if !drop[i] {
			out = append(out, line)
		}
	}
	if !placed && len(newLines) > 0 {
		for len(out) > 0 && out[len(out)-1] == "" {
			out = out[:len(out)-1]
		}
		out = append(append(out, ""), newLines...)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

// group matches the sections of the array of tables name, with their
// subtables such as [verify.headers].
func group(name string) func(string, bool) bool {
	return func(n string, array bool) bool { return (array && n == name) || strings.HasPrefix(n, name+".") }
}
