// Package export ports lib/ajx/export.py: one run as a folder a product team
// can open: report.html, journey.html, asks.md.
//
// The copies differ from the run directory in two ways.
//
// Paths. The worker's directories recorded in state.json "paths" become
// neutral labels (workspace -> /workspace, home_dir -> /agent-home,
// config_dir -> /agent-home/.config/harness, cache_dir -> /agent-home/.cache),
// including the '-'-encoded spellings some harnesses use for project
// directories and the archived copies under the run directory. Other attempt
// directories under the same workspace root (<root>/<hex>) become
// /agent-home/attempt, the bare root becomes /agent-home/attempts, and the
// coordinator's home directory becomes ~. Every rule is applied in one pass,
// longest match first, so a directory nested in another gets its own label
// and inserted labels are never rewritten.
//
// Links. Links to files the bundle does not carry (run.json, verify.json,
// environment.json, agent-configuration.json, ../../index.html) become their
// text; list items that only carry such a link are dropped; links to
// pretty-journey.html follow its rename to journey.html; the brand link loses
// its href.
//
// Text-based and standard-library only. The result is a starting point for
// review, not a redaction: the agent's commands and outputs, account
// identifiers and URLs stay as the reports recorded them.
//
// Python's patterns use lookahead and tempered tokens, which Go's regexp
// lacks; the matchers here are written by hand to the same semantics.
package export

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/util"
)

type M = util.M

// Files are (source, bundle name) pairs.
var Files = [][2]string{{"report.html", "report.html"}, {"pretty-journey.html", "journey.html"}, {"asks.md", "asks.md"}}

// LabelKeys and Labels map state.json path keys to their neutral labels.
var (
	LabelKeys = []string{"workspace", "home_dir", "config_dir", "cache_dir"}
	Labels    = map[string]string{"workspace": "/workspace", "home_dir": "/agent-home",
		"config_dir": "/agent-home/.config/harness", "cache_dir": "/agent-home/.cache"}
)

const (
	AttemptLabel = "/agent-home/attempt"
	RootLabel    = "/agent-home/attempts" // the workspace root itself, when it appears without an attempt below it
	HomeLabel    = "~"
)

// Outside are files the bundle does not carry: links to them become their text.
var Outside = []string{"run.json", "verify.json", "environment.json", "agent-configuration.json", "../../index.html"}

// Error is a run that cannot be exported; the message says why and names the path.
type Error struct{ Msg string }

func (e *Error) Error() string    { return e.Msg }
func (e *Error) TypeName() string { return "ExportError" }

// ---------------------------------------------------------------- path rules

type endKind int

const (
	rawEnd     endKind = iota // (?![\w-]|\.\w): may not continue into a longer name
	encodedEnd                // (?!\w|\.\w): encoded spellings continue with '-'
)

// Rule is one replacement: a literal prefix, optionally followed by a
// [0-9a-f]{6,} attempt name, then a boundary.
type Rule struct {
	Literal     string
	Hex         bool
	End         endKind
	Replacement string
	Key         string
}

