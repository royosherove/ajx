package agentconfig

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/royosherove/ajx/internal/util"
)

// ---------------------------------------------------------------- errors

// failure carries an error up through panics inside this package (Python
// exceptions); exported functions recover it with catch.
type failure struct{ err error }

func fail(err error) { panic(failure{err}) }

// must is Python's _need: a false condition is a validation failure.
func must(cond bool, msg string) {
	if !cond {
		fail(&Error{Msg: msg})
	}
}

// check propagates an operating-system error unchanged (Python OSError).
func check(err error) {
	if err != nil {
		fail(err)
	}
}

func catch(errp *error) {
	if r := recover(); r != nil {
		if f, ok := r.(failure); ok {
			*errp = f.err
			return
		}
		panic(r)
	}
}

// pyErrName is the Python exception class name for an OS error.
func pyErrName(err error) string {
	var errno syscall.Errno
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "FileNotFoundError"
	case errors.Is(err, fs.ErrPermission):
		return "PermissionError"
	case errors.Is(err, fs.ErrExist):
		return "FileExistsError"
	case errors.As(err, &errno) && errno == syscall.ENOTDIR:
		return "NotADirectoryError"
	case errors.As(err, &errno) && errno == syscall.EISDIR:
		return "IsADirectoryError"
	}
	return "OSError"
}

// ---------------------------------------------------------------- snapshots

// tree is Python _read_tree's result; content is private (never published).
type tree struct {
	sha256   string
	files    []util.M
	bytes    int64
	identity util.M
	content  map[string][]byte
	order    []string // content keys in walk order
}

func (t *tree) records() []any {
	out := make([]any, len(t.files))
	for i, f := range t.files {
		out[i] = f
	}
	return out
}

