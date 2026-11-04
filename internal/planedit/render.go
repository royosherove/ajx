package planedit

import (
	"math"
	"strconv"
	"strings"

	"github.com/royosherove/ajx/internal/modelcatalog"
	"github.com/royosherove/ajx/internal/render/pyfmt"
)

// RenderOpts are Render's optional arguments.
type RenderOpts struct {
	Text    *string // the trial file's text; nil reads the file
	Current *Dict   // the file's own model; nil reads it from the text
}

// Rendered is what Render writes: the new trial.toml text, the new prompt
// text (nil when the model has none) and the sign-in profiles it adds.
type Rendered struct {
	Text      string
	Prompt    *string
	AddedAuth []string
}

// Render is the trial file for model. Only what differs from the file's own
// model is rewritten; everything else keeps its text and comments. A plan it
// cannot write is an *Error.
func Render(trial string, model any, o RenderOpts) (*Rendered, error) {
	var text string
	if o.Text != nil {
		text = *o.Text
	} else {
		t, err := readText(trial)
		if err != nil {
			return nil, err
		}
		text = t
	}
	current := o.Current
	if current == nil {
		c, err := ReadModelText(trial, text)
		if err != nil {
			return nil, err
		}
		current = c
	}
	if err := shape(model); err != nil {
		return nil, err
	}
	var out *Rendered
	err := catch(func() { out = (&renderer{}).render(text, model.(*Dict), current) })
	if pe, ok := err.(*pyError); ok && renderCatches[pe.typ] {
		return nil, &Error{Msg: "the plan has a value the console cannot write (" + pe.typ + ": " + pe.msg + "); reload the page"}
	}
	return out, err
}

// renderCatches are the exceptions Python's render turns into a PlanError
// (KeyError, TypeError, ValueError and its subclasses, AttributeError, IndexError).
var renderCatches = setOf("KeyError", "TypeError", "ValueError", "AttributeError", "IndexError",
	"UnicodeDecodeError", "TOMLDecodeError")

func shape(model any) error {
	d, ok := model.(*Dict)
	if ok {
		for _, k := range modelKeys {
			v := d.Get(k)
			if k == "checks" || k == "envs" || k == "workers" {
				_, ok = v.([]any)
			} else {
				_, ok = v.(*Dict)
			}
			if !ok {
				break
			}
		}
	}
	if !ok {
		return &Error{Msg: "the plan is incomplete; reload the page"}
	}
	return nil
}

// renderer carries Python's lenient context: values already in the file are
// rebuilt leniently, so only what an edit changes is validated and an odd
// value the user did not touch (one spec.load accepts) never blocks an
// unrelated edit.
type renderer struct{ lenient bool }

func (r *renderer) with(on bool, fn func()) {
	saved := r.lenient
	r.lenient = on
	defer func() { r.lenient = saved }()
	fn()
}

// known is lenient while building an item the edit left exactly as the file
// has it (or already lenient).
func (r *renderer) known(item any, known any, fn func()) {
	r.with(r.lenient || (known != nil && has(known, item)), fn)
}

func numStrict(v any, label string, low *float64) (float64, error) {
	if _, isBool := v.(bool); isBool {
		return 0, &Error{Msg: label + " must be a number"}
	}
	n, ok := pyFloat(v)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, &Error{Msg: label + " must be a number"}
	}
	if low != nil && n < *low {
		return 0, &Error{Msg: label + " must be at least " + strconv.FormatFloat(*low, 'g', 6, 64)}
	}
	return n, nil
}

func lo(f float64) *float64 { return &f }

// num is a number as a float; in lenient mode an invalid value is kept as it is.
func (r *renderer) num(v any, label string, low *float64) any {
	n, err := numStrict(v, label, low)
	if err != nil {
		if r.lenient {
			return v
		}
		panic(err)
	}
	return n
}

// whole is a whole number as an int; in lenient mode an invalid value is kept as it is.
func (r *renderer) whole(v any, label string, low *float64) any {
	n, err := numStrict(v, label, low)
	if err == nil && n != math.Trunc(n) {
		err = &Error{Msg: label + " must be a whole number"}
	}
	if err != nil {
		if r.lenient {
			return v
		}
		panic(err)
	}
	return int64(n)
}

