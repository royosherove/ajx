package planedit

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/royosherove/ajx/internal/render/pyfmt"
)

var section = map[string]string{"task": "task", "run": "run settings", "reporter": "reporter", "account": "task account"}

var fieldNames = map[string]string{"minutes": "time limit (min)", "attempts": "attempts", "budget": "budget", "cli": "agent",
	"model": "model", "models": "models compared", "config": "starting setup", "auth": "sign-in",
	"env": "runs in", "skills": "skills", "skillPaths": "skill folders", "kind": "kind", "image": "image",
	"network": "network", "required": "protections", "limits": "limits", "tools": "tools",
	"shell": "shell startup", "workspace": "workspace folder", "prompt": "prompt", "materials": "materials",
	"fixture": "starting files", "human": "human help", "setup": "setup commands", "on": "uses an account",
	"identity": "account check", "preflight": "preflight", "teardown": "cleanup", "parallel": "parallel",
	"report_minutes": "reporting time limit", "output_dir": "output folder", "effort": "effort",
	"order_seed": "order seed", "docker_host": "Docker socket", "extra": "other settings",
	"name": "name", "type": "type", "path": "path", "contains": "contains", "run": "command",
	"expect_exit": "expected exit code", "expect_stdout": "output pattern", "url": "URL",
	"expect_status": "expected status", "expect_body": "body pattern", "where": "runs on"}

var skipFields = setOf("id", "profile", "models_form", "label", "prompt_file")

type flatKey struct{ node, label, field string }

// sortKey is str() of the (node, label, field) tuple, which Python sorts by.
func (k flatKey) sortKey() string {
	return "(" + pyfmt.StrRepr(k.node) + ", " + pyfmt.StrRepr(k.label) + ", " + pyfmt.StrRepr(k.field) + ")"
}

func flat(model any) map[flatKey]any {
	out := map[flatKey]any{}
	for _, part := range []string{"task", "run", "reporter", "account"} {
		d := items(item(model, part))
		for _, k := range d.keys {
			if !skipFields[k] {
				out[flatKey{part, section[part], k}] = d.vals[k]
			}
		}
	}
	for i, c := range iter(item(model, "checks")) {
		d := items(c)
		for _, k := range d.keys {
			out[flatKey{"checks", fmt.Sprintf("check %d", i+1), k}] = d.vals[k]
		}
	}
	for _, e := range iter(item(model, "envs")) {
		d := items(e)
		for _, k := range d.keys {
			if !skipFields[k] {
				id := pyStr(item(e, "id"))
				out[flatKey{"env-" + id, "environment " + id, k}] = d.vals[k]
			}
		}
	}
	for _, w := range iter(item(model, "workers")) {
		d := items(w)
		for _, k := range d.keys {
			if !skipFields[k] {
				id := pyStr(item(w, "id"))
				out[flatKey{"worker-" + id, "worker " + id, k}] = d.vals[k]
			}
		}
	}
	return out
}

func shown(v any) string {
	if v == nil || Equal(v, "") || Equal(v, []any{}) {
		return "default"
	}
	switch t := v.(type) {
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			if d, ok := x.(*Dict); ok {
				parts[i] = pyStr(or(d.Get("run"), d.Get("name")))
			} else {
				parts[i] = pyStr(x)
			}
		}
		if s := strings.Join(parts, ", "); s != "" {
			return s
		}
		return "none"
	case *Dict:
		return "…"
	}
	text := pyStr(v)
	if utf8.RuneCountInString(text) <= 60 {
		return text
	}
	return string([]rune(text)[:57]) + "…"
}

func idSet(list any) map[string]bool {
	out := map[string]bool{}
	for _, x := range iter(list) {
		k, ok := hashKey(item(x, "id"))
		if !ok {
			k = pyStr(item(x, "id"))
		}
		out[k] = true
	}
	return out
}

func minus(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Change is one thing an edit changes, in words: the plan node it belongs to and the text.
type Change struct {
	Node string `json:"node"`
	Text string `json:"text"`
}

// Changes is what the edit from old to new changes, in words, for the
// unsaved bar and the journal.
func Changes(old, new any) (out []Change, err error) {
	err = catch(func() { out = changes(old, new) })
	if pe, ok := err.(*pyError); ok {
		switch pe.typ {
		case "KeyError", "TypeError", "AttributeError":
			return nil, &Error{Msg: "the plan is incomplete (" + pe.msg + "); reload the page"}
		}
	}
	return out, err
}

func changes(old, new any) []Change {
	a, b := flat(old), flat(new)
	out := []Change{}
	oldW, newW := idSet(item(old, "workers")), idSet(item(new, "workers"))
	oldE, newE := idSet(item(old, "envs")), idSet(item(new, "envs"))
	for _, i := range minus(newW, oldW) {
		out = append(out, Change{"worker-" + i, "worker " + i + " added"})
	}
	for _, i := range minus(oldW, newW) {
		out = append(out, Change{"map", "worker " + i + " removed"})
	}
	for _, i := range minus(newE, oldE) {
		out = append(out, Change{"env-" + i, "environment " + i + " added"})
	}
	for _, i := range minus(oldE, newE) {
		out = append(out, Change{"map", "environment " + i + " removed"})
	}
	oldChecks, newChecks := len(iter(item(old, "checks"))), len(iter(item(new, "checks")))
	if newChecks != oldChecks {
		out = append(out, Change{"checks", "success checks: " + strconv.Itoa(oldChecks) + " -> " + strconv.Itoa(newChecks)})
	}
	keys := map[flatKey]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	paths := make([]flatKey, 0, len(keys))
	for k := range keys {
		paths = append(paths, k)
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i].sortKey() < paths[j].sortKey() })
	for _, path := range paths {
		node := path.node
		if rest, ok := strings.CutPrefix(node, "worker-"); ok && !(oldW[rest] && newW[rest]) {
			continue
		}
		if rest, ok := strings.CutPrefix(node, "env-"); ok && !(oldE[rest] && newE[rest]) {
			continue
		}
		if node == "checks" && newChecks != oldChecks {
			continue
		}
		before, after := a[path], b[path]
		if Equal(before, after) {
			continue
		}
		if path.field == "prompt" {
			if strings.TrimRight(pyStr(or(before, "")), "\n") != strings.TrimRight(pyStr(or(after, "")), "\n") {
				out = append(out, Change{node, "task prompt edited"}) // saved with one final newline
			}
			continue
		}
		label := fieldNames[path.field]
		if label == "" {
			label = path.field
		}
		out = append(out, Change{node, path.label + " · " + label + ": " + shown(before) + " -> " + shown(after)})
	}
	return out
}

// DiffLines is the unified diff (one line of context) of a file before and after saving.
func DiffLines(before, after, name string) []string {
	out := []string{}
	for _, line := range UnifiedDiff(splitLinesKeep(before), splitLinesKeep(after), name+" (now)", name+" (after saving)", 1) {
		out = append(out, strings.TrimRight(line, "\n"))
	}
	return out
}

// splitLinesKeep is Python str.splitlines(keepends=True).
func splitLinesKeep(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			if r == '\r' && i+1 < len(s) && s[i+1] == '\n' {
				size = 2
			}
			out = append(out, s[start:i+size])
			start = i + size
		}
		i += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
