package cli

// A small command-line parser that behaves like Python's argparse as bin/ajx
// uses it: options before or after positionals, --flag=value, unique-prefix
// abbreviations, "--", subcommands, mutually exclusive groups, and the same
// usage text, error messages and exit code 2 for usage errors.

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Kind is how an argument stores its value.
type Kind int

const (
	Store       Kind = iota // one string value
	StoreInt                // one int value
	StoreTrue               // flag
	HelpFlag                // -h/--help
	VersionFlag             // --version
	Append                  // repeatable string option
	parsers                 // subcommand choice
)

// Arg is one argument (option or positional).
type Arg struct {
	Flags    []string // option strings; empty for a positional
	Dest     string
	Kind     Kind
	Required bool
	Optional bool // positional nargs='?'
	Metavar  string
	Help     string
	Default  any
	Hidden   bool // left out of help and usage (argparse.SUPPRESS)
	// Deferred is required in usage but checked by the command, not the
	// parser (init asks for it in a terminal); see RequireDeferred.
	Deferred bool
	group    *Group
	sub      *Subparsers
}

// Group is a mutually exclusive group.
type Group struct {
	Required bool
	args     []*Arg
}

// Parser is one (sub)command.
type Parser struct {
	Prog        string
	Description string
	RawDesc     bool // RawDescriptionHelpFormatter
	Version     string
	Help        string // help shown in the parent's command list ("" = not listed)
	Run         func(*Namespace) int
	args        []*Arg
	groups      []*Group
	parent      *Parser
}

// Subparsers is the subcommand slot of a parser.
type Subparsers struct {
	Dest     string
	Required bool
	names    []string
	parsers  map[string]*Parser
	owner    *Parser
}

// Namespace holds parsed values.
type Namespace struct {
	Values map[string]any
	Leaf   *Parser
}

// Str is a string value ("" when unset).
func (n *Namespace) Str(dest string) string {
	if s, ok := n.Values[dest].(string); ok {
		return s
	}
	return ""
}

// Has reports whether dest has a non-nil value.
func (n *Namespace) Has(dest string) bool { return n.Values[dest] != nil }

// Bool is a flag value.
func (n *Namespace) Bool(dest string) bool {
	b, _ := n.Values[dest].(bool)
	return b
}

// Int is an int value (def when unset).
func (n *Namespace) Int(dest string, def int) int {
	if i, ok := n.Values[dest].(int); ok {
		return i
	}
	return def
}

// List is a repeatable option's values.
func (n *Namespace) List(dest string) []string {
	l, _ := n.Values[dest].([]string)
	return l
}

// IntPtr is an int value or nil when unset.
func (n *Namespace) IntPtr(dest string) *int {
	if i, ok := n.Values[dest].(int); ok {
		return &i
	}
	return nil
}

// NewParser creates a top-level parser.
func NewParser(prog, description string) *Parser {
	p := &Parser{Prog: prog, Description: description}
	p.args = append(p.args, &Arg{Flags: []string{"-h", "--help"}, Dest: "help", Kind: HelpFlag,
		Help: "show this help message and exit"})
	return p
}

// Add appends an argument and returns it.
func (p *Parser) Add(a *Arg) *Arg {
	if a.Dest == "" {
		if len(a.Flags) == 0 {
			panic("positional without dest")
		}
		long := a.Flags[0]
		for _, f := range a.Flags {
			if strings.HasPrefix(f, "--") {
				long = f
				break
			}
		}
		a.Dest = strings.ReplaceAll(strings.TrimLeft(long, "-"), "-", "_")
	}
	if len(a.Flags) == 0 && !a.Optional && !a.Deferred && a.Kind != parsers {
		a.Required = true // argparse: a positional with nargs=None is required
	}
	p.args = append(p.args, a)
	return a
}

// Flag adds a store_true option.
func (p *Parser) Flag(flag, help string) *Arg {
	return p.Add(&Arg{Flags: []string{flag}, Kind: StoreTrue, Help: help, Default: false})
}

