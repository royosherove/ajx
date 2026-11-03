// Package reportui ports lib/ajx/report_ui.py: offline HTML views of the same
// asks, narrative and recorded event evidence.
//
// Values are JSON-shaped (util.M, []any, ...). Python's str() of a value is
// pyfmt.Str, so a float stays 2.0 and a list keeps its Python spelling.
// Dictionaries iterate in sorted key order (Go maps keep no insertion order).
package reportui

import (
	"fmt"
	"regexp"
	"strings"

	ajx "github.com/royosherove/ajx"
	"github.com/royosherove/ajx/internal/mdlite"
	"github.com/royosherove/ajx/internal/measure"
	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/res"
	"github.com/royosherove/ajx/internal/util"
)

type M = util.M

// LabelNames is the vocabulary order (references/vocabulary.md).
var LabelNames = []string{"Wall", "Roadblock", "Detour", "Wait", "Dead End", "Fork", "Wrong Turn",
	"Misdirection", "Blind Spot", "Gate", "Trust Decision", "Cost", "Delight"}

// Labels maps a label to [icon, tone]. Labels are observations, not severity scores.
var Labels = map[string][2]string{
	"Wall": {"🧱", "bad"}, "Roadblock": {"🚧", "bad"}, "Detour": {"↩️", "warn"},
	"Wait": {"⏳", "warn"}, "Dead End": {"🚫", "bad"}, "Fork": {"🔀", "warn"},
	"Wrong Turn": {"❓", "warn"}, "Misdirection": {"🔴", "bad"}, "Blind Spot": {"👁️", "warn"},
	"Gate": {"🚪", "warn"}, "Trust Decision": {"🧭", "info"}, "Cost": {"💸", "info"},
	"Delight": {"✅", "good"},
}

// StatusNames is the Python STATUSES order.
var StatusNames = []string{"verified_defect", "observed_friction", "untested_risk", "feature_request",
	"succeeded", "partial", "failed", "not_run"}

// Statuses maps an evidence or verification status to [text, tone].
var Statuses = map[string][2]string{
	"verified_defect": {"Verified defect", "bad"}, "observed_friction": {"Observed friction", "warn"},
	"untested_risk": {"Untested risk", "info"}, "feature_request": {"Feature request", "info"},
	"succeeded": {"Verified success", "good"}, "partial": {"Partially verified", "warn"},
	"failed": {"Verification failed", "bad"}, "not_run": {"Not verified", "warn"},
}

// ExtensionHeadings are the extension-state table headings.
var ExtensionHeadings = []string{"Category / entry", "Version / content hash", "Installed", "Discoverable", "Enabled",
	"Loaded into context", "Invoked / executed"}

// ConsistencyKeys is the order render.matrix_report builds its consistency record in.
var ConsistencyKeys = []string{"prompt_sha256", "product_versions", "harness_versions", "environment_profiles",
	"environment_backends", "agent_configuration_profiles", "wall_clock_comparable", "configuration_note",
	"token_note", "cost_note"}

var eventID = regexp.MustCompile(`^E-` + pyfmt.Digit + `{3,}$`)

// IsEventID is EVENT_ID.fullmatch for a string value.
func IsEventID(v any) bool {
	s, ok := v.(string)
	return ok && eventID.MatchString(s)
}

// ---------------------------------------------------------------- primitives

// Esc is html.escape(str(value if value is not None else ""), quote=True).
func Esc(v any) string {
	if v == nil {
		return ""
	}
	return pyfmt.Escape(pyfmt.Str(v), true)
}

// Number formats a count or measurement; None is "Unavailable".
func Number(v any) string {
	switch t := v.(type) {
	case nil:
		return "Unavailable"
	case bool:
		if t {
			return "1"
		}
		return "0"
	case float64:
		return pyfmt.FormatG(t, 6, true)
	case float32:
		return pyfmt.FormatG(float64(t), 6, true)
	}
	if pyfmt.IsInt(v) {
		return pyfmt.Commas(pyfmt.Str(v))
	}
	return pyfmt.Str(v)
}

// Duration is "1m 05s" from 60 seconds up, else "<g>s"; None is "Unavailable".
func Duration(v any) string {
	if v == nil {
		return "Unavailable"
	}
	f, ok := pyfmt.Number(v)
	if !ok {
		return pyfmt.Str(v)
	}
	if f >= 60 {
		return fmt.Sprintf("%sm %02ds", pyfmt.IntStr(pyfmt.FloorDiv(f, 60)), int64(pyfmt.Mod(f, 60)))
	}
	return pyfmt.FormatG(f, 6, false) + "s"
}

// Badge is a status pill.
func Badge(text, tone string) string {
	return `<span class="badge ` + Esc(tone) + `">` + Esc(text) + `</span>`
}

// Status is the badge for an evidence or verification status.
func Status(v any) string {
	if s, ok := v.(string); ok {
		if st, known := Statuses[s]; known {
			return Badge(st[0], st[1])
		}
	}
	if !util.Truthy(v) {
		return Badge("Unknown", "")
	}
	return Badge(pyfmt.Str(v), "")
}

// LabelBadges is report_ui.labels(): a badge per known label.
func LabelBadges(names []any) string {
	var b strings.Builder
	for _, n := range names {
		name, ok := n.(string)
		if !ok {
			continue
		}
		if l, known := Labels[name]; known {
			b.WriteString(Badge(l[0]+" "+name, l[1]))
		}
	}
	return b.String()
}