func (r *renderer) modelName(v any, harness string) any {
	name := pyfmt.Strip(pyStr(or(v, "")))
	if name == "" {
		return nil
	}
	if r.lenient {
		return name
	}
	if strings.IndexFunc(name, pyfmt.IsSpace) >= 0 || strings.HasPrefix(name, "-") || len([]rune(name)) > 200 {
		fail("%s is not a model name an agent CLI accepts: use an alias such as sonnet, or the "+
			"provider's model id (no spaces)", pyfmt.StrRepr(name))
	}
	resolved, err := modelcatalog.Resolve(harness, name)
	if err != nil {
		fail("%s", err)
	}
	return resolved
}

// clean drops None values (Python {k: v for k, v in entry.items() if v is not None}).
func clean(d *Dict) *Dict {
	out := NewDict()
	for _, k := range d.keys {
		if d.vals[k] != nil {
			out.Set(k, d.vals[k])
		}
	}
	return out
}

func noneIfFalsy(v any) any {
	if truthy(v) {
		return v
	}
	return nil
}

func commandTables(list any) []any {
	out := []any{}
	for _, c := range orList(list) {
		run := pyStr(or(get(c, "run"), ""))
		if pyfmt.Strip(run) == "" {
			continue
		}
		entry := NewDict("name", noneIfFalsy(get(c, "name")), "run", run)
		extra := items(orDict(get(c, "extra")))
		for _, k := range extra.keys {
			if v := extra.vals[k]; v != nil && k != "name" && k != "run" {
				entry.Set(k, v)
			}
		}
		out = append(out, clean(entry))
	}
	return out
}

// stripped is a typed number as entered; the file's own value (any type) is kept as it is.
func stripped(v any) any {
	if s, ok := v.(string); ok {
		return pyfmt.Strip(s)
	}
	return v
}

func (r *renderer) checkTable(c any) *Dict {
	short := or(get(c, "type"), "cmd")
	typ := short
	if s, ok := hashKey(short); ok {
		if t, known := checkTOML[s]; known {
			typ = t
		}
	}
	entry := NewDict("name", noneIfFalsy(pyfmt.Strip(pyStr(or(get(c, "name"), "")))), "type", typ)
	switch {
	case Equal(short, "file"):
		entry.Set("path", pyfmt.Strip(pyStr(or(get(c, "path"), ""))))
		entry.Set("contains", noneIfFalsy(get(c, "contains")))
	case Equal(short, "cmd"):
		code := getDef(c, "expect_exit", "")
		run := pyfmt.Strip(pyStr(or(get(c, "run"), "")))
		var exit any
		if s := pyfmt.Strip(pyStr(code)); s != "" && s != "0" {
			exit = r.whole(stripped(code), "expected exit code", nil)
		}
		entry.Set("run", run)
		entry.Set("expect_exit", exit)
		entry.Set("expect_stdout", noneIfFalsy(get(c, "expect_stdout")))
	case Equal(short, "http"):
		status := getDef(c, "expect_status", "")
		url := pyfmt.Strip(pyStr(or(get(c, "url"), "")))
		var want any
		if s := pyfmt.Strip(pyStr(status)); s != "" && s != "200" {
			want = r.whole(stripped(status), "expected status", lo(100))
		}
		entry.Set("url", url)
		entry.Set("expect_status", want)
		entry.Set("expect_body", noneIfFalsy(get(c, "expect_body")))
	}
	if Equal(get(c, "where"), "worker") {
		entry.Set("location", "environment")
	}
	extra := items(orDict(get(c, "extra")))
	for _, k := range extra.keys {
		if v := extra.vals[k]; v != nil && !entry.Has(k) {
			entry.Set(k, v)
		}
	}
	return clean(entry)
}

func sameNumber(a, b any) bool {
	x, ok1 := pyFloat(a)
	y, ok2 := pyFloat(b)
	return ok1 && ok2 && x == y
}