// Opt adds a string option.
func (p *Parser) Opt(flag, metavar, help string) *Arg {
	return p.Add(&Arg{Flags: []string{flag}, Kind: Store, Metavar: metavar, Help: help})
}

// IntOpt adds an int option.
func (p *Parser) IntOpt(flag, metavar, help string, def any) *Arg {
	return p.Add(&Arg{Flags: []string{flag}, Kind: StoreInt, Metavar: metavar, Help: help, Default: def})
}

// Pos adds a positional.
func (p *Parser) Pos(dest, help string) *Arg {
	return p.Add(&Arg{Dest: dest, Help: help})
}

// OptPos adds an optional positional (nargs='?').
func (p *Parser) OptPos(dest, help string) *Arg {
	return p.Add(&Arg{Dest: dest, Help: help, Optional: true})
}

// MutexGroup adds a mutually exclusive group.
func (p *Parser) MutexGroup(required bool) *Group {
	g := &Group{Required: required}
	p.groups = append(p.groups, g)
	return g
}

// Add puts an argument into the parser and the group.
func (g *Group) Add(p *Parser, a *Arg) *Arg {
	p.Add(a)
	a.group = g
	g.args = append(g.args, a)
	return a
}

// Subcommands adds the subcommand slot.
func (p *Parser) Subcommands(dest string, required bool) *Subparsers {
	s := &Subparsers{Dest: dest, Required: required, parsers: map[string]*Parser{}, owner: p}
	p.args = append(p.args, &Arg{Dest: dest, Kind: parsers, Required: required, sub: s})
	return s
}

// Add creates a subcommand parser.
func (s *Subparsers) Add(name, help string) *Parser {
	c := NewParser(s.owner.Prog+" "+name, "")
	c.Help = help
	c.parent = s.owner
	s.names = append(s.names, name)
	s.parsers[name] = c
	return c
}

// Names lists the subcommands in order.
func (s *Subparsers) Names() []string { return append([]string(nil), s.names...) }

// Get returns a subcommand parser.
func (s *Subparsers) Get(name string) *Parser { return s.parsers[name] }

// UsageError is a parse failure: print usage + "prog: error: msg", exit 2.
type UsageError struct {
	Parser *Parser
	Msg    string
}

func (e *UsageError) Error() string { return e.Msg }

// Exit is an early, successful exit (help or version) with text for stdout.
type Exit struct {
	Code int
	Text string
}

func (e *Exit) Error() string { return "exit" }

// Parse parses argv for p (the top-level parser).
func (p *Parser) Parse(argv []string) (*Namespace, error) {
	ns := &Namespace{Values: map[string]any{}, Leaf: p}
	extras, err := p.parseKnown(argv, ns)
	if err != nil {
		return nil, err
	}
	if len(extras) > 0 {
		return nil, &UsageError{p, "unrecognized arguments: " + strings.Join(extras, " ")}
	}
	return ns, nil
}

var negativeNumber = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

type optMatch struct {
	arg      *Arg
	flag     string
	explicit *string
}

