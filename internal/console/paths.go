package console

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/royosherove/ajx/internal/res"
	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

type M = util.M

// Schema is the snapshot/since schema version.
const Schema = 1

// IdleTimeout stops a console server after this long without a request.
const IdleTimeout = 30 * time.Minute

const maxText = 4000

// KnownModels are offered in the model field; any other name the agent accepts still works.
var KnownModels = map[string][]string{"claude-code": {"sonnet", "opus", "haiku"}}

// AgainModes are the two ways to run a trial again.
var AgainModes = []string{"same-folder", "fresh-folder"}

// StageSets are the recovery commands the page offers for one run.
var StageSets = map[string]string{
	"teardown,render":                 "Retry task cleanup",
	"teardown,release,render,archive": "Retry task cleanup in the kept environment",
	"release,render":                  "Release the environment",
	"extract,measure,render":          "Re-run the analysis (makes reporter model calls)",
}

// promptLint is advisory only: words that can turn the worker's task into a guided or observed one.
var promptLint = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`(?i)\bAJX\b`), "names AJX"},
	{regexp.MustCompile(`(?i)\b(evaluat\w*|benchmark\w*|usability (?:test|study|review))\b`), "says the task is an evaluation"},
	{regexp.MustCompile(`(?i)\b(this is a test|you are being (?:tested|evaluated|observed|watched))\b`), "tells the worker it is observed"},
	{regexp.MustCompile(`(?i)\b(journey|friction|product asks?)\b`), "uses AJX reporting vocabulary"},
	{regexp.MustCompile(`(?i)\b(?:known|expected) (?:bugs?|issues?|problems?|errors?)\b`), "mentions an expected problem"},
	{regexp.MustCompile(`(?i)\bwork-?arounds?\b`), "suggests a workaround"},
}

var contentTypes = map[string]string{".html": "text/html; charset=utf-8", ".md": "text/plain; charset=utf-8",
	".txt": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8",
	".jsonl": "text/plain; charset=utf-8", ".csv": "text/plain; charset=utf-8",
	".json": "application/json; charset=utf-8", ".svg": "image/svg+xml", ".png": "image/png"}

var staticFiles = map[string]string{"console.js": "text/javascript; charset=utf-8", "console.css": "text/css; charset=utf-8"}

// PageCSP and ReportCSP are the Content-Security-Policy headers of the page and of served reports.
const (
	PageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	ReportCSP = "sandbox allow-scripts allow-popups allow-popups-to-escape-sandbox allow-downloads; default-src 'none'; " +
		"style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'"
)

// ---------------------------------------------------------------- Python-shaped formatting

// py formats a value the way a Python f-string does (None, True, ints, float repr).
func py(v any) string {
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
		return util.PyFloat(t)
	case float32:
		return util.PyFloat(float64(t))
	case int, int64, int32, uint64:
		return fmt.Sprint(t)
	case M, []any:
		return runner.PyRepr(t)
	}
	return fmt.Sprint(v)
}

// pstr is Python str(x or "") for a JSON-shaped value: nil becomes "".
func pstr(v any) string {
	if v == nil {
		return ""
	}
	return py(v)
}

// pyG is Python's format(f, "g").
func pyG(f float64) string { return strconv.FormatFloat(f, 'g', 6, 64) }

// clip is Python _clip: str(text), cut to limit characters with a trailing ellipsis.
func clip(text any, limit int) string {
	s := pstr(text)
	if util.RuneLen(s) <= limit {
		return s
	}
	return util.Clip(s, limit-1) + "…"
}

// clipOrNil is `_clip(x, n) or None`.
func clipOrNil(text any, limit int) any {
	if s := clip(text, limit); s != "" {
		return s
	}
	return nil
}

// eq is Python == for JSON-shaped scalars (numbers by value; None == None).
func eq(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if fa, ok := util.Num(a); ok {
		if fb, ok := util.Num(b); ok {
			return fa == fb
		}
		return false
	}
	if sa, ok := a.(string); ok {
		sb, ok := b.(string)
		return ok && sa == sb
	}
	if ba, ok := a.(bool); ok {
		bb, ok := b.(bool)
		return ok && ba == bb
	}
	return runner.PyEqual(a, b)
}

func intOf(v any) (int, bool) {
	if f, ok := util.Num(v); ok {
		return int(f), true
	}
	if s, ok := v.(string); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return n, true
		}
	}
	return 0, false
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readJSON reads a JSON file with Python number types; missing or malformed -> def.
func readJSON(path string, def any) any {
	v, err := runner.ReadJSON(path, def)
	if err != nil {
		return def
	}
	return v
}

// readMap reads a JSON object file (`read_json(p, {}) or {}`); never nil.
func readMap(path string) M {
	return util.MapOr(readJSON(path, nil))
}

// readMapOrNil reads a JSON object file; nil when missing or not an object.
func readMapOrNil(path string) M {
	return util.Map(readJSON(path, nil))
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// resolvePath is Python Path.resolve(): absolute, symlinks resolved for the
// part of the path that exists.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	dir, rest := filepath.Dir(abs), filepath.Base(abs)
	if dir == abs {
		return abs
	}
	return filepath.Join(resolvePath(dir), rest)
}