func (r *renderer) envTable(e any) *Dict {
	var entry *Dict
	if Equal(get(e, "kind"), "container") {
		limits := NewDict()
		defaults := ContainerLimits()
		l := items(orDict(get(e, "limits")))
		for _, k := range l.keys {
			v := l.vals[k]
			if v == nil || Equal(v, "") || (defaults.Has(k) && sameNumber(v, defaults.Get(k))) {
				continue
			}
			label := pyStr(item(e, "id")) + " " + k
			if k == "cpus" {
				limits.Set(k, r.num(v, label, lo(0)))
			} else {
				limits.Set(k, r.whole(v, label, lo(0)))
			}
		}
		var required any = listOf(orList(get(e, "required")))
		if len(required.([]any)) == 0 {
			required = nil
		}
		var lim any = limits
		if limits.Len() == 0 {
			lim = nil
		}
		entry = NewDict("backend", "container", "image", pyfmt.Strip(pyStr(or(get(e, "image"), ""))),
			"network", or(get(e, "network"), "none"), "required", required, "limits", lim,
			"docker_host", noneIfFalsy(pyfmt.Strip(pyStr(or(get(e, "docker_host"), "")))))
	} else {
		entry = NewDict("backend", "local")
	}
	var tools []any
	for _, t := range orList(get(e, "tools")) {
		if truthy(get(t, "name")) {
			tools = append(tools, t)
		}
	}
	if len(tools) > 0 {
		out := make([]any, len(tools))
		for i, t := range tools {
			argv := or(get(t, "argv"), []any{item(t, "name"), "--version"})
			out[i] = NewDict("name", item(t, "name"), "argv", listOf(argv), "required", truthy(getDef(t, "required", true)))
		}
		entry.Set("tools", out)
	}
	extra := items(orDict(get(e, "extra")))
	for _, k := range extra.keys {
		if v := extra.vals[k]; v != nil && !entry.Has(k) {
			entry.Set(k, v)
		}
	}
	return clean(entry)
}

var defaults = map[[2]string]any{
	{"trial", "timeout_seconds"}: int64(1800), {"trial", "repetitions"}: int64(1), {"trial", "max_budget_usd"}: nil,
	{"trial", "parallel"}: int64(1), {"trial", "narrate_timeout_seconds"}: int64(900), {"trial", "order_seed"}: int64(0),
	{"trial", "workspace_root"}: "/tmp", {"trial", "shell_startup"}: "inherit", {"trial", "output_dir"}: nil,
	{"task", "materials"}: nil, {"task", "fixture_dir"}: nil, {"task", "human"}: "unavailable",
	{"task", "identity_cmd"}: nil, {"reporter", "harness"}: nil, {"reporter", "model"}: nil, {"reporter", "auth"}: nil,
}

// scalar is a trial.toml key, the model fields it comes from and how to compute it.
// Only keys whose model fields changed are converted and written.
type scalar struct {
	table, name string
	sources     [][2]string
	compute     func(r *renderer, m *Dict) any
}

func run(m *Dict) any    { return item(m, "run") }
func taskOf(m *Dict) any { return item(m, "task") }
func minutes(r *renderer, v any, label string) any {
	n := r.num(v, label, lo(0.1))
	f, ok := n.(float64)
	if !ok {
		raise("TypeError", "type %s doesn't define __round__ method", typeName(n))
	}
	return int64(math.RoundToEven(f * 60))
}