// parseOptional classifies one argument string: nil for a positional, a
// single match with arg==nil for an unknown option, several for ambiguity.
func (p *Parser) parseOptional(s string) []optMatch {
	if s == "" || s[0] != '-' {
		return nil
	}
	if a := p.option(s); a != nil {
		return []optMatch{{a, s, nil}}
	}
	if len(s) == 1 {
		return nil
	}
	if name, val, ok := strings.Cut(s, "="); ok {
		if a := p.option(name); a != nil {
			v := val
			return []optMatch{{a, name, &v}}
		}
	}
	if strings.HasPrefix(s, "--") {
		prefix, val, hasEq := strings.Cut(s, "=")
		var out []optMatch
		for _, a := range p.args {
			for _, f := range a.Flags {
				if strings.HasPrefix(f, prefix) {
					m := optMatch{arg: a, flag: f}
					if hasEq {
						v := val
						m.explicit = &v
					}
					out = append(out, m)
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if negativeNumber.MatchString(s) {
		return nil
	}
	if strings.Contains(s, " ") {
		return nil
	}
	return []optMatch{{nil, s, nil}}
}

func (p *Parser) option(flag string) *Arg {
	for _, a := range p.args {
		for _, f := range a.Flags {
			if f == flag {
				return a
			}
		}
	}
	return nil
}

func (p *Parser) positionals() []*Arg {
	var out []*Arg
	for _, a := range p.args {
		if len(a.Flags) == 0 {
			out = append(out, a)
		}
	}
	return out
}

func argName(a *Arg) string {
	if len(a.Flags) > 0 {
		return strings.Join(a.Flags, "/")
	}
	if a.Metavar != "" {
		return a.Metavar
	}
	return a.Dest
}

func argErr(p *Parser, a *Arg, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if a != nil {
		msg = "argument " + argName(a) + ": " + msg
	}
	return &UsageError{p, msg}
}

func pyRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

func (p *Parser) parseKnown(argv []string, ns *Namespace) ([]string, error) {
	ns.Leaf = p
	for _, a := range p.args {
		if _, set := ns.Values[a.Dest]; !set && a.Kind != HelpFlag && a.Kind != VersionFlag {
			ns.Values[a.Dest] = a.Default
		}
	}
	// classify
	pattern := make([]byte, len(argv))
	optIdx := map[int][]optMatch{}
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--" {
			pattern[i] = '-'
			for j := i + 1; j < len(argv); j++ {
				pattern[j] = 'A'
			}
			break
		}
		if m := p.parseOptional(argv[i]); m != nil {
			optIdx[i] = m
			pattern[i] = 'O'
		} else {
			pattern[i] = 'A'
		}
	}
	seen := map[*Arg]bool{}
	seenNonDefault := map[*Arg]bool{}
	var extras []string
	positionals := p.positionals()

	take := func(a *Arg, values []string, explicitFlag bool) error {
		seen[a] = true
		if len(a.Flags) > 0 || len(values) > 0 {
			seenNonDefault[a] = true
			if a.group != nil {
				for _, other := range a.group.args {
					if other != a && seenNonDefault[other] {
						return argErr(p, a, "not allowed with argument %s", argName(other))
					}
				}
			}
		}
		switch a.Kind {
		case HelpFlag:
			return &Exit{0, p.FormatHelp()}
		case VersionFlag:
			return &Exit{0, p.Version + "\n"}
		case StoreTrue:
			ns.Values[a.Dest] = true
		case Append:
			if len(values) > 0 {
				prev, _ := ns.Values[a.Dest].([]string)
				ns.Values[a.Dest] = append(append([]string(nil), prev...), values[0])
			}
		case StoreInt:
			if len(values) == 0 {
				return nil
			}
			n, err := strconv.Atoi(strings.TrimSpace(values[0]))
			if err != nil {
				return argErr(p, a, "invalid int value: %s", pyRepr(values[0]))
			}
			ns.Values[a.Dest] = n
		case parsers:
			name := values[0]
			sub := a.sub.parsers[name]
			if sub == nil {
				q := make([]string, len(a.sub.names))
				for i, n := range a.sub.names {
					q[i] = pyRepr(n)
				}
				return argErr(p, a, "invalid choice: %s (choose from %s)", pyRepr(name), strings.Join(q, ", "))
			}
			ns.Values[a.Dest] = name
			more, err := sub.parseKnown(values[1:], ns)
			if err != nil {
				return err
			}
			extras = append(extras, more...)
		default:
			if len(values) > 0 {
				ns.Values[a.Dest] = values[0]
			}
		}
		return nil
	}

	// consumePositionals matches as many positionals as possible at start.
	consumePositionals := func(start int) (int, error) {
		rest := pattern[start:]
		counts := matchPartial(positionals, rest)
		for i, n := range counts {
			a := positionals[i]
			vals := append([]string(nil), argv[start:start+n]...)
			seg := pattern[start : start+n]
			if a.Kind == parsers {
				if len(seg) > 0 && seg[0] == '-' {
					vals = vals[1:]
				}
			} else if idx := strings.IndexByte(string(seg), '-'); idx >= 0 {
				vals = append(vals[:idx], vals[idx+1:]...)
			}
			start += n
			if err := take(a, vals, false); err != nil {
				return start, err
			}
		}
		positionals = positionals[len(counts):]
		return start, nil
	}

	consumeOptional := func(start int) (int, error) {
		matches := optIdx[start]
		if len(matches) > 1 {
			flags := make([]string, len(matches))
			for i, m := range matches {
				flags[i] = m.flag
			}
			return 0, &UsageError{p, fmt.Sprintf("ambiguous option: %s could match %s", argv[start], strings.Join(flags, ", "))}
		}
		m := matches[0]
		if m.arg == nil {
			extras = append(extras, argv[start])
			return start + 1, nil
		}
		a := m.arg
		takesValue := a.Kind == Store || a.Kind == StoreInt || a.Kind == Append
		if m.explicit != nil {
			if !takesValue {
				return 0, argErr(p, a, "ignored explicit argument %s", pyRepr(*m.explicit))
			}
			return start + 1, take(a, []string{*m.explicit}, true)
		}
		if !takesValue {
			return start + 1, take(a, nil, false)
		}
		next := start + 1
		// '-*A-*': skip a leading "--" marker, then one positional string
		i := next
		for i < len(pattern) && pattern[i] == '-' {
			i++
		}
		if i >= len(pattern) || pattern[i] != 'A' {
			return 0, argErr(p, a, "expected one argument")
		}
		val := argv[i]
		end := i + 1
		for end < len(pattern) && pattern[end] == '-' {
			end++
		}
		return end, take(a, []string{val}, false)
	}

	maxOpt := -1
	for i := range optIdx {
		if i > maxOpt {
			maxOpt = i
		}
	}
	start := 0
	for start <= maxOpt {
		next := start
		for next <= maxOpt {
			if _, ok := optIdx[next]; ok {
				break
			}
			next++
		}
		if start != next {
			end, err := consumePositionals(start)
			if err != nil {
				return nil, err
			}
			if end > start {
				start = end
				continue
			}
			start = end
		}
		if _, ok := optIdx[start]; !ok {
			extras = append(extras, argv[start:next]...)
			start = next
		}
		if start > maxOpt {
			break
		}
		end, err := consumeOptional(start)
		if err != nil {
			return nil, err
		}
		start = end
	}
	stop, err := consumePositionals(start)
	if err != nil {
		return nil, err
	}
	if stop < len(argv) {
		extras = append(extras, argv[stop:]...)
	}

	var missing []string
	for _, a := range p.args {
		if a.Required && !seen[a] {
			missing = append(missing, argName(a))
		}
	}
	if len(missing) > 0 {
		return nil, &UsageError{p, "the following arguments are required: " + strings.Join(missing, ", ")}
	}
	for _, g := range p.groups {
		if !g.Required {
			continue
		}
		hit := false
		var names []string
		for _, a := range g.args {
			hit = hit || seenNonDefault[a]
			names = append(names, argName(a))
		}
		if !hit {
			return nil, &UsageError{p, "one of the arguments " + strings.Join(names, " ") + " is required"}
		}
	}
	return extras, nil
}

// matchPartial is argparse's _match_arguments_partial for our nargs kinds:
// it returns how many strings each of the leading positionals takes.
func matchPartial(pos []*Arg, pattern []byte) []int {
	for n := len(pos); n >= 0; n-- {
		if counts, ok := matchSeq(pos[:n], pattern); ok {
			return counts
		}
	}
	return nil
}

func matchSeq(pos []*Arg, pattern []byte) ([]int, bool) {
	if len(pos) == 0 {
		return []int{}, true
	}
	a := pos[0]
	// leading "--" markers belong to the following positional
	lead := 0
	for lead < len(pattern) && pattern[lead] == '-' {
		lead++
	}
	try := func(take int) ([]int, bool) {
		n := lead + take
		// trailing "--" markers ('-*' after the value)
		for n < len(pattern) && pattern[n] == '-' && take > 0 {
			n++
			break
		}
		rest, ok := matchSeq(pos[1:], pattern[n:])
		if !ok {
			return nil, false
		}
		return append([]int{n}, rest...), true
	}
	hasA := lead < len(pattern) && pattern[lead] == 'A'
	switch {
	case a.Kind == parsers:
		if !hasA {
			return nil, false
		}
		// 'A' followed by everything
		counts := []int{len(pattern)}
		for range pos[1:] {
			counts = append(counts, 0)
		}
		if len(pos) > 1 {
			return nil, false
		}
		return counts, true
	case a.Optional || a.Deferred:
		if hasA {
			if c, ok := try(1); ok {
				return c, true
			}
		}
		rest, ok := matchSeq(pos[1:], pattern[lead:])
		if !ok {
			return nil, false
		}
		return append([]int{lead}, rest...), true
	default:
		if !hasA {
			return nil, false
		}
		return try(1)
	}
}

// ---------------------------------------------------------------- help

// Width is the help text width (terminal columns - 2, like argparse).
var Width = func() int { return termColumns() - 2 }

func (p *Parser) visible() []*Arg {
	var out []*Arg
	for _, a := range p.args {
		if !a.Hidden {
			out = append(out, a)
		}
	}
	return out
}

func metavarOf(a *Arg) string {
	if a.Metavar != "" {
		return a.Metavar
	}
	if len(a.Flags) > 0 {
		return strings.ToUpper(a.Dest)
	}
	if a.Kind == parsers {
		return "{" + strings.Join(a.sub.names, ",") + "}"
	}
	return a.Dest
}

func usagePart(a *Arg, inGroup, groupRequired bool) string {
	if len(a.Flags) == 0 {
		part := metavarOf(a)
		if a.Kind == parsers {
			part += " ..."
		} else if a.Optional && !inGroup {
			part = "[" + part + "]"
		}
		return part
	}
	var part string
	if a.Kind == Store || a.Kind == StoreInt || a.Kind == Append {
		part = a.Flags[0] + " " + metavarOf(a)
	} else {
		part = a.Flags[0]
	}
	if !(a.Required || a.Deferred || groupRequired || inGroup) {
		part = "[" + part + "]"
	}
	return part
}

// usageParts returns argparse's usage parts and where positionals start.
func (p *Parser) usageParts() ([]string, int) {
	actions := p.visible()
	used := map[*Arg]bool{}
	var parts []string
	emit := func(group []*Arg, required bool) {
		start := len(parts)
		in := len(group) > 1
		for _, a := range group {
			parts = append(parts, usagePart(a, in, required))
		}
		if in {
			open, close := "[", "]"
			if required {
				open, close = "(", ")"
			}
			parts[start] = open + parts[start]
			for i := start; i < len(parts)-1; i++ {
				parts[i] += " |"
			}
			parts[len(parts)-1] += close
		}
	}
	for _, a := range actions {
		if len(a.Flags) == 0 || used[a] {
			continue
		}
		used[a] = true
		if a.group != nil {
			group := []*Arg{a}
			for _, b := range a.group.args {
				if b != a && len(b.Flags) > 0 && !used[b] && !b.Hidden {
					used[b] = true
					group = append(group, b)
				}
			}
			emit(group, a.group.Required)
		} else {
			emit([]*Arg{a}, false)
		}
	}
	posStart := len(parts)
	for _, a := range actions {
		if len(a.Flags) == 0 {
			emit([]*Arg{a}, false)
		}
	}
	return parts, posStart
}

// FormatUsage is the "usage: ..." block (without the trailing blank line).
func (p *Parser) FormatUsage() string {
	prefix := "usage: "
	prog := p.Prog
	parts, posStart := p.usageParts()
	usage := strings.Join(append([]string{prog}, parts...), " ")
	textWidth := Width()
	if len(prefix)+len(usage) > textWidth {
		opt, pos := parts[:posStart], parts[posStart:]
		getLines := func(parts []string, indent string, prefix *string) []string {
			var lines, line []string
			lineLen := len(indent) - 1
			if prefix != nil {
				lineLen = len(*prefix) - 1
			}
			for _, part := range parts {
				if lineLen+1+len(part) > textWidth && len(line) > 0 {
					lines = append(lines, indent+strings.Join(line, " "))
					line = nil
					lineLen = len(indent) - 1
				}
				line = append(line, part)
				lineLen += len(part) + 1
			}
			if len(line) > 0 {
				lines = append(lines, indent+strings.Join(line, " "))
			}
			if prefix != nil && len(lines) > 0 {
				lines[0] = lines[0][len(indent):]
			}
			return lines
		}
		var lines []string
		if float64(len(prefix)+len(prog)) <= 0.75*float64(textWidth) {
			indent := strings.Repeat(" ", len(prefix)+len(prog)+1)
			if len(opt) > 0 {
				lines = getLines(append([]string{prog}, opt...), indent, &prefix)
				lines = append(lines, getLines(pos, indent, nil)...)
			} else if len(pos) > 0 {
				lines = getLines(append([]string{prog}, pos...), indent, &prefix)
			} else {
				lines = []string{prog}
			}
		} else {
			indent := strings.Repeat(" ", len(prefix))
			all := append(append([]string{}, opt...), pos...)
			lines = getLines(all, indent, nil)
			if len(lines) > 1 {
				lines = append(getLines(opt, indent, nil), getLines(pos, indent, nil)...)
			}
			lines = append([]string{prog}, lines...)
		}
		usage = strings.Join(lines, "\n")
	}
	return prefix + usage + "\n"
}

func invocation(a *Arg) string {
	if len(a.Flags) == 0 {
		return metavarOf(a)
	}
	if a.Kind == Store || a.Kind == StoreInt || a.Kind == Append {
		return strings.Join(a.Flags, ", ") + " " + metavarOf(a)
	}
	return strings.Join(a.Flags, ", ")
}

type helpItem struct {
	inv    string
	help   string
	indent int
}

// FormatHelp is the full --help text.
func (p *Parser) FormatHelp() string {
	width := Width()
	maxHelpPos := 24
	if m := width - 20; m < maxHelpPos {
		maxHelpPos = m
		if maxHelpPos < 4 {
			maxHelpPos = 4
		}
	}
	var pos, opts []helpItem
	maxLen := 0
	note := func(inv string, indent int) {
		if l := len(inv) + indent; l > maxLen {
			maxLen = l
		}
	}
	for _, a := range p.visible() {
		item := helpItem{invocation(a), a.Help, 2}
		note(item.inv, 2)
		if len(a.Flags) == 0 {
			pos = append(pos, item)
			if a.Kind == parsers {
				for _, name := range a.sub.names {
					sp := a.sub.parsers[name]
					if sp.Help == "" {
						continue
					}
					note(name, 4)
					pos = append(pos, helpItem{name, sp.Help, 4})
				}
			}
		} else {
			opts = append(opts, item)
		}
	}
	helpPos := maxLen + 2
	if helpPos > maxHelpPos {
		helpPos = maxHelpPos
	}
	helpWidth := width - helpPos
	if helpWidth < 11 {
		helpWidth = 11
	}
	var b strings.Builder
	b.WriteString(p.FormatUsage())
	b.WriteString("\n")
	if p.Description != "" {
		if p.RawDesc {
			b.WriteString(p.Description)
			if !strings.HasSuffix(p.Description, "\n") {
				b.WriteString("\n")
			}
		} else {
			for _, line := range wrapText(p.Description, maxInt(width, 11)) {
				b.WriteString(line + "\n")
			}
		}
		b.WriteString("\n")
	}
	section := func(title string, items []helpItem) {
		if len(items) == 0 {
			return
		}
		b.WriteString(title + ":\n")
		for _, it := range items {
			actionWidth := helpPos - it.indent - 2
			pad := strings.Repeat(" ", it.indent)
			first := 0
			switch {
			case strings.TrimSpace(it.help) == "":
				b.WriteString(pad + it.inv + "\n")
				continue
			case len(it.inv) <= actionWidth:
				b.WriteString(pad + it.inv + strings.Repeat(" ", actionWidth-len(it.inv)) + "  ")
			default:
				b.WriteString(pad + it.inv + "\n")
				first = helpPos
			}
			lines := wrapText(it.help, helpWidth)
			for i, line := range lines {
				if i == 0 {
					b.WriteString(strings.Repeat(" ", first) + line + "\n")
				} else {
					b.WriteString(strings.Repeat(" ", helpPos) + line + "\n")
				}
			}
		}
		b.WriteString("\n")
	}
	section("positional arguments", pos)
	section("options", opts)
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// wrapText is textwrap.wrap after collapsing whitespace, with argparse's
// hyphen breaks inside words ("non-empty" may break after "non-").
func wrapText(text string, width int) []string {
	words := strings.Fields(text)
	type chunk struct {
		s     string
		space bool // preceded by a space
	}
	var chunks []chunk
	for _, w := range words {
		for i, part := range hyphenChunks(w) {
			chunks = append(chunks, chunk{part, i == 0})
		}
	}
	var lines []string
	var cur strings.Builder
	curLen := 0
	flush := func() {
		if curLen > 0 {
			lines = append(lines, cur.String())
		}
		cur.Reset()
		curLen = 0
	}
	for _, c := range chunks {
		n := len([]rune(c.s))
		add := n
		if c.space && curLen > 0 {
			add++
		}
		if curLen+add <= width {
			if c.space && curLen > 0 {
				cur.WriteByte(' ')
			}
			cur.WriteString(c.s)
			curLen += add
			continue
		}
		if curLen > 0 {
			flush()
		}
		r := []rune(c.s)
		for len(r) > width {
			lines = append(lines, string(r[:width]))
			r = r[width:]
		}
		cur.WriteString(string(r))
		curLen = len(r)
	}
	flush()
	return lines
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127 && !(r >= '0' && r <= '9')
}

// hyphenChunks splits a word after hyphens that textwrap would break at:
// two letters (or letter-hyphen-letter) before it and letter[-]letter after.
func hyphenChunks(w string) []string {
	r := []rune(w)
	var out []string
	start := 0
	for i := 1; i < len(r)-1; i++ {
		if r[i] != '-' {
			continue
		}
		before := i >= 2 && isLetter(r[i-1]) && isLetter(r[i-2]) ||
			i >= 3 && isLetter(r[i-1]) && r[i-2] == '-' && isLetter(r[i-3])
		after := i+1 < len(r) && isLetter(r[i+1]) && (i+2 < len(r) && isLetter(r[i+2]) ||
			i+3 < len(r) && r[i+2] == '-' && isLetter(r[i+3]))
		if before && after {
			out = append(out, string(r[start:i+1]))
			start = i + 1
		}
	}
	return append(out, string(r[start:]))
}

// RequireDeferred returns argparse's "the following arguments are required"
// error for deferred arguments of the leaf parser that are still unset.
func (n *Namespace) RequireDeferred(skip func(dest string) bool) error {
	var missing []string
	for _, a := range n.Leaf.args {
		if a.Deferred && n.Values[a.Dest] == nil && (skip == nil || !skip(a.Dest)) {
			missing = append(missing, argName(a))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &UsageError{n.Leaf, "the following arguments are required: " + strings.Join(missing, ", ")}
}

// PrintUsageError writes argparse's usage error to w.
func PrintUsageError(w io.Writer, e *UsageError) {
	fmt.Fprint(w, e.Parser.FormatUsage())
	fmt.Fprintf(w, "%s: error: %s\n", e.Parser.Prog, e.Msg)
}