// EncodedForms are how a harness may spell a directory as a project-directory
// name: '/' -> '-' (and, for Claude Code, every other non-alphanumeric
// character too). Distinct forms only.
func EncodedForms(path string) []string {
	a := strings.ReplaceAll(path, "/", "-")
	var b strings.Builder
	for _, r := range path {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.String() == a {
		return []string{a}
	}
	return []string{a, b.String()}
}

// pairedForms pairs each encoded spelling of path with the matching spelling of label.
func pairedForms(path, label string) [][2]string {
	labels := EncodedForms(label)
	var out [][2]string
	for i, form := range EncodedForms(path) {
		j := i
		if j > len(labels)-1 {
			j = len(labels) - 1
		}
		out = append(out, [2]string{form, labels[j]})
	}
	return out
}

type literal struct{ text, replacement, key string }

// ReplacementRules are the rules in priority order for a single-pass rewrite:
// literal directories longest first, then sibling attempt directories under
// the workspace root, then anything shorter than that root (typically the
// coordinator's home).
func ReplacementRules(paths M, home string, archived M) []Rule {
	var literals []literal
	for _, source := range []M{paths, archived} {
		for _, key := range LabelKeys {
			label := Labels[key]
			value, ok := source[key].(string)
			if !ok || strings.Trim(value, "/") == "" {
				continue
			}
			literals = append(literals, literal{value, label, label})
			for _, f := range pairedForms(value, label) {
				literals = append(literals, literal{f[0], f[1], label})
			}
		}
	}
	root := ""
	if ws, ok := paths["workspace"].(string); ok && strings.Trim(ws, "/") != "" {
		root = pyfmt.Parent(ws)
	}
	var attempts []Rule
	if strings.Trim(root, "/") != "" {
		attempts = append(attempts, Rule{root + "/", true, rawEnd, AttemptLabel, AttemptLabel})
		for _, f := range pairedForms(root, AttemptLabel) {
			attempts = append(attempts, Rule{f[0] + "-", true, encodedEnd, f[1], AttemptLabel})
		}
		attempts = append(attempts, Rule{root, false, rawEnd, RootLabel, RootLabel})
		for _, f := range pairedForms(root, RootLabel) {
			attempts = append(attempts, Rule{f[0], false, encodedEnd, f[1], RootLabel})
		}
	}
	if home != "" {
		h := pyfmt.CleanPath(pyfmt.ExpandUser(home))
		resolved := pyfmt.Resolve(h)
		var homes []string
		for _, x := range []string{h, resolved} {
			if strings.Trim(x, "/") != "" && !util.Contains(homes, x) {
				homes = append(homes, x)
			}
		}
		for _, x := range homes {
			literals = append(literals, literal{x, HomeLabel, HomeLabel})
			for _, f := range EncodedForms(x) {
				literals = append(literals, literal{f, HomeLabel, HomeLabel})
			}
		}
	}
	seen := map[literal]bool{}
	var unique []literal
	for _, l := range literals {
		if !seen[l] {
			seen[l] = true
			unique = append(unique, l)
		}
	}
	sort.SliceStable(unique, func(i, j int) bool {
		return utf8.RuneCountInString(unique[i].text) > utf8.RuneCountInString(unique[j].text)
	})
	rule := func(l literal) Rule {
		end := encodedEnd
		if strings.HasPrefix(l.text, "/") {
			end = rawEnd
		}
		return Rule{l.text, false, end, l.replacement, l.key}
	}
	rootLen := utf8.RuneCountInString(root)
	var longer, shorter []Rule
	for _, l := range unique {
		if utf8.RuneCountInString(l.text) > rootLen {
			longer = append(longer, rule(l))
		} else {
			shorter = append(shorter, rule(l))
		}
	}
	return append(append(longer, attempts...), shorter...)
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }

// endOK checks the boundary lookahead at byte offset i.
func endOK(text string, i int, kind endKind) bool {
	r, size := pyfmt.RuneAt(text, i)
	if size == 0 {
		return true
	}
	if pyfmt.IsWord(r) || (kind == rawEnd && r == '-') {
		return false
	}
	if r == '.' {
		if next, n := pyfmt.RuneAt(text, i+size); n > 0 && pyfmt.IsWord(next) {
			return false
		}
	}
	return true
}

// match returns the end of a match of r at offset i, or -1.
func (r Rule) match(text string, i int) int {
	if !strings.HasPrefix(text[i:], r.Literal) {
		return -1
	}
	j := i + len(r.Literal)
	if r.Hex {
		k := j
		for k < len(text) && isHex(text[k]) {
			k++
		}
		// A shorter hex run is followed by a hex digit (a word character), so
		// only the full run can end at a boundary.
		if k-j < 6 {
			return -1
		}
		j = k
	}
	if !endOK(text, j, r.End) {
		return -1
	}
	return j
}

// RewritePaths applies every rule in one pass (earlier rules win at the same
// position) and counts hits per key in counts (nil = not counted).
func RewritePaths(text string, rules []Rule, counts M) string {
	if len(rules) == 0 {
		return text
	}
	if counts != nil {
		for _, r := range rules {
			if _, ok := counts[r.Key]; !ok {
				counts[r.Key] = int64(0)
			}
		}
	}
	var b strings.Builder
	last := 0
	for i := 0; i < len(text); {
		matched := false
		for _, r := range rules {
			if end := r.match(text, i); end >= 0 {
				b.WriteString(text[last:i])
				b.WriteString(r.Replacement)
				if counts != nil {
					counts[r.Key] = counts[r.Key].(int64) + 1
				}
				last, i, matched = end, end, true
				break
			}
		}
		if !matched {
			_, size := utf8.DecodeRuneInString(text[i:])
			i += size
		}
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// keyOrder lists the rules' keys in first-use order (Python's counts dict order).
func keyOrder(rules []Rule) []string {
	var out []string
	for _, r := range rules {
		if !util.Contains(out, r.Key) {
			out = append(out, r.Key)
		}
	}
	return out
}

// ---------------------------------------------------------------- links

var (
	brandRe       = regexp.MustCompile(`<a class="brand" href="[^"]*">`)
	journeyHref   = regexp.MustCompile(`href="pretty-journey\.html`)
	mdOutside     = `(?:` + quoteAll(Outside) + `)(?:#[^)]*)?`
	mdLiOutside   = regexp.MustCompile(`(?m)^[ \t]*[-*+][ \t]+\[[^\]]*\]\(` + mdOutside + `\)[ \t]*\n?`)
	mdLinkOutside = regexp.MustCompile(`\[([^\]]*)\]\(` + mdOutside + `\)`)
	mdJourney     = regexp.MustCompile(`\]\(pretty-journey\.html`)
)

func quoteAll(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = regexp.QuoteMeta(n)
	}
	return strings.Join(q, "|")
}

// wordBoundaryAfter is \b after a word character ending at byte offset i.
func wordBoundaryAfter(text string, i int) bool {
	r, size := pyfmt.RuneAt(text, i)
	return size == 0 || !pyfmt.IsWord(r)
}

// outsideHref matches `href="<outside>(#[^"]*)?"` at offset i (the caller
// checked the \b before "href"); it returns the offset after the closing quote.
func outsideHref(text string, i int) int {
	if !strings.HasPrefix(text[i:], `href="`) {
		return -1
	}
	j := i + len(`href="`)
	for _, name := range Outside {
		if !strings.HasPrefix(text[j:], name) {
			continue
		}
		k := j + len(name)
		if k < len(text) && text[k] == '#' {
			q := strings.IndexByte(text[k:], '"')
			if q < 0 {
				continue
			}
			return k + q + 1
		}
		if k < len(text) && text[k] == '"' {
			return k + 1
		}
	}
	return -1
}

// anchorHead matches `<a\b[^>]*\bhref="<outside>(#[^"]*)?"` at offset i and
// returns the possible end offsets in backtracking preference order (the
// greedy [^>]* tries the last href first).
func anchorHead(text string, i int) []int {
	if !strings.HasPrefix(text[i:], "<a") || !wordBoundaryAfter(text, i+2) {
		return nil
	}
	start := i + 2
	stop := strings.IndexByte(text[start:], '>')
	limit := len(text)
	if stop >= 0 {
		limit = start + stop
	}
	var ends []int
	for p := limit; p >= start; p-- { // href may start anywhere [^>]* reaches
		if p+1 > len(text) || !strings.HasPrefix(text[p:], "href=") {
			continue
		}
		if r, size := pyfmt.RuneBefore(text, p); size > 0 && pyfmt.IsWord(r) {
			continue
		}
		if end := outsideHref(text, p); end >= 0 {
			ends = append(ends, end)
		}
	}
	return ends
}

// forbiddenAt reports whether "</li>" or "<li\b" starts at offset i.
func forbiddenAt(text string, i int) bool {
	if strings.HasPrefix(text[i:], "</li>") {
		return true
	}
	return strings.HasPrefix(text[i:], "<li") && wordBoundaryAfter(text, i+3)
}

// nextForbidden is the first offset >= i where the tempered token stops.
func nextForbidden(text string, i int) int {
	for j := i; j < len(text); j++ {
		if text[j] == '<' && forbiddenAt(text, j) {
			return j
		}
	}
	return len(text)
}

// liOutside matches Python's _LI_OUTSIDE at offset i:
// <li\b[^>]*>(?:(?!</li>|<li\b).)*?<a\b[^>]*\bhref="OUTSIDE"(?:(?!</li>|<li\b).)*?</li>
func liOutside(text string, i int) int {
	if !strings.HasPrefix(text[i:], "<li") || !wordBoundaryAfter(text, i+3) {
		return -1
	}
	gt := strings.IndexByte(text[i+3:], '>')
	if gt < 0 {
		return -1
	}
	s := i + 3 + gt + 1
	e := nextForbidden(text, s)
	for q := s; q < e; q++ {
		if text[q] != '<' {
			continue
		}
		for _, r := range anchorHead(text, q) {
			e2 := nextForbidden(text, r)
			if strings.HasPrefix(text[e2:], "</li>") {
				return e2 + len("</li>")
			}
		}
	}
	return -1
}

// aOutside matches Python's _A_OUTSIDE at offset i:
// <a\b[^>]*\bhref="OUTSIDE"[^>]*>(.*?)</a>  (re.S); it returns the match end
// and the link text.
func aOutside(text string, i int) (int, string) {
	for _, r := range anchorHead(text, i) {
		gt := strings.IndexByte(text[r:], '>')
		if gt < 0 {
			continue
		}
		bodyStart := r + gt + 1
		end := strings.Index(text[bodyStart:], "</a>")
		if end < 0 {
			continue
		}
		return bodyStart + end + len("</a>"), text[bodyStart : bodyStart+end]
	}
	return -1, ""
}

// RewriteHTMLLinks: brand link without href; list items and links that lead
// outside the bundle reduced to text; pretty-journey.html links renamed.
func RewriteHTMLLinks(text string) string {
	text = brandRe.ReplaceAllString(text, `<a class="brand">`)
	text = scan(text, func(t string, i int) (int, string) {
		if end := liOutside(t, i); end >= 0 {
			return end, ""
		}
		return -1, ""
	})
	text = scan(text, aOutside)
	return journeyHref.ReplaceAllString(text, `href="journey.html`)
}

// scan replaces non-overlapping matches found left to right by m.
func scan(text string, m func(string, int) (int, string)) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(text); {
		if text[i] == '<' {
			if end, repl := m(text, i); end >= 0 {
				b.WriteString(text[last:i])
				b.WriteString(repl)
				last, i = end, end
				continue
			}
		}
		i++
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// RewriteMarkdownLinks is the Markdown counterpart: bullet lines that only
// carry an outside link are dropped, other outside links become their text,
// pretty-journey.html links renamed.
func RewriteMarkdownLinks(text string) string {
	text = mdLiOutside.ReplaceAllString(text, "")
	text = mdLinkOutside.ReplaceAllString(text, "$1")
	return mdJourney.ReplaceAllString(text, "](journey.html")
}

// ---------------------------------------------------------------- export

// ExportOpts are ExportRun's optional arguments.
type ExportOpts struct {
	Home string // coordinator home to relabel as ~ ("" = the caller's home)
}

// ExportRun writes report.html, journey.html and asks.md for one run into
// dest (created with parents). It returns M{files, replacements, dest,
// warnings} plus replacement_order, the labels in Python's dict order for
// printing. An *Error reports a missing run directory or source file;
// nothing is written in that case.
func ExportRun(runDir, dest string, o ExportOpts) (M, error) {
	if st, err := os.Stat(runDir); err != nil || !st.IsDir() {
		return nil, &Error{fmt.Sprintf("run directory not found: %s", pyfmt.CleanPath(runDir))}
	}
	var missing []string
	for _, f := range Files {
		if st, err := os.Stat(filepath.Join(runDir, f[0])); err != nil || !st.Mode().IsRegular() {
			missing = append(missing, f[0])
		}
	}
	if len(missing) > 0 {
		return nil, &Error{fmt.Sprintf("%s: cannot export without %s in %s (render the run first: ajx run <trial> --stages render)",
			pyfmt.Name(runDir), strings.Join(missing, ", "), pyfmt.CleanPath(runDir))}
	}
	warnings := []any{}
	var paths M
	stateV, err := pyfmt.ReadJSON(filepath.Join(runDir, "state.json"), nil)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return nil, err // OSError propagates; only a decode error (ValueError) is a warning
		}
		stateV = nil
		warnings = append(warnings, "state.json is unreadable; the worker's directories were not relabeled")
	}
	state, ok := stateV.(M)
	switch {
	case !ok:
		if len(warnings) == 0 {
			warnings = append(warnings, "state.json is missing; the worker's directories were not relabeled")
		}
		state = M{}
	case util.Truthy(state["paths"]) && util.Map(state["paths"]) != nil:
		paths = state["paths"].(M)
	default:
		warnings = append(warnings, "state.json records no worker directories (paths); only the home directory was relabeled")
	}
	home := o.Home
	if home == "" {
		home = util.Home()
	}
	rules := ReplacementRules(paths, home, util.Map(state["archived"]))
	counts := M{}
	type output struct{ name, text string }
	var outputs []output
	for _, f := range Files {
		data, err := os.ReadFile(filepath.Join(runDir, f[0]))
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(data) {
			return nil, &Error{fmt.Sprintf("%s is not UTF-8 text: %s", pyfmt.CleanPath(runDir)+"/"+f[0], decodeError(data))}
		}
		text := RewritePaths(string(data), rules, counts)
		if strings.HasSuffix(f[1], ".md") {
			text = RewriteMarkdownLinks(text)
		} else {
			text = RewriteHTMLLinks(text)
		}
		outputs = append(outputs, output{f[1], text})
	}
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return nil, err
	}
	files := []any{}
	for _, out := range outputs {
		target := filepath.Join(dest, out.name)
		if err := os.WriteFile(target, []byte(out.text), 0o644); err != nil {
			return nil, err
		}
		if err := os.Chmod(target, 0o644); err != nil {
			return nil, err
		}
		files = append(files, out.name)
	}
	order := []any{}
	for _, k := range keyOrder(rules) {
		order = append(order, k)
	}
	return M{"files": files, "replacements": counts, "replacement_order": order,
		"dest": pyfmt.CleanPath(dest), "warnings": warnings}, nil
}

// decodeError is the message of Python's UnicodeDecodeError for data.
func decodeError(data []byte) string {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		b := data[i]
		need := 0
		switch {
		case b >= 0xc2 && b <= 0xdf:
			need = 1
		case b >= 0xe0 && b <= 0xef:
			need = 2
		case b >= 0xf0 && b <= 0xf4:
			need = 3
		default:
			return fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: invalid start byte", b, i)
		}
		if i+need >= len(data) {
			valid := true
			for _, c := range data[i+1:] {
				if c < 0x80 || c > 0xbf {
					valid = false
				}
			}
			if valid {
				if len(data)-1 > i {
					return fmt.Sprintf("'utf-8' codec can't decode bytes in position %d-%d: unexpected end of data", i, len(data)-1)
				}
				return fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: unexpected end of data", b, i)
			}
		}
		return fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: invalid continuation byte", b, i)
	}
	return "invalid UTF-8"
}