var scalars = []scalar{
	{"trial", "timeout_seconds", [][2]string{{"run", "minutes"}},
		func(r *renderer, m *Dict) any { return minutes(r, item(run(m), "minutes"), "time limit") }},
	{"trial", "repetitions", [][2]string{{"run", "attempts"}},
		func(r *renderer, m *Dict) any { return r.whole(item(run(m), "attempts"), "attempts", lo(1)) }},
	{"trial", "max_budget_usd", [][2]string{{"run", "budget"}}, func(r *renderer, m *Dict) any {
		if pyfmt.Strip(pyStr(getDef(run(m), "budget", ""))) == "" {
			return nil
		}
		return r.num(item(run(m), "budget"), "budget", lo(0.01))
	}},
	{"trial", "parallel", [][2]string{{"run", "parallel"}},
		func(r *renderer, m *Dict) any {
			return r.whole(getDef(run(m), "parallel", int64(1)), "parallel", lo(1))
		}},
	{"trial", "narrate_timeout_seconds", [][2]string{{"run", "report_minutes"}},
		func(r *renderer, m *Dict) any {
			return minutes(r, item(run(m), "report_minutes"), "reporting time limit")
		}},
	{"trial", "order_seed", [][2]string{{"run", "order_seed"}},
		func(r *renderer, m *Dict) any {
			return r.whole(getDef(run(m), "order_seed", int64(0)), "order seed", nil)
		}},
	{"trial", "workspace_root", [][2]string{{"run", "workspace"}},
		func(r *renderer, m *Dict) any { return pyfmt.Strip(pyStr(or(get(run(m), "workspace"), "/tmp"))) }},
	{"trial", "shell_startup", [][2]string{{"run", "shell"}}, func(r *renderer, m *Dict) any {
		if s := get(run(m), "shell"); !Equal(s, "inherit") && !Equal(s, "skip") {
			fail("shell startup must be inherit or skip")
		}
		return item(run(m), "shell")
	}},
	{"trial", "output_dir", [][2]string{{"run", "output_dir"}},
		func(r *renderer, m *Dict) any {
			return noneIfFalsy(pyfmt.Strip(pyStr(or(get(run(m), "output_dir"), ""))))
		}},
	{"task", "materials", [][2]string{{"task", "materials"}}, func(r *renderer, m *Dict) any {
		out := []any{}
		for _, y := range orList(get(taskOf(m), "materials")) {
			if x := pyfmt.Strip(pyStr(y)); x != "" {
				out = append(out, x)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}},
	{"task", "fixture_dir", [][2]string{{"task", "fixture"}},
		func(r *renderer, m *Dict) any {
			return noneIfFalsy(pyfmt.Strip(pyStr(or(get(taskOf(m), "fixture"), ""))))
		}},
	{"task", "human", [][2]string{{"task", "human"}},
		func(r *renderer, m *Dict) any { return or(get(taskOf(m), "human"), "unavailable") }},
	{"task", "identity_cmd", [][2]string{{"account", "identity"}, {"account", "on"}}, func(r *renderer, m *Dict) any {
		acc := item(m, "account")
		if !truthy(get(acc, "on")) {
			return nil
		}
		return noneIfFalsy(pyfmt.Strip(pyStr(or(get(acc, "identity"), ""))))
	}},
	{"reporter", "harness", [][2]string{{"reporter", "cli"}},
		func(r *renderer, m *Dict) any { return noneIfFalsy(get(item(m, "reporter"), "cli")) }},
	{"reporter", "model", [][2]string{{"reporter", "model"}, {"reporter", "cli"}},
		func(r *renderer, m *Dict) any {
			harness := get(item(m, "reporter"), "cli")
			if !truthy(harness) {
				harness = AutoReporter(iter(item(m, "workers")), NewDict())
			}
			return r.modelName(get(item(m, "reporter"), "model"), pyStr(harness))
		}},
	{"reporter", "auth", [][2]string{{"reporter", "auth"}},
		func(r *renderer, m *Dict) any { return noneIfFalsy(get(item(m, "reporter"), "auth")) }},
}

var groupNames = []string{"setup", "preflight", "teardown", "verify"}

// groups are the command and check groups; known is the file's own model
// (its unchanged checks are not validated again).
func (r *renderer) groups(m *Dict, known *Dict) map[string][]any {
	checks := []any{}
	var knownChecks any
	if truthy(known) {
		knownChecks = item(known, "checks")
	}
	for _, c := range iter(item(m, "checks")) {
		r.known(c, knownChecks, func() { checks = append(checks, r.checkTable(c)) })
	}
	acc := item(m, "account")
	preflight := []any{}
	setup := commandTables(get(item(m, "task"), "setup"))
	if truthy(get(acc, "on")) {
		preflight = commandTables(get(acc, "preflight"))
	}
	return map[string][]any{"setup": setup, "preflight": preflight, "teardown": commandTables(get(acc, "teardown")),
		"verify": checks}
}

func (r *renderer) envTables(m *Dict, known *Dict) *Dict {
	out := NewDict()
	var knownEnvs any
	if truthy(known) {
		knownEnvs = item(known, "envs")
	}
	for _, e := range iter(item(m, "envs")) {
		if !Equal(item(e, "kind"), "folder") {
			id, _ := hashKey(item(e, "id"))
			r.known(e, knownEnvs, func() { out.Set(id, r.envTable(e)) })
		}
	}
	return out
}

func skillsOf(table any) (bool, []any) {
	skills := orDict(get(orDict(table), "skills"))
	mode := get(skills, "mode")
	paths := []any{}
	for _, x := range orList(get(skills, "paths")) {
		paths = append(paths, pyStr(x))
	}
	return Equal(mode, "selected") || Equal(mode, "snapshot"), paths
}

// configName is a name for a new agent configuration that does not replace a different existing one.
func configName(base string, table any, configs, wanted *Dict) string {
	name, n, stem := base, 2, base
	if r := []rune(base); len(r) > 44 {
		stem = string(r[:44])
	}
	for (configs.Has(name) && !Equal(configs.Get(name), table)) || (wanted.Has(name) && !Equal(wanted.Get(name), table)) {
		name, n = stem+"-"+strconv.Itoa(n), n+1
	}
	return name
}

type addedAuth struct{ name, harness string }

func addedHas(added []addedAuth, name any) bool {
	for _, a := range added {
		if Equal(a.name, name) {
			return true
		}
	}
	return false
}

// cells is ([[cells]] tables, {agent configuration name: table}, [sign-in
// profiles to create]). known is the file's own model and raw the parsed
// file: a worker the edit left unchanged keeps its [[cells]] table exactly as
// written (never validated or normalized again), with the trial-level
// environment/agent_configuration defaults made explicit because a cell
// rewrite removes them.
func (r *renderer) cells(m *Dict, configs, profiles *Dict, adapters any, known, raw *Dict) ([]any, *Dict, []addedAuth) {
	oldWorkers := NewDict()
	if known != nil {
		for _, w := range orList(get(known, "workers")) {
			if id, ok := hashKey(item(w, "id")); ok {
				oldWorkers.Set(id, w)
			}
		}
	}
	rawCells := NewDict()
	for _, c := range orList(raw.Get("cells")) {
		rawCells.Set(pyStr(or(get(c, "id"), "")), c)
	}
	defaults := NewDict()
	t := items(orDict(raw.Get("trial")))
	for _, k := range t.keys {
		if (k == "environment" || k == "agent_configuration") && truthy(t.vals[k]) {
			defaults.Set(k, t.vals[k])
		}
	}
	cells, wanted, added := []any{}, NewDict(), []addedAuth{}
	for _, w := range iter(item(m, "workers")) {
		id := item(w, "id")
		var old any
		if s, ok := hashKey(id); ok {
			old = oldWorkers.Get(s)
		}
		unchanged := known != nil && Equal(old, w)
		if s, ok := id.(string); unchanged && ok && rawCells.Has(s) {
			own := rawCells.Get(s).(*Dict)
			cell := NewDict()
			for _, k := range defaults.keys {
				if !own.Has(k) {
					cell.Set(k, defaults.vals[k])
				}
			}
			cell.Update(own)
			if name := cell.Get("agent_configuration"); truthy(name) && has(configs, name) {
				n, _ := hashKey(name)
				wanted.Set(n, configs.Get(n))
			}
			cells = append(cells, cell)
			continue
		}
		r.with(r.lenient || unchanged, func() { cells = r.cell(w, m, configs, profiles, cells, wanted, &added) })
	}
	rep := item(m, "reporter")
	rcli := or(get(rep, "cli"), nil)
	if !truthy(rcli) {
		rcli = AutoReporter(iter(item(m, "workers")), adapters)
	}
	if template(rcli, or(get(rep, "auth"), "")) != nil && !has(profiles, item(rep, "auth")) && !addedHas(added, item(rep, "auth")) {
		added = append(added, addedAuth{pyStr(item(rep, "auth")), pyStr(rcli)})
	}
	return cells, wanted, added
}

func (r *renderer) cell(w any, m *Dict, configs, profiles *Dict, cells []any, wanted *Dict, added *[]addedAuth) []any {
	envs := NewDict()
	for _, e := range iter(item(m, "envs")) {
		if id, ok := hashKey(item(e, "id")); ok {
			envs.Set(id, e)
		}
	}
	users := map[string][]any{}
	for _, x := range iter(item(m, "workers")) {
		if p := get(x, "profile"); truthy(p) {
			k, _ := hashKey(p)
			users[k] = append(users[k], item(x, "id"))
		}
	}
	wenv := item(w, "env")
	env := getAny(envs, wenv)
	if env == nil {
		if !r.lenient {
			fail("worker %s runs in an unknown environment %s", pyStr(item(w, "id")), repr(wenv))
		}
		kind := "container"
		if Equal(wenv, FolderEnv) {
			kind = "folder"
		}
		env = NewDict("kind", kind)
	}
	explicit := !Equal(item(env, "kind"), "folder")
	models := []any{}
	for _, x := range orList(get(w, "models")) {
		if pyfmt.Strip(pyStr(x)) != "" {
			models = append(models, r.modelName(x, pyStr(item(w, "cli"))))
		}
	}
	cell := NewDict("id", item(w, "id"), "harness", item(w, "cli"))
	if len(models) > 1 || (len(models) > 0 && truthy(get(w, "models_form"))) {
		cell.Set("models", models) // the cell ids stay <id>-<model>, as recorded attempts know them
	} else if len(models) > 0 {
		cell.Set("model", models[0])
	} else {
		cell.Set("model", r.modelName(get(w, "model"), pyStr(item(w, "cli"))))
	}
	config := any("clean")
	if !explicit {
		config = or(get(w, "config"), "clean")
	}
	cell.Set("effort", noneIfFalsy(get(w, "effort")))
	cell.Set("config", config)
	cell.Set("auth", noneIfFalsy(get(w, "auth")))
	if explicit {
		cell.Set("environment", wenv)
	}
	wants := Equal(get(w, "skills"), "folders")
	paths := []any{}
	for _, y := range orList(get(w, "skillPaths")) {
		if x := pyfmt.Strip(pyStr(y)); x != "" {
			paths = append(paths, x)
		}
	}
	if wants && !explicit && !r.lenient {
		fail("worker %s: giving skills needs a fresh-profile or container environment", pyStr(item(w, "id")))
	}
	if wants && len(paths) == 0 && !r.lenient {
		fail("worker %s: choose at least one skill folder, or select none", pyStr(item(w, "id")))
	}
	profile := or(get(w, "profile"), "")
	own := getAny(configs, profile)
	var name string
	var table any
	wantPaths := []any{}
	if wants {
		wantPaths = paths
	}
	if own != nil {
		if s, p := skillsOf(own); s == wants && Equal(p, wantPaths) {
			name, table = pyStr(item(w, "profile")), own // unchanged: the configuration stays exactly as written
		}
	}
	if table == nil && (wants || own != nil) {
		var base *Dict
		if own != nil {
			base = dictOf(own)
		} else {
			base = NewDict("plugins", NewDict("mode", "none"), "hooks", NewDict("mode", "none"))
		}
		t := base.Copy()
		if wants {
			t.Set("skills", NewDict("mode", "selected", "paths", paths))
		} else {
			t.Set("skills", NewDict("mode", "none"))
		}
		table = t
		pk, _ := hashKey(profile)
		shared := own != nil && len(users[pk]) > 1
		same := ""
		for _, k := range configs.keys { // reuse, so recorded profiles stay
			if Equal(configs.vals[k], t) {
				same = k
				break
			}
		}
		switch {
		case own != nil && !shared:
			name = pyStr(item(w, "profile"))
		case same != "":
			name = same
		default:
			name = configName(pyStr(item(w, "id")), t, configs, wanted)
		}
	}
	if name != "" {
		if wanted.Has(name) && !Equal(wanted.Get(name), table) {
			name = configName(pyStr(item(w, "id")), table, configs, wanted)
		}
		wanted.Set(name, table)
		cell.Set("agent_configuration", name)
	}
	extra := items(orDict(get(w, "extra")))
	for _, k := range extra.keys {
		if v := extra.vals[k]; v != nil && !cellKeys[k] {
			cell.Set(k, v)
		}
	}
	cells = append(cells, clean(cell))
	if template(item(w, "cli"), or(get(w, "auth"), "")) != nil && !has(profiles, item(w, "auth")) &&
		!addedHas(*added, item(w, "auth")) {
		*added = append(*added, addedAuth{pyStr(item(w, "auth")), pyStr(item(w, "cli"))})
	}
	return cells
}

func checkNames(m, current *Dict) {
	var ids []any
	for _, w := range iter(item(m, "workers")) {
		ids = append(ids, item(w, "id"))
	}
	if len(ids) == 0 {
		fail("add at least one worker")
	}
	old := []any{}
	for _, w := range iter(item(current, "workers")) {
		old = append(old, item(w, "id"))
	}
	if !unique(ids) {
		fail("worker names must be unique")
	}
	for _, i := range ids {
		if !has(old, i) && !slugMatch(i) {
			fail("worker name %s: use a short lowercase name (letters, digits, . _ -)", repr(i))
		}
	}
	var envIDs []any
	for _, e := range iter(item(m, "envs")) {
		envIDs = append(envIDs, item(e, "id"))
	}
	oldEnvs := []any{}
	for _, e := range iter(item(current, "envs")) {
		oldEnvs = append(oldEnvs, item(e, "id"))
	}
	if !unique(envIDs) {
		fail("environment names must be unique")
	}
	folders := 0
	for _, e := range iter(item(m, "envs")) {
		folder := Equal(item(e, "kind"), "folder")
		if !folder && !has(oldEnvs, item(e, "id")) && !slugMatch(item(e, "id")) {
			fail("environment name %s: use a short lowercase name (letters, digits, . _ -)", repr(item(e, "id")))
		}
		if !folder && Equal(item(e, "id"), FolderEnv) {
			fail("name the environment before changing what kind it is")
		}
		if folder {
			folders++
		}
	}
	if folders > 1 {
		fail("only one 'fresh folder on your machine' environment is possible")
	}
}

// slugMatch is SLUG.fullmatch(i or "").
func slugMatch(v any) bool {
	v = or(v, "")
	s, ok := v.(string)
	if !ok {
		raise("TypeError", "expected string or bytes-like object, got '%s'", typeName(v))
	}
	return slug.MatchString(s)
}

// unique is len(set(items)) == len(items).
func unique(items []any) bool {
	for i := range items {
		hashKey(items[i])
		for j := 0; j < i; j++ {
			if Equal(items[i], items[j]) {
				return false
			}
		}
	}
	return true
}

func groupsEqual(a, b []any) bool { return Equal(a, b) }

func (r *renderer) render(text string, model, current *Dict) *Rendered {
	raw, err := ParseTOML(text)
	if err != nil {
		raise("TOMLDecodeError", "%s", err.Error())
	}
	checkNames(model, current)
	for _, s := range scalars {
		same := true
		for _, src := range s.sources {
			if !Equal(get(item(current, src[0]), src[1]), get(item(model, src[0]), src[1])) {
				same = false
			}
		}
		if same {
			continue
		}
		target := s.compute(r, model)
		key := [2]string{s.table, s.name}
		tbl := orDict(raw.Get(s.table))
		now := getDef(tbl, s.name, defaults[key])
		if s.table == "task" && s.name == "identity_cmd" {
			now = noneIfFalsy(identity(now))
		}
		if Equal(target, now) || (Equal(target, defaults[key]) && !has(tbl, s.name)) {
			continue
		}
		text = setValue(text, s.table, s.name, target, 0)
	}
	var nowGroups map[string][]any
	var nowEnvs *Dict
	r.with(true, func() { nowGroups, nowEnvs = r.groups(current, nil), r.envTables(current, nil) })
	newGroups, newEnvs := r.groups(model, current), r.envTables(model, current)
	for _, name := range groupNames {
		entries := newGroups[name]
		if !groupsEqual(entries, nowGroups[name]) {
			var lines []string
			for _, e := range entries {
				lines = append(lines, block(name, e.(*Dict), true)...)
			}
			text = ReplaceSections(text, group(name), lines)
		}
	}
	if !Equal(nowEnvs, newEnvs) {
		var lines []string
		for _, name := range newEnvs.keys {
			lines = append(lines, block("environments."+name, newEnvs.vals[name].(*Dict), false)...)
		}
		text = ReplaceSections(text, func(n string, _ bool) bool {
			return n == "environments" || strings.HasPrefix(n, "environments.")
		}, lines)
	}
	configs := NewDict()
	ac := items(orDict(raw.Get("agent_configurations")))
	for _, k := range ac.keys {
		configs.Set(k, dictOf(ac.vals[k]))
	}
	profiles := items(orDict(raw.Get("auth")))
	adapters := orDict(raw.Get("adapters"))
	var nowCells []any
	var nowWanted *Dict
	r.with(true, func() { nowCells, nowWanted, _ = r.cells(current, configs, profiles, adapters, current, raw) })
	cells, wanted, added := r.cells(model, configs, profiles, adapters, current, raw)
	t := orDict(raw.Get("trial"))
	if !Equal(cells, nowCells) {
		var lines []string
		for _, c := range cells {
			lines = append(lines, block("cells", c.(*Dict), true)...)
		}
		text = ReplaceSections(text, group("cells"), lines)
		for _, name := range []string{"environment", "agent_configuration"} {
			if get(t, name) != nil {
				text = setValue(text, "trial", name, nil, 0)
			}
		}
	}
	if !Equal(wanted, nowWanted) {
		// Existing configurations stay even when no worker uses them any more: an output folder's
		// attempts recorded them, and dropping one would lock that folder. Only new or edited ones change.
		keep := configs.Copy()
		keep.Update(wanted)
		if !Equal(keep, configs) {
			var lines []string
			for _, name := range keep.keys {
				lines = append(lines, block("agent_configurations."+name, asDict(keep.vals[name]), false)...)
			}
			text = ReplaceSections(text, func(n string, _ bool) bool {
				return n == "agent_configurations" || strings.HasPrefix(n, "agent_configurations.")
			}, lines)
		}
	}
	for _, a := range added {
		text = strings.TrimRight(text, "\n") + "\n\n" + strings.Join(block("auth."+a.name, template(a.harness, a.name).table(), false), "\n")
	}
	parsed, err := ParseTOML(text)
	if err != nil {
		fail("the edit produced invalid TOML (%s); edit trial.toml directly", err.Error())
	}
	for _, name := range groupNames { // what was written reads back as intended
		entries := newGroups[name]
		if !groupsEqual(entries, nowGroups[name]) {
			back := []any{}
			for _, x := range orList(parsed.Get(name)) {
				back = append(back, dictOf(x))
			}
			if !Equal(back, entries) {
				fail("could not write [[%s]] safely; edit trial.toml directly", name)
			}
		}
	}
	out := &Rendered{Text: text}
	if p := get(item(model, "task"), "prompt"); p != nil {
		s := strings.ReplaceAll(pyStr(p), "\r\n", "\n")
		out.Prompt = &s
	}
	for _, a := range added {
		out.AddedAuth = append(out.AddedAuth, a.name)
	}
	if out.AddedAuth == nil {
		out.AddedAuth = []string{}
	}
	return out
}