func safeComponent(name string) bool {
	if strings.HasPrefix(name, ".") || deniedNames[casefold(name)] {
		return false
	}
	lower := strings.ToLower(name)
	for _, suffix := range []string{".pem", ".key", ".p12", ".pfx", ".pyc"} {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	for _, r := range name {
		if r < 32 {
			return false
		}
	}
	return true
}

func lstatPath(p string) rawStat {
	fi, err := os.Lstat(p)
	check(err)
	return statOf(fi)
}

func fstat(f *os.File) rawStat {
	fi, err := f.Stat()
	check(err)
	return statOf(fi)
}

func rootLstat(root *os.Root, name string) rawStat {
	fi, err := root.Lstat(name)
	check(err)
	return statOf(fi)
}

// readTree reads a skill tree through directory handles that never leave the
// root; symlinks and special files are rejected before any content is read.
// Identities are retained to reject even a changed-and-restored source between
// resolution and copy; content hashes are independent of timestamps.
func readTree(p string) *tree {
	t := &tree{identity: util.M{}, content: map[string][]byte{}}
	folded := map[string]bool{}

	var walk func(root *os.Root, dir *os.File, prefix string, depth int)
	walk = func(root *os.Root, dir *os.File, prefix string, depth int) {
		must(depth <= maxDepth, "skill tree exceeds maximum directory depth")
		before := fstat(dir)
		names, err := dir.Readdirnames(-1)
		check(err)
		sort.Strings(names)
		for _, name := range names {
			rel := name
			if prefix != "" {
				rel = prefix + "/" + name
			}
			// Empty .gitkeep/.keep placeholders are common in shipped skills. Copying
			// them keeps the recorded hash equal to the upstream artifact; any
			// content makes them ordinary hidden data, which stays rejected.
			placeholder := placeholderNames[name]
			must(safeComponent(name) || placeholder, "skill contains disallowed hidden, secret, or cache entry: "+rel)
			key := casefold(rel)
			must(!folded[key], "skill has a case-insensitive path collision: "+rel)
			folded[key] = true
			must(len(folded) <= maxFiles, "skill tree has too many entries")
			st := rootLstat(root, name)
			must(!st.isLink(), "skill symlinks are unsupported: "+rel+"; supply real files")
			must(st.isDir() || st.isReg(), "skill contains a special file: "+rel)
			must(!placeholder || (st.isReg() && st.size == 0),
				"skill contains disallowed hidden, secret, or cache entry: "+rel+" (.gitkeep/.keep placeholders must be empty)")
			must(st.mode&(sISUID|sISGID) == 0, "skill has privileged mode bits: "+rel)
			t.identity[rel] = identityValue(st.identity())
			if st.isDir() {
				sub, err := root.OpenRoot(name)
				check(err)
				child, err := sub.Open(".")
				if err != nil {
					sub.Close()
					check(err)
				}
				func() {
					defer sub.Close()
					defer child.Close()
					must(fstat(child).identity() == st.identity(), "skill changed while reading: "+rel)
					t.files = append(t.files, util.M{"path": rel, "type": "directory"})
					walk(sub, child, rel, depth+1)
					must(fstat(child).identity() == st.identity(), "skill changed while reading: "+rel)
					must(rootLstat(root, name).identity() == st.identity(), "skill changed while reading: "+rel)
				}()
				continue
			}
			child, err := root.Open(name)
			check(err)
			func() {
				defer child.Close()
				must(fstat(child).identity() == st.identity(), "skill changed while reading: "+rel)
				must(st.nlink == 1, "hard-linked skill file is unsupported: "+rel)
				must(st.size <= maxFile, "skill file exceeds size limit: "+rel)
				data, err := io.ReadAll(io.LimitReader(child, maxFile+1))
				check(err)
				must(int64(len(data)) <= maxFile, "skill file exceeds size limit: "+rel)
				must(!placeholder || len(data) == 0,
					"skill contains disallowed hidden, secret, or cache entry: "+rel+" (.gitkeep/.keep placeholders must be empty)")
				t.bytes += int64(len(data))
				must(t.bytes <= maxBytes, "skill content exceeds snapshot size limit")
				t.content[rel] = data
				t.order = append(t.order, rel)
				t.files = append(t.files, util.M{"path": rel, "type": "file", "bytes": int64(len(data)),
					"executable": st.mode&0o111 != 0, "sha256": util.Sha256Bytes(data)})
				must(fstat(child).identity() == st.identity(), "skill changed while reading: "+rel)
				must(rootLstat(root, name).identity() == st.identity(), "skill changed while reading: "+rel)
			}()
		}
		must(fstat(dir).identity() == before.identity(), "skill directory changed while reading")
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				if f, ok := r.(failure); ok {
					var ve *Error
					if !errors.As(f.err, &ve) {
						panic(failure{&Error{Msg: fmt.Sprintf(
							"cannot safely read skill snapshot (%s); check files and permissions", pyErrName(f.err))}})
					}
				}
				panic(r)
			}
		}()
		st := lstatPath(p)
		must(st.isDir() && !st.isLink(), "each skill path must be a real directory containing SKILL.md")
		root, err := os.OpenRoot(p)
		check(err)
		defer root.Close()
		dir, err := root.Open(".")
		check(err)
		defer dir.Close()
		must(fstat(dir).identity() == st.identity(), "skill directory changed while opening")
		t.identity[""] = identityValue(st.identity())
		walk(root, dir, "", 0)
		must(lstatPath(p).identity() == st.identity(), "skill source directory changed while reading")
	}()
	_, ok := t.content["SKILL.md"]
	must(ok, "each explicit skill directory must contain a regular SKILL.md")
	sort.SliceStable(t.files, func(i, j int) bool { return util.S(t.files[i]["path"]) < util.S(t.files[j]["path"]) })
	t.sha256 = jsonHash(t.records())
	return t
}

// jsonHash is sha256(json.dumps(value, sort_keys=True, separators=(",", ":"))).
func jsonHash(v any) string {
	return util.Sha256Bytes([]byte(pyDumps(v, jsonOpts{ascii: true, compact: true, sorted: true})))
}

// ---------------------------------------------------------------- SKILL.md

// scalar returns a plain or quoted scalar field without interpreting YAML escapes.
func scalar(fields map[string]string, key string) string {
	value := fields[key]
	if strings.HasPrefix(value, "'") || strings.HasPrefix(value, `"`) {
		must(len(value) >= 2 && value[len(value)-1] == value[0], "SKILL.md "+key+" has mismatched quotes")
		value = value[1 : len(value)-1]
	}
	return value
}