func uniqueStrings(refs []any) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range refs {
		s, ok := r.(string)
		if ok && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// EventLinks links valid event ids (E-001...) to the journey page.
func EventLinks(refs any, prefix string) string {
	var parts []string
	for _, ref := range uniqueStrings(util.List(refs)) {
		if eventID.MatchString(ref) {
			parts = append(parts, `<a class="mono" href="`+Esc(prefix)+`#`+ref+`">`+ref+`</a>`)
		}
	}
	return strings.Join(parts, " ")
}

// Page wraps a body in the report shell (offline CSS/JS from the skill assets).
func Page(title, body, subtitle, view string, example bool) string {
	css := res.MustText("assets/report.css")
	js := res.MustText("assets/report.js")
	type link struct{ href, label, key string }
	nav := []link{{"report.html", "Product asks", "report"}, {"pretty-journey.html", "Agent journey", "journey"},
		{"../../index.html", "Compare runs", "matrix"}, {"asks.md", "Markdown asks", "markdown"},
		{"run.json", "Run data", "data"}}
	brand := "../../index.html"
	if view == "matrix" {
		nav = []link{{"index.html", "Compare runs", "matrix"}, {"matrix.md", "Markdown", "markdown"},
			{"matrix.json", "Data", "data"}}
		brand = "index.html"
	}
	var navHTML strings.Builder
	for _, l := range nav {
		navHTML.WriteString(`<a href="` + l.href + `"`)
		if l.key == view {
			navHTML.WriteString(` aria-current="page"`)
		}
		navHTML.WriteString(`>` + l.label + `</a>`)
	}
	demo := ""
	generated := "Generated " + util.Now()
	if example {
		demo = `<div class="example"><strong>Fictional example.</strong> All events, versions, costs, and ` +
			`outcomes below are synthetic. This is a report demonstration, not a product or agent benchmark.</div>`
		generated = "Synthetic example"
	}
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'">
<title>` + Esc(title) + `</title><style>` + css + `</style></head><body>
<a class="skip" href="#content">Skip to report</a>
<aside class="rail"><a class="brand" href="` + brand + `">AJX<small>Agent Journey Experience</small></a>
<nav aria-label="Report views">` + navHTML.String() + `</nav><p class="rail-note">Follow an ask to its evidence.<br>Keep measured cost and modeled savings separate.</p></aside>
<main id="content"><header class="page-head"><h1>` + Esc(title) + `</h1><p class="subtitle">` + Esc(subtitle) + `</p></header>
` + demo + body + `
<footer class="page-footer">AJX ` + ajx.Version + `. ` + Esc(generated) + `. Recorded costs describe the observed run.
Provider prices are estimates; modeled savings are hypotheses. Missing measurements remain unavailable.
This report opens offline and makes no network requests.</footer></main><script>` + js + `</script></body></html>`
}

// ---------------------------------------------------------------- values

func rec(v any) M {
	if m, ok := v.(M); ok && m != nil {
		return m
	}
	return M{}
}

// getd is dict.get(key, default): the default only when the key is absent.
func getd(m M, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Value is report_ui._value: "unknown" for None/"", sorted JSON for
// containers and booleans, str() otherwise.
func Value(v any) string {
	if v == nil {
		return "unknown"
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return "unknown"
		}
		return s
	}
	switch v.(type) {
	case bool, M, []any, map[string]string, []string, []M:
		return pyfmt.Dumps(v, pyfmt.DumpOpts{})
	}
	return pyfmt.Str(v)
}

func profileName(r M) any { return util.Or(r["profile_id"], r["id"], r["name"]) }

func profileLabel(r M) string {
	actual, declared := profileName(r), r["declared_profile_id"]
	if util.Truthy(actual) {
		if util.Truthy(declared) && !pyfmt.Equal(actual, declared) {
			return pyfmt.Str(actual) + " (declared: " + pyfmt.Str(declared) + ")"
		}
		return pyfmt.Str(actual)
	}
	declared = util.Or(declared, profileName(rec(r["plan"])))
	if util.Truthy(declared) {
		return pyfmt.Str(declared) + " (declared only)"
	}
	return "unknown"
}

func isContainer(v any) bool {
	switch v.(type) {
	case M, []any, []string, []M:
		return true
	}
	return false
}

// ExtensionEntries accepts named selections and resolved entries without inferring their states.
func ExtensionEntries(configuration M, kind string) []M {
	section := configuration[kind]
	var entries any = []any{}
	if s, ok := section.(M); ok {
		for _, key := range []string{"entries", "items", "selected", "enabled"} {
			if isContainer(s[key]) {
				entries = s[key]
				break
			}
		}
	} else if l := util.List(section); l != nil {
		entries = l
	}
	var out []M
	if d, ok := entries.(M); ok {
		for _, name := range pyfmt.Keys(d) {
			entry := util.Copy(rec(d[name]))
			entry["name"] = name
			out = append(out, entry)
		}
		return out
	}
	for _, e := range util.List(entries) {
		if m, ok := e.(M); ok {
			out = append(out, m)
		} else {
			out = append(out, M{"name": pyfmt.Str(e)})
		}
	}
	return out
}

// ExtensionSummary is "<mode>; <names>" for skills, plugins or hooks.
func ExtensionSummary(configuration M, kind string) string {
	section := rec(configuration[kind])
	mode := getd(section, "mode", configuration[kind+"_mode"])
	var entries []any
	selected := false
	for _, key := range []string{"selected", "enabled"} {
		if l, ok := section[key].([]any); ok {
			entries, selected = l, true
			break
		}
	}
	if !selected {
		for _, e := range ExtensionEntries(configuration, kind) {
			entries = append(entries, e)
		}
	}
	var names []string
	for _, e := range entries {
		if m, ok := e.(M); ok {
			names = append(names, Value(util.Or(m["name"], m["id"])))
		} else {
			names = append(names, pyfmt.Str(e))
		}
	}
	out := Value(mode)
	if len(names) > 0 {
		return out + "; " + strings.Join(names, ", ")
	}
	if pyfmt.Equal(mode, "selected") || pyfmt.Equal(mode, "snapshot") {
		return out + "; selection unknown"
	}
	return out
}

// CleanupStatus is the recorded environment cleanup status ("unknown" when absent).
func CleanupStatus(cleanup any) string {
	if m, ok := cleanup.(M); ok {
		return Value(m["status"])
	}
	return Value(cleanup)
}

// CleanupProblem reports whether environment cleanup is not confirmed.
func CleanupProblem(cleanup any) bool {
	r := rec(cleanup)
	switch strings.ToLower(CleanupStatus(cleanup)) {
	case "failed", "error", "auth_errors", "skipped", "not_run", "not run", "timeout", "timed_out", "partial":
		return true
	}
	if r["ok"] == false || r["confirmed"] == false || util.Truthy(r["errors"]) {
		return true
	}
	failed := r["failed"]
	if b, ok := failed.(bool); ok {
		return b
	}
	f, ok := util.Num(failed)
	return ok && f > 0
}

// ConfigurationValues are the (label, value) pairs describing a run's
// environment and agent configuration.
func ConfigurationValues(run M) [][2]string {
	environment := rec(run["execution_environment"])
	configuration := rec(run["agent_configuration"])
	backend := ""
	if b := environment["backend"]; b != nil {
		backend = Value(b) + " (reported)"
	} else if pb := rec(environment["plan"])["backend"]; pb != nil {
		backend = Value(pb) + " (declared only)"
	} else if t := rec(run["runner"])["type"]; util.Truthy(t) {
		backend = pyfmt.Str(t) + " (legacy runner; capabilities unknown)"
	}
	if backend == "" {
		backend = "unknown"
	}
	return [][2]string{
		{"Environment profile", profileLabel(environment)},
		{"Backend", backend},
		{"Agent profile", profileLabel(configuration)},
		{"Skills mode / selection (declared)", ExtensionSummary(configuration, "skills")},
		{"Plugins mode / selection (declared)", ExtensionSummary(configuration, "plugins")},
		{"Hooks mode / selection (declared)", ExtensionSummary(configuration, "hooks")},
		{"Environment cleanup", CleanupStatus(run["environment_cleanup"])},
	}
}

// ConfigurationValue returns the value of ConfigurationValues(run)[i].
func ConfigurationValue(run M, i int) string { return ConfigurationValues(run)[i][1] }

// ProfileLimitations are the limitations recorded by the environment, its
// plan, the agent configuration, its extensions and the environment cleanup.
func ProfileLimitations(run M) []string {
	environment := rec(run["execution_environment"])
	configuration := rec(run["agent_configuration"])
	type source struct {
		label  string
		record M
	}
	sources := []source{{"execution environment", environment}, {"environment plan", rec(environment["plan"])},
		{"agent configuration", configuration}, {"environment cleanup", rec(run["environment_cleanup"])}}
	for _, kind := range []string{"skills", "plugins", "hooks"} {
		sources = append(sources, source{kind, rec(configuration[kind])})
	}
	var notes []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			notes = append(notes, n)
		}
	}
	for _, s := range sources {
		lim := s.record["limitations"]
		var items []any
		if l, ok := lim.([]any); ok {
			items = l
		} else if util.Truthy(lim) {
			items = []any{lim}
		}
		for _, note := range items {
			add(s.label + ": " + Value(note))
		}
	}
	if len(environment) > 0 && !util.Truthy(environment["capabilities"]) {
		add("execution environment: capability coverage unknown")
	}
	return notes
}

var profileArtifactLinks = [][3]string{{"environment", "environment.json", "Environment evidence"},
	{"agent_configuration", "agent-configuration.json", "Agent configuration manifest"}}

// ProfileLinks links the run's known local profile artifacts (never manifest strings).
func ProfileLinks(run M, prefix string) string {
	artifacts := rec(run["artifacts"])
	var parts []string
	for _, a := range profileArtifactLinks {
		if pyfmt.Equal(artifacts[a[0]], a[1]) {
			parts = append(parts, `<a href="`+Esc(prefix+a[1])+`">`+a[2]+`</a>`)
		}
	}
	return strings.Join(parts, " ")
}

// ConfigurationOverview is the one-line configuration summary.
func ConfigurationOverview(run M) string {
	values := ConfigurationValues(run)
	parts := make([]string, 0, 4)
	for _, v := range values[:4] {
		parts = append(parts, v[0]+": "+v[1])
	}
	return `<p class="small">` + Esc(strings.Join(parts, " · ")) + `. <a href="#configuration">Configuration and coverage</a></p>`
}

// CapabilityRows are (name, declared, enforcement, observed, detail) rows.
func CapabilityRows(record M, scope string) [][5]string {
	type item struct {
		name  string
		value any
	}
	var items []item
	switch c := record["capabilities"].(type) {
	case M:
		for _, k := range pyfmt.Keys(c) {
			items = append(items, item{k, c[k]})
		}
	case []any:
		for _, e := range c {
			if m, ok := e.(M); ok {
				items = append(items, item{pyfmt.Str(util.Or(m["name"], m["id"], "unnamed")), m})
			} else {
				items = append(items, item{pyfmt.Str(e), nil})
			}
		}
	default:
		return nil
	}
	var rows [][5]string
	for _, it := range items {
		entry := rec(it.value)
		enforcement := getd(entry, "enforcement", getd(entry, "state", entry["status"]))
		if s, ok := it.value.(string); ok && len(entry) == 0 {
			enforcement = s
		}
		e, _ := enforcement.(string)
		switch e {
		case "enforced", "observed-only", "observed_only", "unsupported", "unknown", "declared":
		default:
			e = "unknown"
		}
		rows = append(rows, [5]string{scope + ": " + it.name, Value(entry["declared"]),
			strings.ReplaceAll(e, "_", "-"), Value(getd(entry, "observed", entry["observations"])), Value(it.value)})
	}
	return rows
}

// ExtensionStateRows are the per-entry extension states (missing states stay unknown).
func ExtensionStateRows(configuration M) [][]string {
	var rows [][]string
	for _, kind := range []string{"skills", "plugins", "hooks", "uncontrolled_sources"} {
		for _, entry := range ExtensionEntries(configuration, kind) {
			states := rec(entry["states"])
			stateValue := func(keys ...string) string {
				for _, key := range keys {
					if v, ok := states[key]; ok {
						return Value(v)
					}
					if v, ok := entry[key]; ok {
						return Value(v)
					}
				}
				return "unknown"
			}
			identity := Value(util.Or(entry["name"], entry["id"]))
			revision := util.Or(entry["content_hash"], entry["content_sha256"], entry["sha256"], entry["hash"])
			rows = append(rows, []string{strings.ReplaceAll(kind, "_", " ") + ": " + identity,
				Value(entry["version"]) + " / " + Value(revision),
				stateValue("installed"), stateValue("discoverable"), stateValue("enabled"),
				stateValue("loaded_into_context", "loaded"), stateValue("invoked", "executed")})
		}
	}
	return rows
}

// ProfileCoverage is the one-line capability coverage description.
func ProfileCoverage(run M) string {
	rows := CapabilityRows(rec(run["execution_environment"]), "Environment")
	rows = append(rows, CapabilityRows(rec(run["agent_configuration"]), "Agent adapter")...)
	var out []string
	for _, r := range rows {
		name, declared, enforcement, observed, detail := r[0], r[1], r[2], r[3], r[4]
		d := name + ": declared: " + declared + "; enforcement: " + enforcement + "; observed: " + observed
		if declared == "unknown" && enforcement == "unknown" && observed == "unknown" && detail != "unknown" {
			d = name + ": reported: " + detail + "; enforcement: unknown; observed: unknown"
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return "Capability coverage unknown"
	}
	return strings.Join(out, "; ")
}

func tableHTML(headings []string, rows [][]string) string {
	var b strings.Builder
	b.WriteString(`<div class="table-wrap"><table><thead><tr>`)
	for _, h := range headings {
		b.WriteString(`<th scope="col">` + h + `</th>`)
	}
	b.WriteString("</tr></thead><tbody>")
	for _, row := range rows {
		b.WriteString("<tr>")
		for _, v := range row {
			b.WriteString("<td>" + Esc(v) + "</td>")
		}
		b.WriteString("</tr>")
	}
	b.WriteString("</tbody></table></div>")
	return b.String()
}

// ConfigurationSection is the "Execution environment and agent configuration" section.
func ConfigurationSection(run M) string {
	environment := rec(run["execution_environment"])
	configuration := rec(run["agent_configuration"])
	var b strings.Builder
	b.WriteString(`<section class="section" id="configuration"><h2>Execution environment and agent configuration</h2>`)
	b.WriteString(`<div class="panel"><dl class="detail-grid">`)
	for _, v := range ConfigurationValues(run) {
		b.WriteString(`<div><dt>` + Esc(v[0]) + `</dt><dd>` + Esc(v[1]) + `</dd></div>`)
	}
	b.WriteString("</dl>")
	b.WriteString(`<p class="small">Declared settings express intent. Enforced describes a reported control; observed ` +
		`describes recorded checks or telemetry. Missing evidence remains unknown. ` +
		`A private HOME or isolated harness configuration is not a filesystem sandbox.</p>`)
	b.WriteString(`<div class="links">` + ProfileLinks(run, "") + `<a href="run.json">Complete run evidence</a></div></div>`)
	capabilities := CapabilityRows(environment, "Environment")
	capabilities = append(capabilities, CapabilityRows(configuration, "Agent adapter")...)
	b.WriteString(`<h3>Capability coverage</h3>`)
	if len(capabilities) > 0 {
		rows := make([][]string, len(capabilities))
		for i, r := range capabilities {
			rows[i] = r[:]
		}
		b.WriteString(tableHTML([]string{"Capability", "Declared", "Enforcement", "Observed", "Recorded detail / scope"}, rows))
	} else {
		b.WriteString(`<p class="muted">Capability coverage unknown; no capability report was recorded.</p>`)
	}
	b.WriteString(`<h3>Extension states</h3><p class="small">A selection or enabled state does not prove use. ` +
		`Installed, discoverable, and enabled are recorded configuration states. Loaded into context and ` +
		`invoked / executed require their own telemetry; missing states remain unknown, not false.</p>`)
	if states := ExtensionStateRows(configuration); len(states) > 0 {
		b.WriteString(tableHTML(ExtensionHeadings, states))
	} else {
		b.WriteString(`<p class="muted">No extension entries recorded. Loaded / invoked telemetry is unknown.</p>`)
	}
	if notes := ProfileLimitations(run); len(notes) > 0 {
		b.WriteString(`<h3>Profile limitations</h3><ul>`)
		for _, n := range notes {
			b.WriteString("<li>" + Esc(n) + "</li>")
		}
		b.WriteString("</ul>")
	}
	cleanup := run["environment_cleanup"]
	tone := ""
	if CleanupProblem(cleanup) {
		tone = "bad"
	}
	b.WriteString(`<h3>Environment cleanup</h3>` + Badge(CleanupStatus(cleanup), tone))
	if cleanup != nil {
		b.WriteString(`<pre>` + Esc(pyfmt.Dumps(cleanup, pyfmt.DumpOpts{Indent: 2, Ordered: true})) + `</pre>`)
	}
	b.WriteString(`<p class="small">Environment release is separate from task teardown and task verification.</p></section>`)
	return b.String()
}

// EnvironmentCleanupNotice warns when environment cleanup is not confirmed.
func EnvironmentCleanupNotice(run M, prefix string) string {
	if !CleanupProblem(run["environment_cleanup"]) {
		return ""
	}
	return `<div class="notice bad"><strong>Environment cleanup not confirmed.</strong> ` +
		Esc(CleanupStatus(run["environment_cleanup"])) + `. Owned resources may still exist. ` +
		`<a href="` + Esc(prefix) + `run.json">Inspect environment cleanup</a>.</div>`
}

// ReviewNotice warns about an incomplete review, failed extraction or unconfirmed cleanup.
func ReviewNotice(run, data M) string {
	var b strings.Builder
	if pyfmt.Equal(run["review_status"], "incomplete") {
		b.WriteString(`<div class="notice bad"><strong>Review incomplete.</strong> The journey or extraction is missing. ` +
			`The number of asks cannot establish a clean result.</div>`)
	}
	if !pyfmt.Equal(data["extraction_status"], "ok") {
		b.WriteString(`<div class="notice bad"><strong>Extraction did not complete.</strong> ` +
			Esc(util.Or(data["extraction_status"], "No extraction recorded")) +
			`. Regenerate extraction before using this register to prioritize fixes.</div>`)
	}
	switch util.S(rec(util.Or(run["teardown"], M{}))["status"]) {
	case "failed", "auth_errors", "skipped":
		b.WriteString(`<div class="notice bad"><strong>Cleanup not confirmed.</strong> Resources may still exist. ` +
			`Inspect this run’s <a href="teardown.json">teardown record</a>.</div>`)
	}
	return b.String() + EnvironmentCleanupNotice(run, "")
}

// EmptyRegister explains an empty ask register.
func EmptyRegister(data M) string {
	text := "No asks returned, but no-obstacles-observed was not confirmed. Review the extraction."
	if !pyfmt.Equal(data["extraction_status"], "ok") {
		text = "No usable ask register. Extraction is incomplete; this is not a clean result."
	} else if util.Truthy(data["no_obstacles_observed"]) {
		text = "No obstacles observed in this task. The evidence supports an empty register for this run."
	}
	return `<div class="empty">` + Esc(text) + `</div>`
}

// SummaryStrip is the four headline facts.
func SummaryStrip(run, summary M) string {
	product := rec(run["product"])
	type item struct {
		k, v string
		note any
	}
	items := []item{
		{"Task outcome", Status(run["task_outcome_verified"]), util.Or(run["verification_scope"], "No verification recorded")},
		{"Product version", Esc(util.Or(product["version"], "Unknown")), product["name"]},
		{"Task wall clock", Esc(Duration(summary["elapsed_seconds"])), "Execution window only"},
		{"Output tokens", Esc(Number(summary["output_tokens"])), util.Or(summary["usage_status"], "Coverage unavailable")},
	}
	var b strings.Builder
	b.WriteString(`<dl class="summary-strip">`)
	for _, it := range items {
		b.WriteString(`<div><dt>` + Esc(it.k) + `</dt><dd>` + it.v + `<small>` + Esc(it.note) + `</small></dd></div>`)
	}
	b.WriteString("</dl>")
	return b.String()
}

// ---------------------------------------------------------------- asks

func strsOf(v any) []string {
	var out []string
	for _, x := range util.List(v) {
		out = append(out, pyfmt.Str(x))
	}
	return out
}

// AskCard renders one ask (compact for the matrix columns).
func AskCard(ask M, index int, prefix string, compact bool) string {
	measured := rec(ask["measured"])
	aid := pyfmt.Str(util.Or(ask["id"], fmt.Sprintf("ASK-%03d", index+1)))
	fragment := pyfmt.Quote(aid, "")
	target := "#" + fragment
	if prefix != "" {
		target = prefix + "report.html#" + fragment
	}
	refs := EventLinks(ask["event_refs"], prefix+"pretty-journey.html")
	labels := util.List(ask["labels"])
	tags := `<div class="badges">` + Status(ask["evidence_status"]) + LabelBadges(labels) + `</div>`
	kinds := append([]string{pyfmt.Str(util.Or(ask["evidence_status"], ""))}, strsOf(labels)...)
	attrs := fmt.Sprintf(`data-item data-order="%d" data-kinds="%s" data-span="%s" data-tokens="%s" data-calls="%s" data-failed="%s"`,
		index, Esc(strings.Join(kinds, "|")), Esc(measured["span_seconds"]), Esc(measured["output_tokens"]),
		Esc(measured["tool_calls"]), Esc(measured["failed_tool_calls"]))
	title := `<a href="` + Esc(target) + `">` + Esc(util.Or(ask["title"], "Untitled ask")) + `</a>`
	if compact {
		return `<article class="ask" ` + attrs + `>
<h4 class="ask-title"><span class="mono">` + Esc(aid) + `</span>` + title + `</h4>` + tags + `
<p class="requested">` + Esc(ask["requested_behavior"]) + `</p>
<p class="compact-cost">` + Esc(Duration(measured["span_seconds"])) + ` span · ` + Esc(Number(measured["tool_calls"])) +
			` calls · ` + Esc(Number(measured["output_tokens"])) + ` output tokens</p>
<div class="links">` + refs + `</div></article>`
	}
	facts := [][2]string{{"Linked-event span", Duration(measured["span_seconds"])},
		{"Tool calls / failed", Number(measured["tool_calls"]) + " / " + Number(measured["failed_tool_calls"])},
		{"Output tokens", Number(measured["output_tokens"])}}
	var costs strings.Builder
	costs.WriteString(`<dl class="ask-cost">`)
	for _, f := range facts {
		costs.WriteString(`<div><dt>` + Esc(f[0]) + `</dt><dd>` + Esc(f[1]) + `</dd></div>`)
	}
	costs.WriteString("</dl>")
	model := "Not modeled."
	if saving := ask["modeled_saving"]; util.Truthy(saving) {
		s := rec(saving)
		model = pyfmt.Str(s["claim"]) + "\nComparator: " + pyfmt.Str(s["comparator"]) + "\nAssumption: " +
			pyfmt.Str(s["assumption"]) + "\nUncertainty: " + pyfmt.Str(s["uncertainty"])
	}
	detail := []struct {
		k string
		v any
	}{{"Observation", ask["how_observed"]}, {"Consequence", ask["consequence"]},
		{"Verify the change", ask["verification"]}, {"Workaround", util.Or(ask["workaround"], "None recorded.")},
		{"Measurement basis", util.Or(measured["basis"], "No measurement basis recorded.")},
		{"Modeled saving", model}}
	var details strings.Builder
	details.WriteString(`<dl class="detail-grid">`)
	for _, d := range detail {
		details.WriteString(`<div><dt>` + Esc(d.k) + `</dt><dd>` + Esc(d.v) + `</dd></div>`)
	}
	details.WriteString("</dl>")
	if shared := EventLinks(measured["shared_events"], "pretty-journey.html"); shared != "" {
		details.WriteString(`<p class="notice warn">Costs overlap with another ask at ` + shared +
			`. Do not add them as independent savings.</p>`)
	}
	if util.Truthy(ask["invalid_refs"]) {
		details.WriteString(`<p class="notice warn">Unsupported references removed: ` +
			Esc(strings.Join(strsOf(ask["invalid_refs"]), ", ")) + `.</p>`)
	}
	refsHTML := refs
	if refsHTML == "" {
		refsHTML = "No valid event reference"
	}
	return `<article class="ask" id="` + Esc(aid) + `" ` + attrs + `><div class="ask-main">
<div class="ask-rank">` + fmt.Sprintf("%02d", index+1) + `<small>` + Esc(aid) + `</small></div><div><h3 class="ask-title">` + title + `</h3>` + tags + `
<p class="requested">` + Esc(ask["requested_behavior"]) + `</p>
<p class="rationale">` + Esc(ask["priority_rationale"]) + `</p><div class="links">` + refsHTML + `<a href="#` + fragment + `-detail">Inspect evidence and verification</a></div></div>` + costs.String() + `</div>
<details class="ask-details" id="` + fragment + `-detail"><summary>Observation, verification, and measurement basis</summary>` + details.String() + `</details></article>`
}

// Toolbar is the filter/sort toolbar; kinds and sorts are (value, label) pairs.
func Toolbar(noun string, kinds, sorts [][2]string, compare bool) string {
	var b strings.Builder
	b.WriteString(`<div class="toolbar js-only"><label>Find ` + Esc(noun) + `<input type="search" data-query placeholder="Search this report"></label>`)
	if len(kinds) > 0 {
		b.WriteString(`<label>Show<select data-kind><option value="">All</option>`)
		for _, k := range kinds {
			b.WriteString(`<option value="` + Esc(k[0]) + `">` + Esc(k[1]) + `</option>`)
		}
		b.WriteString("</select></label>")
	}
	if len(sorts) > 0 {
		b.WriteString(`<label>Order<select data-sort><option value="order">Original order</option>`)
		for _, s := range sorts {
			b.WriteString(`<option value="` + Esc(s[0]) + `">` + Esc(s[1]) + `</option>`)
		}
		b.WriteString("</select></label>")
	}
	if compare {
		b.WriteString(`<button class="primary" type="button" data-compare>Compare selected</button>`)
	}
	b.WriteString(`<button type="button" data-reset>Reset</button><span class="filter-count" data-count aria-live="polite"></span></div>`)
	return b.String()
}

// AsksSection is the product asks register.
func AsksSection(run, data M) string {
	asks := util.Maps(data["asks"])
	var kinds [][2]string
	for _, k := range StatusNames {
		switch k {
		case "verified_defect", "observed_friction", "untested_risk", "feature_request":
			kinds = append(kinds, [2]string{k, Statuses[k][0]})
		}
	}
	controls := Toolbar("asks", kinds, [][2]string{{"span", "Longest linked span"}, {"tokens", "Most output tokens"},
		{"calls", "Most tool calls"}, {"failed", "Most failed calls"}}, false)
	plural := "s"
	if len(asks) == 1 {
		plural = ""
	}
	var b strings.Builder
	b.WriteString(`<section class="section" id="asks" data-filter-scope="asks"><div class="section-heading">`)
	b.WriteString(fmt.Sprintf(`<h2>Product asks</h2><p>%d requested change%s</p></div>`, len(asks), plural))
	b.WriteString(`<p class="section-intro">Ordered by the reporter’s priority rationale. Costs belong to the linked events; ` +
		`overlapping costs are not additive.</p>`)
	if len(asks) > 0 {
		b.WriteString(controls)
	}
	b.WriteString(`<div class="asks" data-items>`)
	for i, a := range asks {
		b.WriteString(AskCard(a, i, "", false))
	}
	b.WriteString("</div>")
	if len(asks) > 0 {
		b.WriteString(`<p class="empty" data-empty hidden>No asks match. Reset the filters to see the complete register.</p>`)
	} else {
		b.WriteString(EmptyRegister(data))
	}
	b.WriteString("</section>")
	return b.String()
}

// ---------------------------------------------------------------- journey

// Mark is one event's annotations.
type Mark struct {
	Labels  []string
	Sources []string
	Asks    []any
}

var (
	numberedItem = regexp.MustCompile(`^[` + pyfmt.WS + `]*` + pyfmt.Digit + `+[.)][` + pyfmt.WS + `]+`)
)

func appendUnique(list []string, items ...string) []string {
	for _, it := range items {
		if !util.Contains(list, it) {
			list = append(list, it)
		}
	}
	return list
}

// startsWithLabel is re.match(rf"(?:{icon}\s*)?(?:\*\*)?{name}\b", text).
func startsWithLabel(text, icon, name string) bool {
	try := func(s string) bool {
		s = strings.TrimPrefix(s, "**")
		if !strings.HasPrefix(s, name) {
			return false
		}
		r, size := pyfmt.RuneAt(s, len(name))
		return size == 0 || !pyfmt.IsWord(r)
	}
	if rest, ok := strings.CutPrefix(text, icon); ok {
		if try(pyfmt.LStrip(rest)) {
			return true
		}
	}
	return try(text)
}

// eventRefs is re.findall(r"\bE-\d{3,}\b", text).
func eventRefs(text string) []string {
	var out []string
	for i := 0; i+1 < len(text); i++ {
		if text[i] != 'E' || text[i+1] != '-' {
			continue
		}
		if r, size := pyfmt.RuneBefore(text, i); size > 0 && pyfmt.IsWord(r) {
			continue
		}
		j, count := i+2, 0
		for j < len(text) {
			r, size := pyfmt.RuneAt(text, j)
			if !pyfmt.IsDigit(r) {
				break
			}
			j += size
			count++
		}
		if count < 3 {
			continue
		}
		if r, size := pyfmt.RuneAt(text, j); size > 0 && pyfmt.IsWord(r) {
			continue
		}
		out = append(out, text[i:j])
		i = j - 1
	}
	return out
}

// Annotations attaches only cited labels. Narrative steps take precedence over broader ask labels.
func Annotations(markdown string, data M, events []M) map[string]*Mark {
	result := map[string]*Mark{}
	for _, e := range events {
		if id, ok := e["eid"].(string); ok {
			result[id] = &Mark{Labels: []string{}, Sources: []string{}, Asks: []any{}}
		}
	}
	add := func(refs []string, names []string, source string) {
		for _, eid := range refs {
			if m, ok := result[eid]; ok {
				m.Labels = appendUnique(m.Labels, names...)
				if !util.Contains(m.Sources, source) {
					m.Sources = append(m.Sources, source)
				}
			}
		}
	}
	inChronology := false
	var blocks, block []string
	for _, line := range pyfmt.SplitLines(markdown) {
		if strings.HasPrefix(line, "## ") {
			if len(block) > 0 {
				blocks = append(blocks, strings.Join(block, "\n"))
				block = nil
			}
			inChronology = strings.Contains(strings.ToLower(line), "chronolog")
		} else if inChronology {
			numbered := numberedItem.MatchString(line)
			if numbered && len(block) > 0 {
				blocks = append(blocks, strings.Join(block, "\n"))
				block = nil
			}
			if len(block) > 0 || numbered {
				block = append(block, line)
			}
		}
	}
	if len(block) > 0 {
		blocks = append(blocks, strings.Join(block, "\n"))
	}
	for _, blk := range blocks {
		first := strings.TrimLeft(numberedItem.ReplaceAllString(blk, ""), "* ")
		var names []string
		for _, name := range LabelNames {
			if startsWithLabel(first, Labels[name][0], name) {
				names = append(names, name)
			}
		}
		add(eventRefs(blk), names, "Chronological account")
	}
	narrated := map[string]bool{}
	for id, m := range result {
		if len(m.Labels) > 0 {
			narrated[id] = true
		}
	}
	for _, ask := range util.Maps(data["asks"]) {
		var refs []string
		for _, r := range util.List(ask["event_refs"]) {
			if s, ok := r.(string); ok && result[s] != nil {
				refs = append(refs, s)
			}
		}
		for _, eid := range refs {
			result[eid].Asks = append(result[eid].Asks, getd(ask, "id", "Ask"))
		}
		var fresh []string
		for _, eid := range refs {
			if !narrated[eid] {
				fresh = append(fresh, eid)
			}
		}
		var names []string
		for _, n := range util.List(ask["labels"]) {
			if s, ok := n.(string); ok {
				if _, known := Labels[s]; known {
					names = append(names, s)
				}
			}
		}
		add(fresh, names, "Ask "+pyfmt.Str(getd(ask, "id", "")))
	}
	stringRefs := func(v any) []string {
		var out []string
		for _, r := range util.List(v) {
			if s, ok := r.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	for _, s := range util.Maps(data["strengths"]) {
		add(stringRefs(s["event_refs"]), []string{"Delight"}, "Strength register")
	}
	for _, g := range util.Maps(data["gates"]) {
		add(stringRefs(g["event_refs"]), []string{"Gate"}, "Gate register")
	}
	return result
}

// Elapsed is "+<duration>" since the task start, or "Time unavailable".
func Elapsed(run, event M) string {
	a, aok := run["task_started_at"].(string)
	b, bok := event["at"].(string)
	if !aok || !bok || a == "" || b == "" {
		return "Time unavailable"
	}
	ta, awareA, err1 := measure.ParseISO(a)
	tb, awareB, err2 := measure.ParseISO(b)
	if err1 != nil || err2 != nil || awareA != awareB {
		return "Time unavailable"
	}
	return "+" + Duration(pyfmt.Round(tb.Sub(ta).Seconds(), 3))
}

func firstTruthy(vals ...any) any { return util.Or(vals...) }

// JourneyMap is the SVG map of annotated events.
func JourneyMap(run M, events []M, marks map[string]*Mark) string {
	type group struct {
		event M
		label string
		refs  []string
		end   int
	}
	var groups []*group
	for i, event := range events {
		eid := util.S(event["eid"])
		kinds := marks[eid].Labels
		if i != 0 && i != len(events)-1 && len(kinds) == 0 && !util.Truthy(event["is_error"]) {
			continue
		}
		primary := ""
		if len(kinds) > 0 {
			primary = kinds[0]
		}
		if len(groups) > 0 && primary != "" && primary == groups[len(groups)-1].label && i == groups[len(groups)-1].end+1 {
			g := groups[len(groups)-1]
			g.refs = append(g.refs, eid)
			g.end = i
		} else {
			groups = append(groups, &group{event: event, label: primary, refs: []string{eid}, end: i})
		}
	}
	shown := groups
	if len(shown) > 24 {
		shown = shown[:24]
	}
	if len(shown) == 0 {
		return `<div class="empty">No event map is available. Read the narrative and its evidence limits below.</div>`
	}
	width := len(shown)*164 + 30
	if width < 660 {
		width = 660
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="journey-map" tabindex="0" role="region" aria-label="Scrollable recorded event map">`+
		`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="178" viewBox="0 0 %d 178" role="img" aria-labelledby="map-title map-desc">`, width, width)
	b.WriteString(`<title id="map-title">Recorded agent journey</title><desc id="map-desc">Selected events in recorded order. ` +
		`Each node links to the complete event table. Arrows indicate order, not proven causation.</desc>` +
		`<defs><marker id="arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="5" markerHeight="5" orient="auto">` +
		`<path d="M 0 0 L 10 5 L 0 10 z" fill="#9eb4d6"/></marker></defs>`)
	for i, g := range shown {
		event := g.event
		x, eid := 25+i*164, util.S(event["eid"])
		kinds := marks[eid].Labels
		label := "Event"
		if len(kinds) > 0 {
			label = kinds[0]
		} else if util.Truthy(event["is_error"]) {
			label = "Failed call"
		}
		icon, tone := "●", ""
		if util.Truthy(event["is_error"]) {
			tone = "bad"
		}
		if l, ok := Labels[label]; ok {
			icon, tone = l[0], l[1]
		}
		if i > 0 {
			fmt.Fprintf(&b, `<path class="route" d="M%d 71 H%d" marker-end="url(#arrow)"/>`, x-25, x-3)
		}
		action := pyfmt.CollapseSpace(pyfmt.Str(firstTruthy(event["input"], event["text"], event["name"], "Recorded event")))
		short := util.Clip(action, 22)
		if util.RuneLen(action) > 22 {
			short += "…"
		}
		var shape string
		if util.Contains(kinds, "Fork") {
			shape = fmt.Sprintf(`<path class="node %s" d="M%d 33 H%d L%d 71 L%d 109 H%d L%d 71 Z"/>`, tone, x+12, x+123, x+142, x+123, x+12, x-7)
		} else {
			shape = fmt.Sprintf(`<rect class="node %s" x="%d" y="33" width="140" height="76" rx="9"/>`, tone, x)
		}
		references := eid
		if len(g.refs) > 1 {
			references = eid + "–" + g.refs[len(g.refs)-1]
		}
		b.WriteString(`<a href="#` + eid + `" aria-label="` + Esc(references) + `: ` + Esc(label) + `"><title>` + Esc(action) + `</title>` + shape)
		fmt.Fprintf(&b, `<text x="%d" y="56" class="step-label">%s %s</text>`, x+10, icon, Esc(label))
		fmt.Fprintf(&b, `<text x="%d" y="77" class="step-meta">%s</text>`, x+10, Esc(references))
		fmt.Fprintf(&b, `<text x="%d" y="96" class="step-title">%s</text>`, x+10, Esc(short))
		fmt.Fprintf(&b, `<text x="%d" y="131" class="step-meta">%s</text></a>`, x+10, Esc(Elapsed(run, event)))
	}
	b.WriteString("</svg></div>")
	count := 0
	for _, g := range shown {
		count += len(g.refs)
	}
	limited := ""
	if len(groups) > 24 {
		limited = "; map limited to the first 24 steps"
	}
	fmt.Fprintf(&b, `<p class="small muted">%d steps summarize %d of %d recorded events%s. Consecutive events with the same annotation share a node. Arrows show recorded order.</p>`,
		len(shown), count, len(events), limited)
	return b.String()
}

