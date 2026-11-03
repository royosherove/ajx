// Package runner ports lib/ajx/runner.py: cell lifecycle orchestration. Each
// (cell, repetition) is one ordinary AJX run.
//
// Stages (resumable; each writes a marker in state.json):
//
//	prepare -> execute -> verify -> teardown -> normalize -> narrate -> release -> extract -> measure -> render -> archive
//
// Rules:
//   - A stage runs once its prerequisites reached a terminal status (done, error, or interrupted);
//     later stages degrade gracefully so the two primary artifacts exist even for a failed task.
//   - execute is never retried implicitly: a run whose worker crashed or was interrupted keeps
//     its evidence and is marked interrupted; redoing the task means a new repetition.
//   - No worker starts unless every [[preflight]] passed in that run's prepare stage.
//   - Teardown runs once execute has started, including on a later invocation after a crash. Its
//     stage status says it ran; state["teardown"]["status"] says whether it could have cleaned up.
//   - Archival moves evidence under the run dir after confirmed environment release.
//     Failed release preserves the original directories for cleanup; --keep-workspace retains them too.
//
// Interruption (Python's KeyboardInterrupt) is cancellation of the context
// passed to RunMatrix / Run.Go: the run stops the worker's process group, tears
// down, releases the environment, purges copied credentials and archives, then
// returns an error satisfying util.Interrupted. While that cleanup runs, a
// further SIGINT/SIGTERM/SIGHUP interrupts only the cleanup step in progress
// (recorded as interrupted) and cleanup continues with the next step.
package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/royosherove/ajx/internal/agentconfig"
	"github.com/royosherove/ajx/internal/util"
)

// M is a JSON-shaped object.
type M = util.M

// Stages is the lifecycle order (Python STAGES).
var Stages = []string{"prepare", "execute", "verify", "teardown", "normalize", "narrate", "release", "extract",
	"measure", "render", "archive"}

// CleanupStages may run with saved cleanup profiles (Python CLEANUP_STAGES).
var CleanupStages = []string{"teardown", "release", "render", "archive"}

// Prereqs lists each stage's prerequisites (Python PREREQS).
var Prereqs = map[string][]string{"execute": {"prepare"}, "verify": {"execute"}, "teardown": {"prepare"},
	"normalize": {"execute"}, "narrate": {"normalize"}, "extract": {"narrate"},
	"release": {"prepare"}, "measure": {"normalize"}, "render": {"measure"}, "archive": {"teardown"}}

// Terminal stage statuses.
var Terminal = []string{"done", "error", "interrupted"}

// NotRedoable stages cannot be redone with --stages.
var NotRedoable = []string{"prepare", "execute"}

// RedoWhenInterrupted is post-processing that an interruption left half done;
// a resume redoes it (never prepare/execute/cleanup).
var RedoWhenInterrupted = []string{"normalize", "narrate", "extract", "measure", "render"}

// NotRedoableAfterArchive: resume needs the original cwd and harness config.
var NotRedoableAfterArchive = []string{"narrate"}

// CredentialFiles are purged from copied harness config and agent homes at archive.
var CredentialFiles = []string{"auth.json", ".credentials.json", "credentials.json", "credentials", "token.json",
	"tokens.json", ".npmrc", ".netrc", ".pypirc", ".git-credentials"}

// JourneyUnavailable is the journey provenance when there is nothing to narrate.
const JourneyUnavailable = "unavailable"

// CacheVars point package managers at the per-run cache when trial.cache = "isolated".
var CacheVars = []string{"npm_config_cache", "YARN_CACHE_FOLDER", "PNPM_STORE_DIR", "UV_CACHE_DIR",
	"PIP_CACHE_DIR", "POETRY_CACHE_DIR", "GOCACHE", "GOMODCACHE", "BUN_INSTALL_CACHE_DIR",
	"DENO_DIR", "NUGET_PACKAGES", "GRADLE_USER_HOME"}

// DefaultProbes are the tool versions recorded for local runs.
var DefaultProbes = map[string][]string{"python": {"python3", "--version"}, "node": {"node", "--version"},
	"npm": {"npm", "--version"}, "uv": {"uv", "--version"}, "git": {"git", "--version"},
	"docker": {"docker", "--version"}}

