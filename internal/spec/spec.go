// Package spec ports lib/ajx/spec.py: trial file (trial.toml) loading,
// validation, plugin resolution and the seeded run plan.
package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/royosherove/ajx/internal/agentconfig"
	"github.com/royosherove/ajx/internal/builtin"
	"github.com/royosherove/ajx/internal/environments"
	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

type M = util.M

// Configs are the harness config modes a cell may use.
var Configs = []string{"clean", "user"}

// ShellStartup are the accepted shell_startup values.
var ShellStartup = []string{"inherit", "skip"}

// ShellStartupSkipEnv is applied (with an empty ZDOTDIR) when shell_startup =
// "skip": the agent's `zsh -c` tool shell reads $ZDOTDIR/.zshenv, so an empty
// ZDOTDIR keeps ~/.zshenv (for example an exported AWS_PROFILE) out of it;
// bash and sh read BASH_ENV / ENV (ShellStartupDrop).
var ShellStartupSkipEnv = map[string]string{}

// ShellStartupDrop are variables unset when shell_startup = "skip".
var ShellStartupDrop = []string{"BASH_ENV", "ENV"}

var (
	idPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,47}$`)
	notIDPattern = regexp.MustCompile(`[^a-z0-9._-]+`)
)

// idMatch is Python _ID.match: `$` also matches before one trailing newline.
func idMatch(s string) bool { return idPattern.MatchString(strings.TrimSuffix(s, "\n")) }

// idFullMatch is Python _ID.fullmatch.
func idFullMatch(s string) bool { return idPattern.MatchString(s) }

// Error is a trial-file problem (Python SpecError); the CLI exits 2 on it
// and prints "trial file error: <message>".
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Errorf builds a spec.Error.
func Errorf(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// IsError reports whether err is (or wraps) a spec.Error.
func IsError(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

// DecodeError is a trial.toml syntax error (Python tomllib.TOMLDecodeError,
// which spec.load does not convert to SpecError). Msg ends with
// "(at line L, column C)" like tomllib; the parser's wording differs.
type DecodeError struct {
	Msg       string
	Line, Col int
}

func (e *DecodeError) Error() string { return e.Msg }

// Preserve the concrete compatibility error and its parser location, while
// exposing all malformed input to callers as a trial-file error (exit 2).
func (e *DecodeError) Unwrap() error { return &Error{Msg: e.Msg} }

// TypeName is the Python exception class name.
func (e *DecodeError) TypeName() string { return "TOMLDecodeError" }

// PyError is a Python exception other than SpecError that spec.load lets
// propagate (TypeError, ValueError from malformed TOML values); the console
// shows it as "<TypeName>: <message>".
type PyError struct{ Type, Msg string }

func (e *PyError) Error() string { return e.Msg }

func (e *PyError) Unwrap() error { return &Error{Msg: e.Msg} }

// TypeName is the Python exception class name.
func (e *PyError) TypeName() string { return e.Type }

// pyTypeName is type(v).__name__ for TOML/JSON values.
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case string:
		return "str"
	case bool:
		return "bool"
	case int64, int, int32:
		return "int"
	case float64, float32:
		return "float"
	case M:
		return "dict"
	case []any:
		return "list"
	}
	if util.List(v) != nil {
		return "list"
	}
	return fmt.Sprintf("%T", v)
}

// pathPart is a value used as a path component (Path / value): strings only.
func pathPart(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", &PyError{"TypeError", fmt.Sprintf("unsupported operand type(s) for /: 'PosixPath' and '%s'", pyTypeName(v))}
}

func need(cond bool, format string, a ...any) error {
	if cond {
		return nil
	}
	return &Error{Msg: fmt.Sprintf(format, a...)}
}

// LoadOpts are spec.Load's optional arguments.
type LoadOpts struct {
	CleanupOnly bool
	// SkipReadiness skips only the checks that resolve the caller's
	// credentials (Python readiness=False: `ajx console` validates edits
	// without the trial's secrets exported).
	SkipReadiness bool
}

// PlanItem is one (cell, repetition) of the run plan.
type PlanItem struct {
	Cell M
	Rep  int
}

// DecodeTOML parses TOML text into the canonical JSON shape (util.Normalize).
func DecodeTOML(text string) (M, error) {
	raw := map[string]any{}
	if _, err := toml.Decode(text, &raw); err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			msg := pe.Message
			if r, size := utf8.DecodeRuneInString(msg); size > 0 {
				msg = string(unicode.ToUpper(r)) + msg[size:]
			}
			return nil, &DecodeError{Msg: fmt.Sprintf("%s (at line %d, column %d)", msg, pe.Position.Line, pe.Position.Col),
				Line: pe.Position.Line, Col: pe.Position.Col}
		}
		return nil, &DecodeError{Msg: err.Error()}
	}
	return util.MapOr(util.Normalize(raw)), nil
}

// Resolve is pathlib's Path(p).resolve(): absolute, symlinks resolved for the
// part of the path that exists.
func Resolve(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
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

func joinBase(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home := os.Getenv("HOME")
		if home == "" {
			home = util.Home()
		}
		return home + p[1:]
	}
	return p
}

// pyInt is Python int(v) for TOML/JSON values.
func pyInt(v any) (int64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, false
		}
		return int64(t), true
	case string:
		s := strings.TrimFunc(t, unicode.IsSpace)
		s = strings.ReplaceAll(s, "_", "")
		if s == "" || strings.HasPrefix(t, "_") || strings.HasSuffix(t, "_") || strings.Contains(t, "__") {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func setDefault(m M, key string, v any) {
	if _, ok := m[key]; !ok {
		m[key] = v
	}
}

// pyDict is dict(v) for a TOML table value (nil/falsy -> empty).
func pyDict(v any) (M, error) {
	if !util.Truthy(v) {
		return M{}, nil
	}
	switch t := v.(type) {
	case M:
		return util.Copy(t), nil
	case string:
		return nil, &PyError{"ValueError", fmt.Sprintf("dictionary update sequence element #0 has length %d; 2 is required",
			util.RuneLen(string([]rune(t)[:1])))}
	case []any:
		first := t[0]
		if s, ok := first.(string); ok {
			return nil, &PyError{"ValueError", fmt.Sprintf("dictionary update sequence element #0 has length %d; 2 is required", util.RuneLen(s))}
		}
		if l := util.List(first); l != nil {
			return nil, &PyError{"ValueError", fmt.Sprintf("dictionary update sequence element #0 has length %d; 2 is required", len(l))}
		}
		return nil, &PyError{"TypeError", fmt.Sprintf("cannot convert dictionary update sequence element #0 to a sequence")}
	}
	return nil, &PyError{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyTypeName(v))}
}

// iterItems is `for item in (raw.get(key) or [])`: a table iterates its keys.
func iterItems(v any) []any {
	if !util.Truthy(v) {
		return nil
	}
	if l := util.List(v); l != nil {
		return l
	}
	switch t := v.(type) {
	case M:
		out := []any{}
		for _, k := range util.SortedKeys(t) {
			out = append(out, k)
		}
		return out
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out
	}
	return []any{v}
}

func stripBytes(b []byte) []byte { return bytes.Trim(b, " \t\n\r\x0b\x0c") }

func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

func pathParts(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}

// tupleRepr is repr() of a Python tuple of strings: ('a',) or ('a', 'b').
func tupleRepr(items []string) string {
	if len(items) == 1 {
		return "(" + Repr(items[0]) + ",)"
	}
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = Repr(s)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// profileError converts a profile normalization/validation error into a
// spec.Error the way Python converts TypeError/ValueError into SpecError.
// Other failures (EnvironmentError, OSError, interruption) propagate.
func profileError(err error, prefix string) error {
	if err == nil {
		return nil
	}
	var v interface{ IsValue() bool }
	if errors.As(err, &v) && !v.IsValue() {
		return err
	}
	var t interface{ TypeName() string }
	if errors.As(err, &t) && v == nil {
		switch t.TypeName() {
		case "ValueError", "TypeError", "_InspectionChanged", "SpecError":
		default:
			return err
		}
	}
	var pe *fs.PathError
	if errors.As(err, &pe) || util.Interrupted(err) {
		return err
	}
	return &Error{Msg: prefix + err.Error()}
}

// Load parses and validates a trial file.
func Load(path string, o LoadOpts) (M, error) {
	readiness := !o.SkipReadiness
	cleanupOnly := o.CleanupOnly
	path = Resolve(path)
	if _, err := os.Stat(path); err != nil {
		return nil, Errorf("trial file not found: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Errorf("cannot read trial file %s: %v", path, err)
	}
	if !utf8.Valid(data) {
		return nil, Errorf("trial file %s is not valid UTF-8", path)
	}
	raw, err := DecodeTOML(string(data))
	if err != nil {
		return nil, err
	}
	base := filepath.Dir(path)
	plugin.Load(base)

	trial, err := pyDict(raw["trial"])
	if err != nil {
		return nil, err
	}
	idv, hasID := trial["id"]
	if !hasID {
		idv = ""
	}
	if err := need(idMatch(PyStr(idv)), "trial.id must be a short lowercase slug"); err != nil {
		return nil, err
	}
	if err := need(util.Truthy(trial["product"]), "trial.product is required"); err != nil {
		return nil, err
	}
	if s, ok := trial["product"].(string); ok && strings.TrimSpace(s) == "" {
		return nil, Errorf("trial.product is required")
	}
	for _, d := range []struct {
		k string
		v any
	}{{"product_version_cmd", nil}, {"workspace_root", "/tmp"}, {"repetitions", int64(1)}, {"parallel", int64(1)},
		{"timeout_seconds", int64(1800)}, {"max_budget_usd", nil}, {"order_seed", int64(0)}, {"cache", "isolated"},
		{"narrate_timeout_seconds", int64(900)}, {"environment", nil}, {"agent_configuration", nil},
		{"shell_startup", "inherit"}} {
		setDefault(trial, d.k, d.v)
	}
	product, err := pathPart(trial["product"])
	if err != nil {
		return nil, err
	}
	tid, err := pathPart(trial["id"])
	if err != nil {
		return nil, err
	}
	setDefault(trial, "output_dir", filepath.Join(base, "ajx-reports", product, tid))
	if err := need(trial["cache"] == "isolated" || trial["cache"] == "shared", "trial.cache must be isolated|shared"); err != nil {
		return nil, err
	}
	if err := need(trial["shell_startup"] == "inherit" || trial["shell_startup"] == "skip", "trial.shell_startup must be inherit|skip"); err != nil {
		return nil, err
	}
	for _, key := range []string{"repetitions", "parallel", "timeout_seconds", "narrate_timeout_seconds", "order_seed"} {
		n, ok := pyInt(trial[key])
		if !ok {
			return nil, Errorf("trial.%s must be an integer", key)
		}
		trial[key] = n
	}
	if err := need(trial["repetitions"].(int64) >= 1, "trial.repetitions must be >= 1"); err != nil {
		return nil, err
	}
	if err := need(trial["parallel"].(int64) >= 1, "trial.parallel must be >= 1"); err != nil {
		return nil, err
	}
	if err := need(trial["timeout_seconds"].(int64) >= 1 && trial["narrate_timeout_seconds"].(int64) >= 1,
		"timeouts must be >= 1 second"); err != nil {
		return nil, err
	}
	od, err := pathPart(trial["output_dir"])
	if err != nil {
		return nil, err
	}
	outputDir := Resolve(joinBase(base, od))
	trial["output_dir"] = outputDir
	wr, ok := trial["workspace_root"].(string)
	if !ok {
		return nil, &PyError{"TypeError", fmt.Sprintf("argument should be a str or an os.PathLike object where __fspath__ returns a str, not '%s'", pyTypeName(trial["workspace_root"]))}
	}
	trial["workspace_root"] = Resolve(expandUser(wr))

	savedPlan := M{}
	if cleanupOnly {
		v, err := util.ReadJSON(filepath.Join(outputDir, "matrix-plan.json"), M{})
		if err != nil {
			return nil, err
		}
		if util.Truthy(v) {
			m, ok := v.(M)
			if !ok {
				return nil, Errorf("saved matrix plan must be an object")
			}
			savedPlan = m
		}
	}
	savedProfiles, hasSaved := savedPlan["normalized_profiles"], false
	if savedProfiles != nil {
		hasSaved = true
		sp, ok := savedProfiles.(M)
		valid := ok && util.Int(sp["schema_version"], -1) == 1 && sp["schema_version"] != true
		if valid {
			for _, key := range []string{"environments", "agent_configurations"} {
				if _, isMap := sp[key].(M); !isMap {
					valid = false
				}
			}
		}
		if err := need(valid, "saved profile contract is invalid; cleanup requires the original matrix plan"); err != nil {
			return nil, err
		}
		if err := need(savedPlan["trial_file"] == path && savedPlan["trial_id"] == trial["id"] &&
			valuesEqual(savedPlan["product"], trial["product"]),
			"saved profile contract belongs to a different trial"); err != nil {
			return nil, err
		}
	}

	task, err := pyDict(raw["task"])
	if err != nil {
		return nil, err
	}
	if err := need(util.Truthy(task["prompt_file"]), "task.prompt_file is required"); err != nil {
		return nil, err
	}
	var promptPath string
	if hasSaved {
		promptPath = filepath.Join(outputDir, "task-prompt.md")
	} else {
		pf, err := pathPart(task["prompt_file"])
		if err != nil {
			return nil, err
		}
		promptPath = Resolve(joinBase(base, pf))
	}
	if _, err := os.Stat(promptPath); err != nil {
		return nil, Errorf("task prompt not found: %s", promptPath)
	}
	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		return nil, Errorf("cannot read task prompt %s: %v", promptPath, err)
	}
	if err := need(len(stripBytes(promptBytes)) > 0, "task prompt is empty"); err != nil {
		return nil, err
	}
	promptSha := util.Sha256Bytes(promptBytes)
	if hasSaved {
		if err := need(promptSha == savedPlan["task_prompt_sha256"], "saved task prompt does not match the original matrix plan"); err != nil {
			return nil, err
		}
	}
	if !utf8.Valid(promptBytes) {
		return nil, Errorf("task prompt %s is not valid UTF-8", promptPath)
	}
	task["prompt_path"] = promptPath
	task["prompt_sha256"] = promptSha
	task["prompt_text"] = string(promptBytes)
	setDefault(task, "materials", []any{})
	setDefault(task, "human", "unavailable")
	// The account the worker acts in, checked by doctor in the worker's own tool shell.
	ident := task["identity_cmd"]
	if l, isList := ident.([]any); isList {
		ok := len(l) > 0
		var argv []string
		for _, a := range l {
			s, isStr := a.(string)
			ok = ok && isStr
			argv = append(argv, s)
		}
		if err := need(ok, "task.identity_cmd list must hold strings"); err != nil {
			return nil, err
		}
		ident = util.ShellJoin(argv)
	}
	identStr, isStr := ident.(string)
	if err := need(ident == nil || (isStr && pyStrip(identStr) != ""),
		"task.identity_cmd must be a shell command string or an argv list"); err != nil {
		return nil, err
	}
	task["identity_cmd"] = ident
	if util.Truthy(task["fixture_dir"]) {
		fd, err := pathPart(task["fixture_dir"])
		if err != nil {
			return nil, err
		}
		fixture := Resolve(joinBase(base, fd))
		if !cleanupOnly {
			st, err := os.Stat(fixture)
			if err := need(err == nil && st.IsDir(), "fixture_dir not found: %s", fixture); err != nil {
				return nil, err
			}
		}
		task["fixture_dir"] = fixture
	} else {
		task["fixture_dir"] = nil
	}

	adapters := M{}
	if util.Truthy(raw["adapters"]) {
		table, ok := raw["adapters"].(M)
		if !ok {
			return nil, &PyError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'items'", pyTypeName(raw["adapters"]))}
		}
		for _, name := range util.SortedKeys(table) {
			conf, err := pyDict(table[name])
			if err != nil {
				return nil, err
			}
			adapters[name] = conf
		}
	}
	for _, name := range util.SortedKeys(adapters) {
		conf := adapters[name].(M)
		_, isList := conf["execute"].([]any)
		if err := need(isList && util.Truthy(conf["execute"]), "adapters.%s.execute must be a non-empty argv list", name); err != nil {
			return nil, err
		}
		if err := validateAdapterFields(conf, "adapters."+name); err != nil {
			return nil, err
		}
	}
	authProfiles := M{}
	if util.Truthy(raw["auth"]) {
		table, ok := raw["auth"].(M)
		if !ok {
			return nil, &PyError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'items'", pyTypeName(raw["auth"]))}
		}
		for _, name := range util.SortedKeys(table) {
			conf, err := pyDict(table[name])
			if err != nil {
				return nil, err
			}
			authProfiles[name] = conf
		}
	}
	authNames := plugin.Names("auth")
	for _, name := range util.SortedKeys(authProfiles) {
		t, _ := authProfiles[name].(M)["type"].(string)
		if err := need(util.Contains(authNames, t), "auth.%s.type must be one of %s", name, ReprList(authNames)); err != nil {
			return nil, err
		}
		if err := validateAuthFields(authProfiles[name].(M), "auth."+name); err != nil {
			return nil, err
		}
	}
	runner := M{"type": "local"}
	if util.Truthy(raw["runner"]) {
		if runner, err = pyDict(raw["runner"]); err != nil {
			return nil, err
		}
	}
	profiles := M{}
	for _, key := range []string{"environments", "agent_configurations"} {
		var table any
		if hasSaved {
			table = savedProfiles.(M)[key]
		} else if v, ok := raw[key]; ok {
			table = v
		} else {
			table = M{}
		}
		tm, ok := table.(M)
		if err := need(ok, "%s must contain named profile tables", key); err != nil {
			return nil, err
		}
		allIDs := true
		for name := range tm {
			if !idFullMatch(name) {
				allIDs = false
			}
		}
		if err := need(allIDs, "%s names must be short lowercase slugs", key); err != nil {
			return nil, err
		}
		if hasSaved {
			profiles[key] = tm
			continue
		}
		var norm M
		if key == "environments" {
			norm, err = environments.NormalizeProfiles(tm, base)
		} else {
			norm, err = agentconfig.NormalizeProfiles(tm, base)
		}
		if err != nil {
			return nil, profileError(err, "")
		}
		if norm == nil {
			norm = M{}
		}
		profiles[key] = norm
	}

	reporter, err := pyDict(raw["reporter"])
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"harness", "model", "effort", "auth"} {
		setDefault(reporter, k, nil)
	}
	if err := validateStrings(reporter, "reporter", "model", "effort"); err != nil {
		return nil, err
	}
	if err := validateStringList(task, "materials", "task"); err != nil {
		return nil, err
	}

	commands := map[string][]any{}
	for _, key := range []string{"setup", "verify", "teardown", "preflight"} {
		out, err := loadCommands(raw, key)
		if err != nil {
			return nil, err
		}
		commands[key] = out
	}
	env := M{}
	if util.Truthy(raw["env"]) {
		table, ok := raw["env"].(M)
		if !ok {
			return nil, &PyError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'items'", pyTypeName(raw["env"]))}
		}
		for k, v := range table {
			env[k] = PyStr(v)
		}
	}
	local, err := environments.NormalizeProfiles(M{"ajx-local": M{"backend": "local"}}, base)
	if err != nil {
		return nil, err
	}
	spec := M{
		"path": path, "sha256": util.Sha256Bytes(data), "trial": trial, "task": task,
		"setup": commands["setup"], "verify": commands["verify"], "teardown": commands["teardown"],
		"preflight": commands["preflight"], "reporter": reporter, "env": env, "adapters": adapters,
		"auth": authProfiles, "runner": runner, "cells": []any{},
		"environments": profiles["environments"], "agent_configurations": profiles["agent_configurations"],
		"_configuration_environment": util.Map(local["ajx-local"]),
		"_cleanup_only":              cleanupOnly,
	}

	_, rawHasRunner := raw["runner"]
	seen := map[string]bool{}
	var cells []any
	for i, item := range iterItems(raw["cells"]) {
		cell, err := pyDict(item)
		if err != nil {
			return nil, err
		}
		cid, hasCID := cell["id"]
		if !hasCID {
			cid = ""
		}
		if err := need(idMatch(PyStr(cid)), "cells[%d].id must be a short lowercase slug", i); err != nil {
			return nil, err
		}
		names := HarnessNames(spec)
		h, _ := cell["harness"].(string)
		if err := need(util.Contains(names, h) && cell["harness"] != nil, "cells[%d].harness %s unknown; available %s",
			i, Repr(cell["harness"]), ReprList(names)); err != nil {
			return nil, err
		}
		for _, d := range []struct {
			k string
			v any
		}{{"model", nil}, {"effort", nil}, {"config", "clean"}, {"args", []any{}}, {"env", M{}}, {"auth", nil},
			{"runner", nil}, {"environment", trial["environment"]}, {"agent_configuration", trial["agent_configuration"]},
			{"shell_startup", trial["shell_startup"]}} {
			setDefault(cell, d.k, d.v)
		}
		field := fmt.Sprintf("cells[%d]", i)
		if err := validateStrings(cell, field, "model", "effort"); err != nil {
			return nil, err
		}
		if err := validateEnv(cell, "env", field); err != nil {
			return nil, err
		}
		if err := need(cell["config"] == "clean" || cell["config"] == "user", "cells[%d].config must be clean|user", i); err != nil {
			return nil, err
		}
		if err := need(cell["shell_startup"] == "inherit" || cell["shell_startup"] == "skip",
			"cells[%d].shell_startup must be inherit|skip", i); err != nil {
			return nil, err
		}
		args, isList := cell["args"].([]any)
		allStr := isList
		for _, a := range args {
			if _, ok := a.(string); !ok {
				allStr = false
			}
		}
		if err := need(allStr, "cells[%d].args must be a list of strings", i); err != nil {
			return nil, err
		}
		for _, kt := range [][2]string{{"environment", "environments"}, {"agent_configuration", "agent_configurations"}} {
			ref := cell[kt[0]]
			s, isStr := ref.(string)
			_, known := util.MapOr(spec[kt[1]])[s]
			if err := need(ref == nil || (isStr && known), "cells[%d].%s must name a [%s.NAME] profile", i, kt[0], kt[1]); err != nil {
				return nil, err
			}
		}
		if util.Truthy(cell["environment"]) || util.Truthy(cell["agent_configuration"]) {
			if err := need(!util.Truthy(cell["runner"]) && !rawHasRunner,
				"cells[%d]: environment/agent profiles and legacy runner settings cannot be combined", i); err != nil {
				return nil, err
			}
			if err := need(trial["cache"] == "isolated", "cells[%d]: environments require trial.cache='isolated'", i); err != nil {
				return nil, err
			}
			if err := need(cell["config"] == "clean", "cells[%d]: environments require config='clean'", i); err != nil {
				return nil, err
			}
		}
		expanded, err := expandModels(cell, i)
		if err != nil {
			return nil, err
		}
		for _, one := range expanded {
			id := PyStr(one["id"])
			if err := need(!seen[id], "duplicate cell id %s", id); err != nil {
				return nil, err
			}
			seen[id] = true
			cells = append(cells, one)
			spec["cells"] = cells
		}
	}
	if err := need(len(cells) > 0, "at least one [[cells]] entry is required"); err != nil {
		return nil, err
	}
	if !util.Truthy(reporter["harness"]) {
		var candidate M
		for _, c := range util.Maps(cells) {
			h, err := HarnessFor(spec, util.Str(c["harness"]))
			if err != nil {
				return nil, err
			}
			if h.Base().CanReport {
				candidate = c
				break
			}
		}
		if candidate != nil {
			reporter["harness"] = candidate["harness"]
			if !util.Truthy(reporter["auth"]) {
				reporter["auth"] = candidate["auth"]
			}
		} else {
			reporter["harness"] = "claude-code"
		}
	}
	rh, _ := reporter["harness"].(string)
	if err := need(util.Contains(HarnessNames(spec), rh), "unknown reporter.harness %s", Repr(reporter["harness"])); err != nil {
		return nil, err
	}
	rhh, err := HarnessFor(spec, rh)
	if err != nil {
		return nil, err
	}
	if err := need(rhh.Base().CanReport, "reporter.harness %s has no restricted reporter adapter; "+
		"choose claude-code, codex, kiro-cli, or a plugin implementing report()", Repr(rh)); err != nil {
		return nil, err
	}
	for _, cell := range util.Maps(cells) {
		auth, err := AuthFor(spec, cell, "")
		if err != nil {
			return nil, err
		}
		environment := EnvironmentFor(spec, cell)
		configuration := AgentConfigurationFor(spec, cell)
		if err := validateCell(spec, cell, auth, environment, configuration, cleanupOnly, readiness); err != nil {
			return nil, profileError(err, fmt.Sprintf("cell %s: ", PyStr(cell["id"])))
		}
	}
	if _, err := AuthFor(spec, M{"id": "reporter", "harness": reporter["harness"], "auth": reporter["auth"]}, ""); err != nil {
		return nil, err
	}
	return spec, nil
}

func validateCell(spec, cell M, auth plugin.Auth, environment, configuration M, cleanupOnly, readiness bool) error {
	if environment != nil {
		b := auth.Base()
		if err := need(b.IsolatesConfig, "auth %s cannot use fresh HOME/config directories; "+
			"environments require an auth profile that supplies credentials to isolated configuration", Repr(b.Profile)); err != nil {
			return err
		}
		env := WorkerEnv(spec, cell, auth)
		withEnv := util.Copy(cell)
		withEnv["env"] = util.AnyMap(env)
		withEnv["unset_env"] = util.List(append([]string{}, auth.Unset()...))
		if err := environments.ValidateForCell(environment, withEnv); err != nil {
			return err
		}
		if !cleanupOnly && readiness {
			// Readiness resolves ${VAR} references against the caller's shell. `ajx status` and the
			// cleanup stages never start a worker, so they must not need the trial's secrets exported.
			problems := auth.EnvironmentProblems(env, environment)
			if err := need(len(problems) == 0, "auth %s is not ready for the selected environment: %s",
				Repr(b.Profile), strings.Join(problems, "; ")); err != nil {
				return err
			}
		}
	}
	if configuration != nil {
		h, err := HarnessFor(spec, util.Str(cell["harness"]))
		if err != nil {
			return err
		}
		if err := agentconfig.ValidateForCell(configuration, cell, h, auth, environment); err != nil {
			return err
		}
	}
	return nil
}

func valuesEqual(a, b any) bool {
	if fa, ok := util.Num(a); ok {
		fb, ok2 := util.Num(b)
		return ok2 && fa == fb
	}
	return a == b
}

func loadCommands(raw M, key string) ([]any, error) {
	out := []any{}
	checks := plugin.Names("check")
	for i, item := range iterItems(raw[key]) {
		m, ok := item.(M)
		if err := need(ok, "%s[%d] must be a table", key, i); err != nil {
			return nil, err
		}
		entry := util.Copy(m)
		setDefault(entry, "type", "shell")
		tv, has := m["timeout"]
		if !has {
			tv = int64(600)
		}
		t, ok := pyInt(tv)
		if !ok {
			return nil, Errorf("%s[%d].timeout must be an integer", key, i)
		}
		entry["timeout"] = t
		if t < 1 {
			return nil, Errorf("%s[%d].timeout must be >= 1 second", key, i)
		}
		if key != "verify" {
			if err := need(entry["type"] == "shell", "%s[%d].type must be shell (only [[verify]] has other check types)", key, i); err != nil {
				return nil, err
			}
		}
		if entry["type"] == "shell" {
			if err := need(util.Truthy(m["run"]), "%s[%d].run is required for type=shell", key, i); err != nil {
				return nil, err
			}
			run, isString := m["run"].(string)
			if err := need(isString && strings.TrimSpace(run) != "" && !strings.ContainsRune(run, '\x00'),
				"%s[%d].run must be a non-empty command string", key, i); err != nil {
				return nil, err
			}
		}
		// credential-error scanning opt-out, for a command that expects access to be denied
		entry["allow_auth_errors"] = util.Truthy(m["allow_auth_errors"])
		if util.Truthy(m["run"]) {
			setDefault(entry, "name", util.Clip(PyStr(m["run"]), 60))
		} else {
			setDefault(entry, "name", entry["type"])
		}
		if key == "verify" {
			typ, _ := entry["type"].(string)
			if err := need(util.Contains(checks, typ), "verify[%d].type %s unknown; available %s", i, Repr(entry["type"]), ReprList(checks)); err != nil {
				return nil, err
			}
			if typ == "file_exists" {
				pattern, isStr := entry["path"].(string)
				valid := isStr && pattern != "" && !strings.Contains(pattern, "\x00") && !strings.HasPrefix(pattern, "/") &&
					!util.Contains(pathParts(pattern), "..")
				if err := need(valid, "verify[%d].path must be a non-empty glob beneath the workspace, without '..'", i); err != nil {
					return nil, err
				}
				if _, err := builtin.FileCount(entry); err != nil {
					return nil, Errorf("verify[%d].%s", i, err)
				}
			}
			if typ == "http" {
				u, _ := entry["url"].(string)
				if err := builtin.ValidateHTTPURL(u); err != nil {
					return nil, Errorf("verify[%d].%s", i, err)
				}
			}
		}
		if err := validateCheckPatterns(entry, fmt.Sprintf("%s[%d]", key, i)); err != nil {
			return nil, err
		}
		if key == "verify" {
			setDefault(entry, "location", "host")
		} else {
			setDefault(entry, "location", "environment")
		}
		if err := need(entry["location"] == "host" || entry["location"] == "environment", "%s[%d].location must be host|environment", key, i); err != nil {
			return nil, err
		}
		if err := need(entry["type"] == "shell" || entry["location"] == "host", "%s[%d]: only shell checks can use location='environment'", key, i); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// expandModels: `models = ["sonnet", "opus"]` is one cell per model, ids
// `<id>-<model>`, grouped under `<id>`.
func expandModels(cell M, i int) ([]M, error) {
	models, has := cell["models"]
	delete(cell, "models")
	if !has || models == nil {
		return []M{cell}, nil
	}
	list, isList := models.([]any)
	valid := isList && len(list) > 0
	var names []string
	for _, m := range list {
		s, ok := m.(string)
		if !ok || pyStrip(s) == "" {
			valid = false
		}
		names = append(names, s)
	}
	if err := need(valid, "cells[%d].models must be a non-empty list of model names", i); err != nil {
		return nil, err
	}
	if err := need(!util.Truthy(cell["model"]), "cells[%d]: set model or models, not both", i); err != nil {
		return nil, err
	}
	if err := need(len(util.Uniq(names)) == len(names), "cells[%d].models lists a model twice", i); err != nil {
		return nil, err
	}
	var out []M
	for _, model := range names {
		suffix := strings.Trim(notIDPattern.ReplaceAllString(strings.ToLower(model), "-"), "-.")
		cid := PyStr(cell["id"]) + "-" + suffix
		if err := need(idMatch(cid), "cells[%d]: cell id %s derived from model %s is not a short "+
			"lowercase slug; shorten the id or write a separate [[cells]] entry for this model", i, Repr(cid), Repr(model)); err != nil {
			return nil, err
		}
		one := util.Copy(cell)
		one["id"], one["model"], one["group"] = cid, model, cell["id"]
		out = append(out, one)
	}
	return out, nil
}

// HarnessNames lists registered harnesses plus the trial's declarative adapters, sorted.
func HarnessNames(spec M) []string {
	names := append([]string{}, plugin.Names("harness")...)
	names = append(names, util.SortedKeys(util.MapOr(spec["adapters"]))...)
	return util.Uniq(names)
}

// WorkerEnv is the variables ajx sets for a worker: trial [env], then the
// cell's env, then the auth profile's.
func WorkerEnv(spec, cell M, auth plugin.Auth) map[string]string {
	env := util.StrMap(spec["env"])
	for k, v := range util.MapOr(cell["env"]) {
		env[k] = PyStr(v)
	}
	if auth == nil {
		return env
	}
	for k, v := range auth.Env() {
		env[k] = v
	}
	if EnvironmentFor(spec, cell) != nil {
		// Selecting an auth profile authorizes its explicitly required variables,
		// without inheriting unrelated caller credentials or harness settings.
		for _, name := range auth.Base().RequiredEnv {
			if _, ok := env[name]; ok {
				continue
			}
			if v, ok := os.LookupEnv(name); ok {
				env[name] = v
			}
		}
	}
	return env
}

// HarnessFor constructs the harness adapter for name: a trial's
// declarative [adapters.<name>] or a registered plugin.
func HarnessFor(spec M, name string) (plugin.Harness, error) {
	if conf, ok := util.MapOr(spec["adapters"])[name]; ok {
		return builtin.NewDeclarative(name, util.MapOr(conf)), nil
	}
	return plugin.NewHarness(name)
}

// AuthFor resolves a cell's auth: override, else the cell's auth, else the
// harness default. It may name a trial [auth.<profile>] or an auth plugin type.
func AuthFor(spec, cell M, override string) (plugin.Auth, error) {
	harnessName := util.Str(cell["harness"])
	h, err := HarnessFor(spec, harnessName)
	if err != nil {
		return nil, err
	}
	ref := override
	if ref == "" {
		ref = util.Str(cell["auth"])
	}
	if ref == "" {
		ref = h.Base().DefaultAuthName()
	}
	cid := PyStr(cell["id"])
	var typ string
	conf := M{}
	if profile := util.Map(util.MapOr(spec["auth"])[ref]); len(profile) > 0 {
		typ, conf = util.Str(profile["type"]), profile
	} else {
		names := plugin.Names("auth")
		if err := need(util.Contains(names, ref), "cell %s: auth %s is neither a [auth.*] profile "+
			"nor an auth type %s", cid, Repr(ref), ReprList(names)); err != nil {
			return nil, err
		}
		typ = ref
	}
	auth, err := plugin.NewAuth(typ, conf)
	if err != nil {
		return nil, err
	}
	b := auth.Base()
	_, isAdapter := util.MapOr(spec["adapters"])[harnessName]
	if err := need(util.Contains(b.Harnesses, "*") || util.Contains(b.Harnesses, harnessName) || isAdapter,
		"cell %s: auth type %s is for %s, not %s", cid, Repr(auth.Name()), tupleRepr(b.Harnesses), harnessName); err != nil {
		return nil, err
	}
	b.Profile = ref
	b.Harness = harnessName
	if util.Truthy(cell["environment"]) || util.Truthy(cell["agent_configuration"]) {
		environment := EnvironmentFor(spec, cell)
		// Resolve again at preparation, without caching secrets or binding host reporters
		// to the trial's worker environment.
		b.EnvironmentCheck = func() []string {
			return auth.EnvironmentProblems(WorkerEnv(spec, cell, auth), environment)
		}
	}
	return auth, nil
}

// RunnerFor constructs the cell's runner: the environment profile's runner,
// else the cell's or trial's legacy [runner], else local.
func RunnerFor(spec, cell M) (plugin.Runner, error) {
	if environment := EnvironmentFor(spec, cell); environment != nil {
		return environments.NewEnvironmentRunner(environment), nil
	}
	conf := util.Or(cell["runner"], spec["runner"])
	if s, ok := conf.(string); ok && s != "" {
		conf = M{"type": s}
	}
	m := util.Map(conf)
	if len(m) == 0 {
		m = M{"type": "local"}
	}
	typ := "local"
	if t, ok := m["type"]; ok {
		typ = util.Str(t)
	}
	return plugin.NewRunner(typ, m)
}

func cellOrTrial(spec, cell M, key string) any {
	if v, ok := cell[key]; ok {
		return v
	}
	return util.Get(spec, "trial", key)
}

// EnvironmentFor is the cell's normalized environment profile; the implicit
// local profile when only an agent configuration is selected; else nil.
func EnvironmentFor(spec, cell M) M {
	if ref := cellOrTrial(spec, cell, "environment"); util.Truthy(ref) {
		return util.Map(util.MapOr(spec["environments"])[util.Str(ref)])
	}
	if util.Truthy(cellOrTrial(spec, cell, "agent_configuration")) {
		return util.Map(spec["_configuration_environment"])
	}
	return nil
}

// AgentConfigurationFor is the cell's normalized agent configuration profile, or nil.
func AgentConfigurationFor(spec, cell M) M {
	if ref := cellOrTrial(spec, cell, "agent_configuration"); util.Truthy(ref) {
		return util.Map(util.MapOr(spec["agent_configurations"])[util.Str(ref)])
	}
	return nil
}

// RunPlan is the interleaved, seeded order: every cell once for rep 1
// (shuffled), then rep 2, ... onlyCells may name a cell or the id of a
// `models = [...]` group. The order matches Python's random.Random(order_seed).
func RunPlan(spec M, onlyCells []string) ([]PlanItem, error) {
	all := util.Maps(spec["cells"])
	known := map[string]bool{}
	for _, c := range all {
		known[PyStr(c["id"])] = true
		if util.Truthy(c["group"]) {
			known[PyStr(c["group"])] = true
		}
	}
	var unknown []string
	for _, c := range onlyCells {
		if !known[c] {
			unknown = append(unknown, c)
		}
	}
	if len(unknown) > 0 {
		return nil, Errorf("unknown cell id(s) %s; cells are %s", ReprList(unknown), ReprList(util.SortedKeys(known)))
	}
	var cells []M
	for _, c := range all {
		if len(onlyCells) == 0 || util.Contains(onlyCells, PyStr(c["id"])) ||
			(c["group"] != nil && util.Contains(onlyCells, PyStr(c["group"]))) {
			cells = append(cells, c)
		}
	}
	seed, _ := pyInt(util.Get(spec, "trial", "order_seed"))
	reps, _ := pyInt(util.Get(spec, "trial", "repetitions"))
	rng := newPyRandom(seed)
	var plan []PlanItem
	for rep := 1; rep <= int(reps); rep++ {
		order := append([]M{}, cells...)
		shuffle(rng, order)
		for _, c := range order {
			plan = append(plan, PlanItem{Cell: c, Rep: rep})
		}
	}
	return plan, nil
}

// sortedCopy returns a sorted copy (kept for callers that list names).
func sortedCopy(items []string) []string {
	out := append([]string{}, items...)
	sort.Strings(out)
	return out
}