// Happenings is the table of every recorded event.
func Happenings(run M, events []M, marks map[string]*Mark) string {
	var rows strings.Builder
	for i, event := range events {
		eid := util.S(event["eid"])
		info := marks[eid]
		kinds := info.Labels
		obstacle := util.Truthy(event["is_error"])
		for _, k := range kinds {
			if k != "Delight" {
				obstacle = true
			}
		}
		group := append([]string{}, kinds...)
		if obstacle {
			group = append(group, "Obstacles")
		}
		kindsAny := make([]any, len(kinds))
		for j, k := range kinds {
			kindsAny[j] = k
		}
		tags := LabelBadges(kindsAny)
		if tags == "" {
			if pyfmt.Equal(event["kind"], "text") {
				tags = Badge("Agent note", "")
			} else {
				tags = Badge("Tool call", "")
			}
		}
		action := firstTruthy(event["input"], event["text"], event["name"], "No event detail")
		output := event["output"]
		outcome := "Outcome not recorded"
		if event["is_error"] != nil {
			outcome = "Completed"
			if util.Truthy(event["is_error"]) {
				outcome = "Failed"
			}
		}
		var details string
		if util.Truthy(output) {
			details = `<details><summary>` + Esc(outcome) + ` · inspect response</summary><pre>` + Esc(output) + `</pre></details>`
		} else {
			small := "Recorded agent text"
			if pyfmt.Equal(event["kind"], "tool") {
				small = outcome
			}
			details = `<small>` + Esc(small) + `</small>`
		}
		var links strings.Builder
		seen := map[string]bool{}
		for _, aid := range info.Asks {
			key := pyfmt.Repr(aid)
			if seen[key] {
				continue
			}
			seen[key] = true
			links.WriteString(`<a class="mono" href="report.html#` + pyfmt.Quote(pyfmt.Str(aid), "") + `">` + Esc(aid) + `</a>`)
		}
		linksHTML := links.String()
		if linksHTML == "" {
			linksHTML = `<span class="muted">—</span>`
		}
		fmt.Fprintf(&rows, `<tr id="%s" data-item data-order="%d" data-kinds="%s">`, eid, i, Esc(strings.Join(group, "|")))
		rows.WriteString(`<td class="event-identity"><a class="mono" href="#` + eid + `">` + eid + `</a><small>` + Esc(Elapsed(run, event)) + `</small>`)
		rows.WriteString(`<small>` + Esc(firstTruthy(event["name"], event["kind"])) + `</small></td>`)
		rows.WriteString(`<td class="event-kind">` + tags + `<small>` + Esc(strings.Join(info.Sources, "; ")) + `</small></td>`)
		rows.WriteString(`<td class="event-action"><code>` + Esc(action) + `</code>` + details + `</td><td class="event-asks">` + linksHTML + `</td></tr>`)
	}
	kinds := [][2]string{{"Obstacles", "Obstacles"}}
	for _, name := range LabelNames {
		kinds = append(kinds, [2]string{name, name})
	}
	controls := Toolbar("events", kinds, nil, false)
	return `<section class="section" id="happenings" data-filter-scope="events"><div class="section-heading">` +
		`<h2>Table of happenings</h2><p>Every recorded event, with evidence links</p></div>` + controls +
		`<div class="table-wrap"><table class="happenings"><thead><tr><th scope="col">Event / elapsed</th>` +
		`<th scope="col">Annotation</th><th scope="col">Action and response</th><th scope="col">Linked asks</th>` +
		`</tr></thead><tbody data-items>` + rows.String() + "</tbody></table></div>" +
		`<p class="empty" data-empty hidden>No matching events. Reset the filters to restore the full timeline.</p></section>`
}