// SensitiveNameHints select the caller env names recorded (names only) in environment.json.
var SensitiveNameHints = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "AWS_", "GOOGLE_", "ANTHROPIC_",
	"OPENAI_", "AZURE_", "GEMINI_", "CURSOR_", "COPILOT_", "GH_", "CLAUDE_CODE_USE"}

func isTerminal(status string) bool { return util.Contains(Terminal, status) }

// CleanupOnly reports whether stages is a non-empty subset of CleanupStages.
func CleanupOnly(stages []string) bool {
	if len(stages) == 0 {
		return false
	}
	for _, s := range stages {
		if !util.Contains(CleanupStages, s) {
			return false
		}
	}
	return true
}

// TaskInputKeys are the [task] values recorded in TaskInputs.
var TaskInputKeys = []string{"materials", "fixture_dir", "human", "identity_cmd"}

// TaskInputs is what attempts were checked and started with: the [[verify]]
// checks, lifecycle commands and task conditions. Recorded in matrix-plan.json
// so resuming cannot attribute old evidence to different checks or commands.
func TaskInputs(spec M) M {
	out := M{"verify": spec["verify"], "setup": spec["setup"], "preflight": spec["preflight"],
		"teardown": spec["teardown"]}
	task := util.MapOr(spec["task"])
	for _, k := range TaskInputKeys {
		out[k] = task[k]
	}
	// Relative to the trial folder, so moving both keeps the record equal.
	if fixture := util.S(out["fixture_dir"]); fixture != "" {
		if rel, err := filepath.Rel(filepath.Dir(util.S(spec["path"])), fixture); err == nil {
			out["fixture_dir"] = rel
		}
	}
	// Values JSON cannot hold (TOML dates and times) are recorded as Python's str() of them.
	return util.MapOr(datesAsText(out))
}