func expandUser(p string) string {
	if p == "~" {
		return util.Home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(util.Home(), p[2:])
	}
	return p
}

// pySuffix is Python PurePath.suffix.
func pySuffix(name string) string {
	i := strings.LastIndex(name, ".")
	if 0 < i && i < len(name)-1 {
		return name[i:]
	}
	return ""
}

// ---------------------------------------------------------------- paths and files

// TrialFile resolves a trial.toml path or the directory holding it.
func TrialFile(path string) (string, error) {
	p := expandUser(path)
	if isDir(p) {
		p = filepath.Join(p, "trial.toml")
	}
	p = resolvePath(p)
	if !isFile(p) {
		return "", Errorf("trial file not found: %s", p)
	}
	return p, nil
}

// StateDirName is the console's folder inside the trial directory.
const StateDirName = ".ajx-console"

// StateDir is <trial-dir>/.ajx-console, created (mode 0700) when missing.
func StateDir(trial string) string {
	d := filepath.Join(filepath.Dir(trial), StateDirName)
	_ = os.Mkdir(d, 0o700)
	return d
}

// writePrivate writes JSON readable only by the user (the server file carries the page's session key).
func writePrivate(path string, data any) error {
	out, err := util.MarshalIndent(data)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), os.Getpid()))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(out); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Short shows a path with the home directory as ~ (Python _short).
func Short(path string) string {
	home := util.Home()
	if home != "" && (path == home || strings.HasPrefix(path, home+string(os.PathSeparator))) {
		return "~" + path[len(home):]
	}
	return path
}

func alive(pid any) bool {
	n, ok := intOf(pid)
	return ok && n != 0 && util.PidAlive(n)
}

// isAjx reports whether pid is still an `ajx <verb>` process, so a reused pid
// is never signalled or shown as running: 1 yes, 0 no, -1 when `ps` cannot tell.
func isAjx(pid any, verb string) int {
	n, ok := intOf(pid)
	if !ok {
		return -1
	}
	cmd := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(n))
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.Output(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return -1
	}
	text := strings.TrimSpace(string(out))
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return -1
		}
		if text == "" {
			return 0
		}
	}
	command := " " + text + " "
	if strings.Contains(command, "ajx") && strings.Contains(command, " "+verb+" ") {
		return 1
	}
	return 0
}

func secondsSince(at any) any {
	s, ok := at.(string)
	if !ok {
		return nil
	}
	t, err := util.ParseTS(s)
	if err != nil {
		return nil
	}
	d := int(time.Since(t).Seconds())
	if d < 0 {
		d = 0
	}
	return d
}

// rawTOML is the trial file as written ({} when it cannot be read or parsed).
func rawTOML(trial string) M {
	data, err := os.ReadFile(trial)
	if err != nil {
		return M{}
	}
	m, err := parseTOML(string(data))
	if err != nil {
		return M{}
	}
	return m
}

func parseTOML(text string) (M, error) {
	var raw map[string]any
	if _, err := toml.Decode(text, &raw); err != nil {
		return nil, err
	}
	return util.MapOr(util.Normalize(raw)), nil
}

// load is (spec, error text): the page shows the trial as written, without the caller's secrets.
func load(trial string) (M, string) {
	sp, err := specLoad(trial)
	if err != nil {
		var se *spec.Error
		if errors.As(err, &se) {
			return nil, se.Msg
		}
		return nil, err.Error()
	}
	if sp == nil {
		return nil, "the trial could not be loaded"
	}
	return sp, ""
}

func outputDir(trial string, sp M) string {
	if sp != nil {
		return util.Str(util.Get(sp, "trial", "output_dir"))
	}
	raw := util.MapOr(rawTOML(trial)["trial"])
	base := filepath.Dir(trial)
	def := filepath.Join(base, "ajx-reports", util.OrStr(pstr(raw["product"]), "product"), util.OrStr(pstr(raw["id"]), "trial"))
	od := pstr(raw["output_dir"])
	if od == "" {
		return resolvePath(def)
	}
	if filepath.IsAbs(od) {
		return resolvePath(od)
	}
	return resolvePath(filepath.Join(base, od))
}

func promptPath(trial string, sp M) string {
	if sp != nil {
		return util.Str(util.Get(sp, "task", "prompt_path"))
	}
	name := pstr(util.Get(rawTOML(trial), "task", "prompt_file"))
	if name == "" {
		return ""
	}
	if filepath.IsAbs(name) {
		return resolvePath(name)
	}
	return resolvePath(filepath.Join(filepath.Dir(trial), name))
}

// ---------------------------------------------------------------- plan fingerprint

type digestEntry struct {
	link   bool
	rel    string
	target string
	size   int64
	mtime  int64
}

type digestCache struct {
	stats    []digestEntry
	digest   string
	complete bool
}

var (
	digestsMu sync.Mutex
	digests   = map[string]digestCache{} // root -> stats, digest: polling must not re-read unchanged files
)