// citeEvents is re.sub(r"\[(E-\d{3,})\](?!\()", ...): citations of known
// events become links to them.
func citeEvents(markdown string, marks map[string]*Mark) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(markdown); i++ {
		if markdown[i] != '[' || !strings.HasPrefix(markdown[i+1:], "E-") {
			continue
		}
		j, count := i+3, 0
		for j < len(markdown) {
			r, size := pyfmt.RuneAt(markdown, j)
			if !pyfmt.IsDigit(r) {
				break
			}
			j += size
			count++
		}
		if count < 3 || j >= len(markdown) || markdown[j] != ']' || (j+1 < len(markdown) && markdown[j+1] == '(') {
			continue
		}
		id := markdown[i+1 : j]
		if marks[id] != nil {
			b.WriteString(markdown[last:i])
			b.WriteString("[" + id + "](#" + id + ")")
			last = j + 1
		}
		i = j
	}
	if last == 0 {
		return markdown
	}
	b.WriteString(markdown[last:])
	return b.String()
}

// JourneyBody is the pretty-journey.html body.
func JourneyBody(run, data, tel, summary M, markdown string) string {
	var events []M
	for _, e := range util.Maps(tel["events"]) {
		if IsEventID(e["eid"]) {
			events = append(events, e)
		}
	}
	marks := Annotations(markdown, data, events)
	var legend strings.Builder
	for _, name := range LabelNames {
		legend.WriteString(`<span>` + Labels[name][0] + ` ` + Esc(name) + `</span>`)
	}
	cited := citeEvents(markdown, marks)
	if cited == "" {
		cited = "No journey was recorded."
	}
	happenings := `<div class="notice warn">No normalized event evidence is available. ` +
		`A narrative alone cannot supply measured event timing or costs.</div>`
	if len(events) > 0 {
		happenings = Happenings(run, events, marks)
	}
	return ReviewNotice(run, data) + SummaryStrip(run, summary) + ConfigurationOverview(run) +
		`<section class="section"><div class="section-heading"><h2>Journey map</h2>` +
		`<a href="#account">Read the first-person account</a></div>` + JourneyMap(run, events, marks) +
		`<details><summary>AJX annotation legend</summary><div class="legend">` + legend.String() + `</div>` +
		`<p class="small muted">A label describes an observation. It does not assign a severity or prove a cause.</p></details></section>` +
		happenings +
		`<section class="section" id="account"><h2>First-person account</h2>` +
		`<p class="section-intro">` + Esc(util.Or(run["journey_provenance"], "Provenance unavailable")) + `</p>` +
		`<article class="panel narrative">` + mdlite.Render(cited) + "</article></section>" +
		ConfigurationSection(run)
}