// datesAsText is json.loads(json.dumps(v, default=str)) for TOML dates and times.
func datesAsText(v any) any {
	switch t := v.(type) {
	case M:
		out := make(M, len(t))
		for k, x := range t {
			out[k] = datesAsText(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = datesAsText(x)
		}
		return out
	case time.Time:
		frac := ""
		if us := t.Nanosecond() / 1000; us != 0 {
			frac = fmt.Sprintf(".%06d", us)
		}
		switch t.Location().String() {
		case "date-local":
			return t.Format("2006-01-02")
		case "time-local":
			return t.Format("15:04:05") + frac
		case "datetime-local":
			return t.Format("2006-01-02 15:04:05") + frac
		}
		return t.Format("2006-01-02 15:04:05") + frac + t.Format("-07:00")
	}
	return v
}

var contractCellKeys = []string{"harness", "model", "effort", "config", "auth", "args", "runner", "environment",
	"agent_configuration", "shell_startup"}

// CellsContract is what every attempt in one output folder must share: cell
// definitions, the fixed reporter and the limits. New cells and repetitions may
// be added; changing these means a new output folder.
func CellsContract(spec M) M {
	cells := M{}
	for _, c := range util.Maps(spec["cells"]) {
		entry := M{}
		for _, k := range contractCellKeys {
			entry[k] = c[k]
		}
		env := util.SortedKeys(util.MapOr(c["env"]))
		envList := make([]any, len(env))
		for i, k := range env {
			envList[i] = k
		}
		entry["env"] = envList
		cells[util.Str(c["id"])] = entry
	}
	reporterSpec := util.MapOr(spec["reporter"])
	reporter := M{}
	for _, k := range []string{"harness", "model", "effort", "auth"} {
		reporter[k] = reporterSpec[k]
	}
	trial := util.MapOr(spec["trial"])
	limits := M{}
	for _, k := range []string{"timeout_seconds", "narrate_timeout_seconds", "max_budget_usd", "cache", "shell_startup"} {
		limits[k] = trial[k]
	}
	return M{"cells": cells, "reporter": reporter, "limits": limits}
}

// ContractChanges lists readable differences that would mix configurations in
// one output folder (added cells are fine). Python iterates the saved dicts in
// insertion order; Go reports keys in the Python key order for known parts and
// sorted order otherwise.
func ContractChanges(saved, current M) []string {
	var out []string
	savedCells := util.MapOr(saved["cells"])
	currentCells := util.MapOr(current["cells"])
	for _, cid := range orderedKeys(savedCells, nil) {
		before := util.MapOr(savedCells[cid])
		afterV, ok := currentCells[cid]
		if !ok || afterV == nil {
			continue
		}
		after := util.MapOr(afterV)
		for _, k := range orderedKeys(before, append(append([]string{}, contractCellKeys...), "env")) {
			if !PyEqual(before[k], after[k]) {
				out = append(out, fmt.Sprintf("cell %s %s: %s -> %s", cid, k, PyRepr(before[k]), PyRepr(after[k])))
			}
		}
	}
	parts := map[string][]string{"reporter": {"harness", "model", "effort", "auth"},
		"limits": {"timeout_seconds", "narrate_timeout_seconds", "max_budget_usd", "cache", "shell_startup"}}
	for _, part := range []string{"reporter", "limits"} {
		before, after := util.MapOr(saved[part]), util.MapOr(current[part])
		for _, k := range orderedKeys(before, parts[part]) {
			if !PyEqual(before[k], after[k]) {
				out = append(out, fmt.Sprintf("%s %s: %s -> %s", part, k, PyRepr(before[k]), PyRepr(after[k])))
			}
		}
	}
	return out
}

// orderedKeys returns m's keys: those in preferred first (in that order), then the rest sorted.
func orderedKeys(m M, preferred []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range preferred {
		if _, ok := m[k]; ok && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// ProfileContract is the saved matrix inputs with volatile source identities
// removed (Python _profile_contract), as a canonical string. Two contracts are
// equal when the profiles mean the same thing; numbers compare by value, so a
// profile read back from matrix-plan.json matches the one loaded from TOML.
func ProfileContract(profiles M) string {
	configs := M{}
	for name, profile := range util.MapOr(profiles["agent_configurations"]) {
		configs[name] = nilIfEmpty(agentconfig.ResumeContract(util.Map(profile)))
	}
	return CanonicalJSON(M{"environments": util.MapOr(profiles["environments"]), "agent_configurations": configs}, true)
}

// ---------------------------------------------------------------- lock

// LockedError is returned when another ajx holds the matrix lock; the CLI exits 3.
type LockedError struct{ Msg string }

func (e *LockedError) Error() string { return e.Msg }

// PidAlive reports whether a process with pid exists (Python _pid_alive).
func PidAlive(pid int) bool { return util.PidAlive(pid) }

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// AcquireLock takes outDir/.lock: one ajx per matrix directory. A second run
// would mark live workers interrupted and archive workspaces still in use.
// Created with O_EXCL, so two launches cannot both take it.
func AcquireLock(outDir string) (string, error) {
	path := filepath.Join(outDir, ".lock")
	data, err := util.MarshalIndent(M{"at": util.Now(), "host": hostname(), "pid": int64(os.Getpid())})
	if err != nil {
		return "", err
	}
	for i := 0; i < 3; i++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			info := util.MapOr(readJSONLoose(path))
			sameHost := util.S(info["host"]) == hostname()
			pid := lockPid(info["pid"])
			if sameHost && pid != 0 && PidAlive(pid) {
				return "", &LockedError{fmt.Sprintf("another ajx (pid %s) is running this matrix since %s; "+
					"stop it or wait. Remove %s only if that process is gone.", util.Str(info["pid"]), pyStr(info["at"]), path)}
			}
			if !sameHost {
				return "", &LockedError{fmt.Sprintf("lock held by %s (pid %s); remove %s if stale",
					pyStr(info["host"]), pyStr(info["pid"]), path)}
			}
			_ = os.Remove(path) // stale: its process is gone
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil {
			return "", werr
		}
		if cerr != nil {
			return "", cerr
		}
		return path, nil
	}
	return "", &LockedError{fmt.Sprintf("could not take %s; another ajx may be starting", path)}
}

func lockPid(v any) int {
	if f, ok := util.Num(v); ok {
		return int(f)
	}
	if s, ok := v.(string); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return n
		}
	}
	return 0
}

// readJSONLoose reads a JSON file; nil when missing or unparsable.
func readJSONLoose(path string) any {
	v, err := ReadJSON(path, nil)
	if err != nil {
		return nil
	}
	return v
}