func sameStats(a, b []digestEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// partsLess orders relative paths the way Python sorts Path objects (by parts).
func partsLess(a, b string) bool {
	pa, pb := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

// treeDigest is (digest, complete) over a directory's files, excluding the
// console's own state and caches (Python _tree_digest, byte for byte).
func treeDigest(root string) (string, bool) {
	const maxFiles, maxBytes = 2000, 64 << 20
	var rels []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != root {
				return fs.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rels = append(rels, filepath.ToSlash(rel))
		return nil
	})
	sort.Slice(rels, func(i, j int) bool { return partsLess(rels[i], rels[j]) })
	var entries []digestEntry
	var total int64
	for _, rel := range rels {
		parts := strings.Split(rel, "/")
		if util.Contains(parts, StateDirName) || util.Contains(parts, "__pycache__") {
			continue
		}
		p := filepath.Join(root, filepath.FromSlash(rel))
		lst, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if lst.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			entries = append(entries, digestEntry{link: true, rel: rel, target: target})
			continue
		}
		if !lst.Mode().IsRegular() {
			continue
		}
		total += lst.Size()
		entries = append(entries, digestEntry{rel: rel, size: lst.Size(), mtime: lst.ModTime().UnixNano()})
		if len(entries) > maxFiles || total > maxBytes {
			return sha256Hex([]byte("<too large>")), false
		}
	}
	digestsMu.Lock()
	cached, ok := digests[root]
	digestsMu.Unlock()
	if ok && sameStats(cached.stats, entries) {
		return cached.digest, cached.complete
	}
	h := sha256.New()
	for _, e := range entries {
		if e.link {
			h.Write([]byte("link\x00" + e.rel + "\x00" + e.target))
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(e.rel)))
		if err != nil {
			data = nil
		}
		sum := sha256.Sum256(data)
		h.Write([]byte("file\x00" + e.rel + "\x00"))
		h.Write(sum[:])
	}
	digest := hex.EncodeToString(h.Sum(nil))
	digestsMu.Lock()
	digests[root] = digestCache{stats: entries, digest: digest, complete: true}
	digestsMu.Unlock()
	return digest, true
}

// PlanInfo is what an approval covers, byte for byte: trial.toml, the task
// prompt, the starting files and the trial's own plugins. "complete" is false
// when those files are too large to fingerprint. Returns M{sha, complete, covers}.
func PlanInfo(trial string, sp M) (M, error) {
	data, err := os.ReadFile(trial)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte("ajx-console-plan-2\x00trial\x00"))
	h.Write(data)
	h.Write([]byte("\x00prompt\x00"))
	if pp := promptPath(trial, sp); pp != "" && isFile(pp) {
		if b, err := os.ReadFile(pp); err == nil {
			h.Write(b)
		}
	}
	complete, covers := true, []any{"trial.toml", "the task prompt"}
	fixture := ""
	if sp != nil {
		fixture = util.S(util.Get(sp, "task", "fixture_dir"))
	}
	for _, part := range []struct{ label, root string }{
		{"the starting files", fixture},
		{"the trial's plugins", filepath.Join(filepath.Dir(trial), "plugins")},
	} {
		if part.root != "" && isDir(part.root) {
			digest, ok := treeDigest(part.root)
			h.Write([]byte("\x00" + part.label + "\x00" + digest))
			complete = complete && ok
			covers = append(covers, part.label)
		}
	}
	return M{"sha": hex.EncodeToString(h.Sum(nil)), "complete": complete, "covers": covers}, nil
}

// PlanSha is the plan id: PlanInfo's sha. `ajx run --expect-plan` compares it.
func PlanSha(trial string, sp M) (string, error) {
	info, err := PlanInfo(trial, sp)
	if err != nil {
		return "", err
	}
	return util.S(info["sha"]), nil
}

func planSha(trial string, sp M) string {
	s, _ := PlanSha(trial, sp)
	return s
}

var (
	meaningsMu sync.Mutex
	meanings   map[string]string
)

// labelMeanings is label -> one-line meaning, from references/vocabulary.md.
func labelMeanings() M {
	meaningsMu.Lock()
	defer meaningsMu.Unlock()
	if len(meanings) == 0 {
		data, err := res.ReadFile("references/vocabulary.md")
		if err != nil {
			return M{}
		}
		meanings = map[string]string{}
		for _, line := range strings.Split(string(data), "\n") {
			raw := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
			if len(raw) != 2 {
				continue
			}
			first, second := strings.TrimSpace(raw[0]), strings.TrimSpace(raw[1])
			if _, name, ok := strings.Cut(first, " "); ok {
				name = strings.TrimSpace(name)
				if _, known := labels[name]; known {
					meanings[name] = second
				}
			}
		}
	}
	out := M{}
	for k, v := range meanings {
		out[k] = v
	}
	return out
}

func lintPrompt(text string) []any {
	found := []any{}
	for _, l := range promptLint {
		if m := l.re.FindString(text); m != "" {
			found = append(found, M{"match": m, "why": l.why})
		}
	}
	return found
}