// RunBody is the report.html body.
func RunBody(run, data, summary M, digest string, journeyExists bool) string {
	evidence := rec(run["evidence"])
	coverage := [][2]any{{"Usage", evidence["usage_coverage"]}, {"Tool calls", evidence["tool_call_coverage"]},
		{"Timestamps", evidence["timestamp_coverage"]}, {"Narrative", run["journey_provenance"]},
		{"Prompt SHA-256", rec(run["task_prompt"])["sha256"]},
		{"Trial SHA-256", run["evaluation_wrapper_sha256"]}, {"Skill revision", run["ajx_skill_revision"]}}
	var cov strings.Builder
	cov.WriteString(`<dl class="coverage">`)
	for _, c := range coverage {
		cov.WriteString(`<dt>` + Esc(c[0]) + `</dt><dd>` + Esc(util.Or(c[1], "Unavailable")) + `</dd>`)
	}
	cov.WriteString("</dl>")
	agent := rec(run["agent"])
	checks := `<h3>Task outcome</h3>` + Status(run["task_outcome_verified"]) +
		`<p>` + Esc(run["verification_scope"]) + `</p><h4>Agent’s declaration at stop</h4>` +
		`<p>` + Esc(util.Or(run["task_outcome_declared"], "Not recorded")) + `</p>` +
		`<p class="small muted">Stop reason: ` + Esc(util.Or(run["task_stop_reason"], "Unknown")) + `</p>` +
		`<a href="verify.json">Inspect verification checks</a>`
	var strengths, gates strings.Builder
	for _, s := range util.Maps(data["strengths"]) {
		strengths.WriteString(`<li><strong>` + Esc(s["title"]) + `</strong><p>` + Esc(s["how_observed"]) + ` ` +
			EventLinks(s["event_refs"], "pretty-journey.html") + `</p></li>`)
	}
	for _, g := range util.Maps(data["gates"]) {
		badge := Badge("Unresolved", "warn")
		if util.Truthy(g["resolved"]) {
			badge = Badge("Resolved", "good")
		}
		gates.WriteString(`<li><strong>` + Esc(g["trigger"]) + `</strong> ` + badge +
			`<p>` + Esc(g["agent_action"]) + ` ` + EventLinks(g["event_refs"], "pretty-journey.html") + `</p></li>`)
	}
	journeyLink := `<span class="muted">Journey unavailable; inspect the evidence digest below.</span>`
	if journeyExists {
		journeyLink = `<a href="pretty-journey.html">Open journey map and event table</a>`
	}
	strengthsHTML := `<p class="muted">No specific strength recorded.</p>`
	if strengths.Len() > 0 {
		strengthsHTML = "<ul>" + strengths.String() + "</ul>"
	}
	gatesHTML := `<p class="muted">No human gate recorded.</p>`
	if gates.Len() > 0 {
		gatesHTML = "<ul>" + gates.String() + "</ul>"
	}
	var limits strings.Builder
	notes := util.List(run["limitations"])
	if len(notes) == 0 {
		notes = []any{"No additional limitation recorded."}
	}
	for _, n := range notes {
		limits.WriteString("<li>" + Esc(n) + "</li>")
	}
	if digest == "" {
		digest = "No digest available."
	}
	return ReviewNotice(run, data) + SummaryStrip(run, summary) + ConfigurationOverview(run) + AsksSection(run, data) +
		ConfigurationSection(run) +
		`<section class="section"><div class="section-heading"><h2>Trace the experience</h2>` + journeyLink + "</div>" +
		`<div class="two-columns"><div class="panel"><h3>What helped</h3>` + strengthsHTML +
		`</div><div class="panel"><h3>Human gates</h3>` + gatesHTML + "</div></div></section>" +
		`<section class="section" id="verification"><h2>Verification and coverage</h2><div class="two-columns">` +
		`<div class="panel">` + checks + `</div><div class="panel"><h3>Evidence coverage</h3>` + cov.String() + `</div></div></section>` +
		`<section class="section" id="method"><h2>Method and limits</h2><div class="panel">` +
		`<p class="small">` + Esc(agent["harness_name"]) + ` ` + Esc(agent["harness_version"]) + `; ` +
		`model ` + Esc(util.Or(agent["model_id"], agent["model_requested"], "default")) + `; ` +
		`configuration ` + Esc(util.Or(rec(agent["settings"])["config"], "unknown")) + `.</p>` +
		`<ul>` + limits.String() + "</ul>" +
		`<p class="small">Shared events can contribute to several asks. Their costs cannot be added as independent savings. ` +
		`Modeled savings require a comparator and follow-up measurement.</p></div>` +
		`<details class="panel"><summary>Full evidence digest</summary>` + mdlite.Render(digest) + "</details></section>"
}