// toolGrants splits a declared allowed-tools scalar into grant strings without
// interpreting them. Grants separate on whitespace or commas outside
// parentheses, so the space-delimited Agent Skills form and Claude Code's
// comma form both keep grants such as Bash(git add:*) intact.
func toolGrants(value string) []string {
	grants := []string{}
	var token strings.Builder
	depth := 0
	for _, char := range value + " " {
		if char == '(' {
			depth++
		} else if char == ')' {
			must(depth > 0, "SKILL.md allowed-tools has unbalanced parentheses")
			depth--
		}
		if depth == 0 && (pyIsSpace(char) || char == ',') {
			if token.Len() > 0 {
				grants = append(grants, token.String())
			}
			token.Reset()
		} else {
			token.WriteRune(char)
		}
	}
	must(depth == 0, "SKILL.md allowed-tools has unbalanced parentheses")
	must(len(grants) <= maxGrants, fmt.Sprintf("SKILL.md allowed-tools declares more than %d grants", maxGrants))
	for _, grant := range grants {
		must(util.RuneLen(grant) <= maxScalar && pyIsPrintable(grant) && !strings.Contains(grant, "#"),
			"SKILL.md allowed-tools grant is oversized, unprintable, or contains '#': "+pyRepr(util.Clip(grant, 40)))
	}
	return grants
}

var (
	idPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	hashPattern  = regexp.MustCompile(`^[a-f0-9]{64}$`)
	fieldPattern = regexp.MustCompile(`^([a-z][a-z0-9-]*):(?:[ \t]+(.*)|[ \t]*)$`)
)

var allowedFields = map[string]bool{"name": true, "description": true, "license": true, "compatibility": true,
	"metadata": true, "disable-model-invocation": true, "user-invocable": true, "argument-hint": true,
	"allowed-tools": true, "version": true}

// frontmatter validates a deliberately small, portable subset without
// rewriting YAML. Unknown execution/dependency fields require a real
// harness-specific resolver, rather than guessing YAML semantics or silently
// enabling a hook.
func frontmatter(data []byte, folderName string) util.M {
	if !utf8.Valid(data) {
		fail(&Error{Msg: "SKILL.md must be UTF-8"})
	}
	lines := pySplitlines(string(data))
	must(len(lines) > 0 && lines[0] == "---", "SKILL.md requires YAML frontmatter with name and description")
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	must(end >= 0, "SKILL.md frontmatter needs a closing ---")
	fields := map[string]string{}
	active := ""
	for _, line := range lines[1:end] {
		if pyStrip(line) == "" || strings.HasPrefix(pyLstrip(line), "#") {
			continue
		}
		first, _ := utf8.DecodeRuneInString(line)
		if pyIsSpace(first) {
			must(active == "description" || active == "compatibility" || active == "metadata",
				"unsupported nested SKILL.md frontmatter; only descriptive metadata may be nested")
			continue
		}
		m := fieldPattern.FindStringSubmatch(line)
		must(m != nil, "unsupported SKILL.md frontmatter syntax; use plain, quoted, or block fields")
		key, value := m[1], pyStrip(m[2])
		must(allowedFields[key], "unsupported SKILL.md field "+pyRepr(key)+"; hooks, execution, and dependency fields need a resolver")
		_, dup := fields[key]
		must(!dup, "duplicate SKILL.md field: "+key)
		must(!strings.HasPrefix(value, "&") && !strings.HasPrefix(value, "*") && !strings.HasPrefix(value, "!") &&
			!strings.HasPrefix(value, "{") && !strings.HasPrefix(value, "["), "unsupported YAML value for SKILL.md "+key)
		fields[key], active = value, key
	}
	name := scalar(fields, "name")
	must(idPattern.MatchString(name) && !strings.Contains(name, "--"),
		"SKILL.md name must be a lowercase skill slug (up to 63 characters)")
	must(!strings.HasSuffix(name, "-"), "SKILL.md name cannot end in a hyphen")
	must(name == folderName, "SKILL.md name must exactly match its directory name")
	must(fields["description"] != "", "SKILL.md description is required")
	for _, key := range []string{"disable-model-invocation", "user-invocable"} {
		if v, ok := fields[key]; ok {
			must(v == "true" || v == "false", "SKILL.md "+key+" must be true or false")
		}
	}
	var version any
	if _, ok := fields["version"]; ok {
		v := scalar(fields, "version")
		must(util.RuneLen(v) <= maxScalar && pyIsPrintable(v), "SKILL.md version must be a short printable string")
		version = v
	}
	// allowed-tools is recorded for the manifest and reports, never enforced:
	// AJX has no permission resolver, and bypassed permission prompts ignore it.
	grants := []string{}
	if _, ok := fields["allowed-tools"]; ok {
		grants = toolGrants(scalar(fields, "allowed-tools"))
	}
	invocation := []any{}
	for _, k := range []string{"disable-model-invocation", "user-invocable"} {
		if _, ok := fields[k]; ok {
			invocation = append(invocation, k)
		}
	}
	return util.M{"name": name, "automatic_invocation": fields["disable-model-invocation"] != "true",
		"user_invocable": fields["user-invocable"] != "false", "invocation_fields": invocation,
		"declared_tool_grants": anyList(grants), "version": version}
}