// ReleaseLock removes the lock file (missing is fine).
func ReleaseLock(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
}

// ---------------------------------------------------------------- JSON with Python number types

// ReadJSON reads a JSON file like util.ReadJSON, but keeps integer literals as
// int64 (Python int) and the rest as float64, so values hash and print like
// Python's json.loads results. Missing file -> def.
func ReadJSON(path string, def any) (any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return def, nil
	}
	if err != nil {
		return nil, err
	}
	return DecodeJSON(data)
}

// DecodeJSON decodes JSON keeping integer literals as int64.
func DecodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("extra data after JSON value")
	}
	return fixNumbers(v), nil
}

func fixNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = fixNumbers(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = fixNumbers(x)
		}
		return t
	case json.Number:
		s := t.String()
		if !strings.ContainsAny(s, ".eE") {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return n
			}
		}
		f, _ := t.Float64()
		return f
	}
	return v
}

// ---------------------------------------------------------------- Python value formatting

// PyDumps is json.dumps(v, sort_keys=True) with Python's default separators
// and ensure_ascii=True: the exact bytes Python hashes for contracts.
func PyDumps(v any) string { return asciiEscape(util.PyJSON(v)) }

// CanonicalJSON is PyDumps, optionally formatting integral floats as integers
// (byValue) so JSON-decoded and TOML-decoded numbers compare equal.
func CanonicalJSON(v any, byValue bool) string {
	if byValue {
		v = intsByValue(v)
	}
	return PyDumps(v)
}

func intsByValue(v any) any {
	switch t := v.(type) {
	case M:
		out := make(M, len(t))
		for k, x := range t {
			out[k] = intsByValue(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = intsByValue(x)
		}
		return out
	case float64:
		if t == float64(int64(t)) && t < 1e15 && t > -1e15 {
			return int64(t)
		}
	}
	if l := util.List(v); l != nil {
		return intsByValue(l)
	}
	return v
}

func asciiEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x80 {
			b.WriteRune(r)
			continue
		}
		if r > 0xFFFF {
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
			continue
		}
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

// PyEqual is Python == for JSON-shaped values (1 == 1.0; nested by value).
func PyEqual(a, b any) bool {
	if fa, ok := util.Num(a); ok {
		if _, isBool := a.(bool); !isBool {
			fb, ok := util.Num(b)
			return ok && fa == fb
		}
	}
	switch ta := a.(type) {
	case nil:
		return b == nil
	case bool:
		tb, ok := b.(bool)
		return ok && ta == tb
	case string:
		tb, ok := b.(string)
		return ok && ta == tb
	case M:
		tb, ok := b.(M)
		if !ok || len(ta) != len(tb) {
			return false
		}
		for k, x := range ta {
			y, present := tb[k]
			if !present || !PyEqual(x, y) {
				return false
			}
		}
		return true
	}
	la, lb := util.List(a), util.List(b)
	if la != nil || lb != nil {
		if la == nil || lb == nil || len(la) != len(lb) {
			return false
		}
		for i := range la {
			if !PyEqual(la[i], lb[i]) {
				return false
			}
		}
		return true
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// PyRepr is Python's repr() for JSON-shaped values.
func PyRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return pyStrRepr(t)
	case float64:
		return util.PyFloat(t)
	case float32:
		return util.PyFloat(float64(t))
	case int64, int, int32, uint64:
		return util.Str(t)
	case M:
		var parts []string
		for _, k := range util.SortedKeys(t) {
			parts = append(parts, pyStrRepr(k)+": "+PyRepr(t[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case map[string]string:
		var parts []string
		for _, k := range util.SortedKeys(t) {
			parts = append(parts, pyStrRepr(k)+": "+pyStrRepr(t[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	if l := util.List(v); l != nil {
		parts := make([]string, len(l))
		for i, x := range l {
			parts[i] = PyRepr(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return fmt.Sprint(v)
}

// pyStr is Python str() of a value in an f-string ("None" for nil).
func pyStr(v any) string {
	switch v.(type) {
	case nil:
		return "None"
	case string:
		return v.(string)
	case M, []any:
		return PyRepr(v)
	}
	return util.Str(v)
}

func pyStrRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
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
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r < 0xa0:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// pyList formats a string list as Python's repr of a list.
func pyList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = pyStrRepr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