// ---------------------------------------------------------------- matrix

func consistencyOrder(consistency M) []string {
	var keys []string
	for _, k := range ConsistencyKeys {
		if _, ok := consistency[k]; ok {
			keys = append(keys, k)
		}
	}
	for _, k := range util.SortedKeys(consistency) {
		if !util.Contains(ConsistencyKeys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// ConsistencyOrder lists consistency keys in the order Python builds them.
func ConsistencyOrder(consistency M) []string { return consistencyOrder(consistency) }

// MatrixBody is the index.html body comparing runs.
func MatrixBody(spec M, rows, cells []M, consistency M, registers map[string]M, markdown string, clusters []M) string {
	product := util.Get(spec, "trial", "product")
	var out []string
	out = append(out, `<section class="section" data-filter-scope="runs"><div class="section-heading">`+
		`<h2>Asks by configuration</h2>`+
		fmt.Sprintf(`<p>%d runs across %d configurations</p></div>`, len(rows), len(cells))+
		`<p class="section-intro">Read the requested changes side by side. Each column retains its product version, `+
		`agent, evidence status, and measured costs.</p>`)
	if !util.Truthy(consistency["wall_clock_comparable"]) {
		out = append(out, `<div class="notice warn"><strong>Concurrent runs.</strong> Wall-clock values are shown for context and are not comparable.</div>`)
	}
	if len(util.List(consistency["product_versions"])) > 1 {
		out = append(out, `<div class="notice">Multiple product versions are present. Compare equivalent tasks and starting conditions; `+
			`a version change and an agent change are separate factors.</div>`)
	}
	var harnesses []string
	for _, r := range rows {
		harnesses = append(harnesses, pyfmt.Str(r["harness"]))
	}
	var hk [][2]string
	for _, h := range util.Uniq(harnesses) {
		hk = append(hk, [2]string{h, h})
	}
	out = append(out, Toolbar("runs", hk, [][2]string{{"asks", "Most asks"}, {"elapsed", "Longest task time"}, {"failed", "Most failed calls"}}, true))
	out = append(out, `<div class="compare-grid" data-items>`)
	for i, row := range rows {
		data := registers[util.S(row["dir"])]
		if data == nil {
			data = M{}
		}
		prefix := "runs/" + pyfmt.Quote(util.S(row["dir"]), "") + "/"
		model := util.Or(row["model_id"], row["model_requested"], "Default model")
		asks := util.Maps(data["asks"])
		profiles := ConfigurationValues(row)
		var header []string
		for _, p := range profiles[:4] {
			header = append(header, Esc(p[0])+": "+Esc(p[1]))
		}
		out = append(out, fmt.Sprintf(`<article class="run-card" data-item data-order="%d" data-kinds="%s" `, i, Esc(row["harness"]))+
			fmt.Sprintf(`data-asks="%d" data-elapsed="%s" data-failed="%s">`, len(asks), Esc(row["elapsed_seconds"]), Esc(row["failed_tool_calls"]))+
			`<header><label class="select-run js-only"><input type="checkbox" data-select-run checked>`+
			Esc(row["run_id"])+`</label><h3>`+Esc(product)+` `+Esc(util.Or(row["product_version"], "unknown version"))+`</h3>`+
			`<div class="run-config"><strong>`+Esc(row["harness"])+`</strong><br>`+Esc(model)+`<br>`+
			Esc(row["config"])+` configuration · `+Esc(row["run_id"])+`<br>`+strings.Join(header, "<br>")+`</div>`+Status(row["verified"])+
			`<dl class="run-facts"><div><dt>Task time</dt>`+
			`<dd>`+Esc(Duration(row["elapsed_seconds"]))+`</dd></div><div><dt>Output tokens</dt>`+
			`<dd>`+Esc(Number(row["output_tokens"]))+` <span class="small muted">(`+Esc(util.Or(row["usage_status"], "unavailable"))+`)</span></dd></div>`+
			`<div><dt>Tool calls / failed</dt><dd>`+Esc(Number(row["tool_calls"]))+` / `+Esc(Number(row["failed_tool_calls"]))+`</dd></div>`+
			`<div><dt>Human gates</dt><dd>`+pyfmt.Str(row["gates"])+`</dd></div></dl></header><div class="run-asks">`)
		if pyfmt.Equal(row["review_status"], "incomplete") {
			out = append(out, `<div class="notice bad">Review incomplete. Do not interpret an empty register as a clean run.</div>`)
		}
		switch util.S(row["teardown"]) {
		case "failed", "auth_errors", "skipped":
			out = append(out, `<div class="notice bad">Cleanup not confirmed. <a href="`+prefix+`teardown.json">Inspect cleanup</a>.</div>`)
		}
		out = append(out, EnvironmentCleanupNotice(row, prefix))
		if util.Truthy(row["verify_auth_errors"]) {
			out = append(out, `<div class="notice warn">Verification encountered credential errors. Inspect the checks before comparing outcomes.</div>`)
		}
		if len(asks) > 0 {
			var cards strings.Builder
			for n, ask := range asks {
				cards.WriteString(AskCard(ask, n, prefix, true))
			}
			out = append(out, cards.String())
		} else {
			out = append(out, EmptyRegister(data))
		}
		var rest []string
		for _, p := range profiles[4:] {
			rest = append(rest, p[0]+": "+p[1])
		}
		var lims strings.Builder
		for _, n := range ProfileLimitations(row) {
			lims.WriteString(`<li>` + Esc(n) + `</li>`)
		}
		out = append(out, `<details><summary>Configuration coverage and limits</summary><p class="small">`+
			Esc(strings.Join(rest, "; "))+
			`</p><p class="small">`+Esc(ProfileCoverage(row))+`</p>`+
			`<p class="small">Enabled extensions do not prove use. Missing loaded/invoked telemetry remains unknown. `+
			`A private HOME is not a filesystem sandbox.</p>`+
			`<ul>`+lims.String()+"</ul>"+
			`<div class="links">`+ProfileLinks(row, prefix)+`<a href="`+prefix+`run.json">Complete run evidence</a></div></details>`)
		out = append(out, `</div><footer class="links"><a href="`+prefix+`report.html">Full ask register</a>`+
			`<a href="`+prefix+`pretty-journey.html">Journey</a></footer></article>`)
	}
	out = append(out, `</div><p class="empty" data-empty hidden>No runs match the current selection. Reset to see the complete matrix.</p></section>`)
	out = append(out, `<section class="section" id="measurements"><h2>Technical comparison</h2>`+
		`<p class="section-intro">Complete run measurements. Missing telemetry stays unavailable. `+
		`Output tokens use each model’s tokenizer; provider dollars and credits keep separate units.</p>`+
		`<div class="table-wrap"><table><thead><tr>`)
	headings := []string{"Run / product version", "Harness / model", "Environment / backend", "Agent profile / extensions",
		"Environment cleanup", "Verified", "Task time", "Calls / failed", "Output tokens", "Asks", "Provider USD", "Credits"}
	var th strings.Builder
	for _, h := range headings {
		th.WriteString(`<th scope="col">` + h + `</th>`)
	}
	out = append(out, th.String()+"</tr></thead><tbody>")
	for _, row := range rows {
		prefix := "runs/" + pyfmt.Quote(util.S(row["dir"]), "") + "/"
		var profiles []string
		for _, p := range ConfigurationValues(row) {
			profiles = append(profiles, p[1])
		}
		cleanupTone := ""
		if CleanupProblem(row["environment_cleanup"]) {
			cleanupTone = "bad"
		}
		values := []string{
			`<a class="mono" href="` + prefix + `report.html">` + Esc(row["run_id"]) + `</a><small>` + Esc(row["product_version"]) + `</small>`,
			Esc(row["harness"]) + `<small>` + Esc(util.Or(row["model_id"], row["model_requested"], "default")) + `</small>`,
			Esc(profiles[0]) + `<small>` + Esc(profiles[1]) + `</small>` +
				`<small><a href="` + prefix + `report.html#configuration">Capabilities and limits</a></small>`,
			Esc(profiles[2]) + `<small>Skills (declared): ` + Esc(profiles[3]) + `</small>` +
				`<small>Plugins (declared): ` + Esc(profiles[4]) + `</small><small>Hooks (declared): ` + Esc(profiles[5]) + `</small>`,
			Badge(profiles[6], cleanupTone),
			Status(row["verified"]), Esc(Duration(row["elapsed_seconds"])),
			Esc(Number(row["tool_calls"])) + ` / ` + Esc(Number(row["failed_tool_calls"])) + `<small>` + Esc(row["tool_status"]) + `</small>`,
			Esc(Number(row["output_tokens"])) + `<small>` + Esc(row["usage_status"]) + `</small>`,
			pyfmt.Str(row["asks"]), Esc(Number(row["cost_estimate_usd"])), Esc(Number(row["credits"]))}
		var tr strings.Builder
		tr.WriteString("<tr>")
		for _, v := range values {
			tr.WriteString("<td>" + v + "</td>")
		}
		tr.WriteString("</tr>")
		out = append(out, tr.String())
	}
	out = append(out, "</tbody></table></div></section>")
	if len(clusters) > 0 {
		out = append(out, `<section class="section"><h2>Related asks across runs</h2>`+
			`<p class="section-intro">Reporter-inferred groups. Inspect each member before treating them as the same defect.</p>`)
		for _, c := range clusters {
			var links []string
			for _, m := range util.Maps(c["members"]) {
				links = append(links, `<a href="runs/`+pyfmt.Quote(pyfmt.Str(m["run_id"]), "")+`/report.html#`+pyfmt.Quote(pyfmt.Str(m["ask_id"]), "")+`">`+
					Esc(m["run_id"])+`/`+Esc(m["ask_id"])+`</a>`)
			}
			out = append(out, `<div class="panel"><h3>`+Esc(c["title"])+`</h3><div class="links">`+strings.Join(links, " ")+`</div></div>`)
		}
		out = append(out, "</section>")
	}
	out = append(out, `<section class="section"><h2>Comparability and provenance</h2><div class="panel"><dl class="coverage">`)
	for _, key := range consistencyOrder(consistency) {
		out = append(out, `<dt>`+Esc(strings.ReplaceAll(key, "_", " "))+`</dt><dd>`+Esc(consistency[key])+`</dd>`)
	}
	out = append(out, `</dl></div><details class="panel"><summary>Complete matrix, variance, and source records</summary>`+
		mdlite.Render(markdown)+"</details></section>")
	if len(rows) == 0 {
		out = append([]string{`<div class="empty">No runs have been recorded yet. Run the trial to populate this comparison.</div>`}, out...)
	}
	return strings.Join(out, "")
}