func anyList(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

var (
	inlineLink    = pyRegexp(`!?\[[^\]\n]*\]\(\s*<?([^\s)>]+)`)
	referenceLink = pyRegexp(`(?m)^\s{0,3}\[[^\]\n]+\]:\s*<?([^\s>]+)`)
)

// pathSuffix is pathlib's PurePath.suffix of the final component.
func pathSuffix(rel string) string {
	name := rel[strings.LastIndex(rel, "/")+1:]
	if i := strings.LastIndex(name, "."); i > 0 {
		return name[i:]
	}
	return ""
}

// pyJoinNorm is posixpath.normpath(posixpath.join(dirname(rel), raw)).
func pyJoinNorm(rel, raw string) string {
	dir := ""
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		dir = strings.TrimRight(rel[:i+1], "/")
		if dir == "" {
			dir = "/"
		}
	}
	joined := raw
	if !strings.HasPrefix(raw, "/") {
		switch {
		case dir == "":
		case strings.HasSuffix(dir, "/"):
			joined = dir + raw
		default:
			joined = dir + "/" + raw
		}
	}
	if strings.HasPrefix(joined, "//") && !strings.HasPrefix(joined, "///") {
		return "/" + path.Clean(joined[1:]) // POSIX keeps exactly two leading slashes
	}
	return path.Clean(joined)
}

// checkSkillConfiguration rejects files that can add dependencies, hooks,
// permissions, agents or extra skills, and validates explicit local Markdown
// links without chasing them outside the chosen root or fetching remote content.
func checkSkillConfiguration(t *tree) {
	for _, entry := range t.files {
		rel := util.S(entry["path"])
		first := strings.SplitN(rel, "/", 2)[0]
		must(first != "agents" && first != "hooks" && first != "plugins" && first != "skills" &&
			rel != "plugin.json" && rel != "hooks.json" && rel != "mcp.json" && rel != "settings.json",
			"unsupported skill configuration or dependency at "+rel+"; provide a standalone portable skill")
	}
	known := map[string]bool{".": true}
	for _, entry := range t.files {
		known[util.S(entry["path"])] = true
	}
	for _, rel := range t.order {
		suffix := strings.ToLower(pathSuffix(rel))
		if suffix != ".md" && suffix != ".markdown" {
			continue
		}
		data := t.content[rel]
		if !utf8.Valid(data) {
			continue
		}
		text := string(data)
		var links []string
		for _, m := range inlineLink.FindAllStringSubmatch(text, -1) {
			links = append(links, m[1])
		}
		for _, m := range referenceLink.FindAllStringSubmatch(text, -1) {
			links = append(links, m[1])
		}
		for _, link := range links {
			uri, err := urlsplit(link)
			if err != nil {
				fail(&Error{Msg: "invalid link in skill file " + rel})
			}
			if uri.scheme != "" && uri.scheme != "file" && uri.scheme != "skill" {
				continue
			}
			raw := pyUnquote(uri.path)
			if raw == "" {
				continue // An anchor in the same document.
			}
			target := pyJoinNorm(rel, raw)
			must(uri.netloc == "" && !strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "~") &&
				!strings.HasPrefix(raw, `\`) && !strings.Contains(raw, `\`) && target != ".." &&
				!strings.HasPrefix(target, "../"),
				"local link in "+rel+" escapes the skill snapshot; bundle the referenced asset in the skill directory")
			must(known[target], "local link in "+rel+" references content missing from the skill snapshot")
		}
	}
}
