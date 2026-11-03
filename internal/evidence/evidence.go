// Package evidence ports lib/ajx/evidence.py: event IDs, digest, isolation
// scan, per-ask cost attribution and reconciled phase measurements. All of it
// is done by code, never by an LLM.
package evidence

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/royosherove/ajx/internal/envcheck"
	"github.com/royosherove/ajx/internal/measure"
	"github.com/royosherove/ajx/internal/res"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

type M = util.M

// Digest bounds for very long tasks (DIGEST_MAX_EVENTS, DIGEST_EDGE).
const (
	DigestMaxEvents = 300
	DigestEdge      = 110
)

// AskCostBasis explains how per-ask measurements are computed.
const AskCostBasis = "sum over referenced events; tokens attributed per whole assistant message (two asks citing " +
	"events from the same message both carry its tokens); span = first referenced event start " +
	"to last referenced event end"

// MeasurementsNote is measurements.json's note.
const MeasurementsNote = "execute is the task window; verify/narrate/extract are separate windows and are " +
	"never added to task cost. Reporter token totals are single harness-reported values."

// SkillRoot, when set, is the on-disk skill directory the isolation scan
// treats as evaluation material (Python SKILL_ROOT). When empty, SkillDir
// finds it.
var SkillRoot = ""

// SkillDir is the skill directory an agent could read evaluation material
// from: SkillRoot, else AJX_SKILL_DIR, else a skills/ajx checkout next to
// the running binary (bin/ajx in a repository checkout); "" when the skill
// exists only embedded in the binary.
func SkillDir() string {
	if SkillRoot != "" {
		return SkillRoot
	}
	if d := res.Dir(); d != "" {
		return d
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	dir := filepath.Dir(exe)
	for i := 0; i < 3; i++ {
		cand := filepath.Join(dir, "skills", "ajx")
		if st, err := os.Stat(filepath.Join(cand, "SKILL.md")); err == nil && !st.IsDir() {
			return cand
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

func tsKey(ev M) string {
	if s := util.Str(ev["at"]); util.Truthy(ev["at"]) {
		return s
	}
	return "9999"
}

func parse(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, _, err := measure.ParseISO(s)
	return t, err == nil
}

func secondsBetween(a, b any) any {
	if !util.Truthy(a) || !util.Truthy(b) {
		return nil
	}
	ta, ok1 := parse(a)
	tb, ok2 := parse(b)
	if !ok1 || !ok2 {
		return nil
	}
	return pyRound(float64(tb.Sub(ta).Microseconds())/1e6, 3)
}

// pyRound is Python's round(x, n) for floats (correctly rounded, ties to even).
func pyRound(f float64, places int) float64 {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return f
	}
	r, err := strconv.ParseFloat(strconv.FormatFloat(f, 'f', places, 64), 64)
	if err != nil {
		return f
	}
	return r
}

// AssignIDs sorts events by time (stable for equal timestamps), numbers
// them E-001..., and records tool durations.
func AssignIDs(tel M) M {
	events := util.Maps(tel["events"])
	sort.SliceStable(events, func(i, j int) bool { return tsKey(events[i]) < tsKey(events[j]) })
	out := make([]any, len(events))
	for i, ev := range events {
		ev["eid"] = fmt.Sprintf("E-%03d", i+1)
		if ev["kind"] == "tool" {
			ev["duration_s"] = secondsBetween(ev["at"], ev["end"])
		}
		out[i] = ev
	}
	tel["events"] = out
	return tel
}

// pyPath is str(Path(p)): repeated and trailing slashes and "." parts dropped.
func pyPath(p string) string {
	if p == "" {
		return "."
	}
	lead := ""
	if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		lead = "//"
	} else if strings.HasPrefix(p, "/") {
		lead = "/"
	}
	var parts []string
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			parts = append(parts, s)
		}
	}
	out := lead + strings.Join(parts, "/")
	if out == "" {
		return "."
	}
	return out
}

var ajxWord = regexp.MustCompile(`(?i)\bajx`)

// IsolationScan flags tool inputs that reach outside the run's own workspace
// toward evaluation material.
func IsolationScan(tel, sp, state M) []any {
	ownSet := map[string]bool{}
	for _, v := range util.MapOr(state["paths"]) {
		if v != nil {
			ownSet[pyPath(util.Str(v))] = true
		}
	}
	own := util.SortedKeys(ownSet)
	root := util.Str(util.Get(sp, "trial", "workspace_root"))
	type needle struct{ label, value string }
	needles := []needle{
		{"report/output dir", util.Str(util.Get(sp, "trial", "output_dir"))},
		{"trial dir", pyPath(filepath.Dir(util.Str(sp["path"])))},
		{"ajx skill files", SkillDir()},
		{"skills dir", "/.claude/skills"},
		{"harness transcripts", "/.claude/projects"},
	}
	scrubList := append([]string{}, own...)
	if !ownSet[root] {
		scrubList = append(scrubList, root)
	}
	sort.SliceStable(scrubList, func(i, j int) bool { return util.RuneLen(scrubList[i]) > util.RuneLen(scrubList[j]) })
	runEntry := regexp.MustCompile(regexp.QuoteMeta(root) + `/[0-9a-f]{10}`)
	flags := []any{}
	for _, ev := range util.Maps(tel["events"]) {
		if ev["kind"] != "tool" {
			continue
		}
		var blob string
		if ev["input_raw"] != nil {
			blob = util.PyJSON(ev["input_raw"])
		} else {
			blob = util.Str(ev["input"])
		}
		scrubbed := blob
		for _, p := range scrubList {
			if p == "" {
				continue
			}
			scrubbed = strings.ReplaceAll(scrubbed, p, "<own>")
		}
		var hits []string
		for _, n := range needles {
			if n.value != "" && strings.Contains(scrubbed, n.value) {
				hits = append(hits, n.label)
			}
		}
		if ajxWord.MatchString(scrubbed) {
			hits = append(hits, "mentions 'ajx'")
		}
		// root + "/" + ten hex characters (the _rand() shape) not followed by [0-9A-Za-z_-]
		for pos := 0; pos <= len(blob); {
			loc := runEntry.FindStringIndex(blob[pos:])
			if loc == nil {
				break
			}
			start, end := pos+loc[0], pos+loc[1]
			if end < len(blob) && isIDChar(blob[end]) {
				pos = start + 1
				continue
			}
			match := blob[start:end]
			mine := false
			for _, o := range own {
				if strings.HasPrefix(match, o) {
					mine = true
					break
				}
			}
			if !mine {
				hits = append(hits, "another run's workspace root entry")
			}
			pos = end
		}
		if len(hits) > 0 {
			flags = append(flags, M{"eid": ev["eid"], "reasons": util.List(util.Uniq(hits)),
				"input": util.Clip(util.Str(ev["input"]), 200)})
		}
	}
	return flags
}

func isIDChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-'
}

func mmss(seconds any) string {
	f, ok := util.Num(seconds)
	if !ok {
		return "?"
	}
	s := int64(math.RoundToEven(f))
	q, r := s/60, s%60
	if r < 0 {
		q, r = q-1, r+60
	}
	return fmt.Sprintf("%02d:%02d", q, r)
}

func isPySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func pyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

func oneLine(text any, n int) string {
	t := strings.Join(strings.FieldsFunc(util.Str(text), isPySpace), " ")
	t = strings.ReplaceAll(t, "|", "\\|")
	if util.RuneLen(t) <= n {
		return t
	}
	return util.Clip(t, n-1) + "…"
}

// number formats a numeric value as Python would print it: integral counts
// read back from JSON (float64 in Go) print as ints.
func number(v any) string {
	if f, ok := v.(float64); ok && f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return spec.PyStr(v)
}

// TaskSummary computes the task window facts from state and telemetry.
func TaskSummary(state, tel M) M {
	ex := util.MapOr(state["execute"])
	events := util.Maps(tel["events"])
	var tools []M
	for _, e := range events {
		if e["kind"] == "tool" {
			tools = append(tools, e)
		}
	}
	failed := 0
	for _, e := range tools {
		if util.Truthy(e["is_error"]) {
			failed++
		}
	}
	retries := 0
	for i := 1; i < len(tools); i++ {
		if util.Truthy(tools[i-1]["is_error"]) && sameValue(tools[i-1]["name"], tools[i]["name"]) {
			retries++
		}
	}
	gaps := []any{}
	var timed []M
	for _, e := range events {
		if util.Truthy(e["at"]) {
			timed = append(timed, e)
		}
	}
	for i := 1; i < len(timed); i++ {
		prev, cur := timed[i-1], timed[i]
		pat, ok1 := parse(prev["at"])
		cat, ok2 := parse(cur["at"])
		if !ok1 || !ok2 {
			continue
		}
		prevEnd := pat
		if util.Truthy(prev["end"]) {
			if pe, ok := parse(prev["end"]); ok && pe.After(prevEnd) {
				prevEnd = pe
			}
		}
		idle := float64(cat.Sub(prevEnd).Microseconds()) / 1e6
		if idle >= 60 {
			gaps = append(gaps, pyRound(idle, 1))
		}
	}
	tokens, integral := 0.0, true
	for _, u := range util.Maps(tel["usage"]) {
		f, _ := util.Num(u["output_tokens"])
		if _, isFloat := u["output_tokens"].(float64); isFloat && f != math.Trunc(f) {
			integral = false
		}
		tokens += f
	}
	var outTokens any
	if tel["usage_status"] != "unavailable" {
		if integral {
			outTokens = int64(tokens)
		} else {
			outTokens = tokens
		}
	}
	var longest any
	if len(tools) > 0 {
		for i, e := range tools {
			f, _ := util.Num(e["duration_s"]) // None or 0 -> 0
			if best, _ := longest.(float64); i == 0 || f > best {
				longest = f
			}
		}
	}
	hr := M{}
	for k, v := range util.MapOr(tel["harness_reported"]) {
		if k != "model_usage" {
			hr[k] = v
		}
	}
	return M{
		"elapsed_seconds":      secondsBetween(ex["started_at"], ex["stopped_at"]),
		"elapsed_source":       "ajx process start/stop (same clock for every harness)",
		"stop_reason":          ex["stop_reason"],
		"harness_stop_reason":  tel["stop_reason"],
		"exit_code":            ex["exit_code"],
		"timed_out":            ex["timed_out"],
		"tool_calls":           int64(len(tools)),
		"tool_status":          tel["tool_status"],
		"failed_tool_calls":    int64(failed),
		"retries_after_error":  int64(retries),
		"output_tokens":        outTokens,
		"usage_status":         tel["usage_status"],
		"longest_tool_seconds": longest,
		"gaps_over_60s":        gaps,
		"model_ids":            tel["model_ids"],
		"harness_reported":     hr,
	}
}

func sameValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return util.PyJSON(a) == util.PyJSON(b)
}

var backticks = regexp.MustCompile("`+")

func fence(text string) string {
	longest := 0
	for _, m := range backticks.FindAllString(text, -1) {
		if len(m) > longest {
			longest = len(m)
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// DigestOpts are BuildDigest's optional arguments.
type DigestOpts struct {
	// OmitVerification is Python include_verification=False: the narrator's
	// digest, so the agent's own account of its outcome is not colored by
	// checks it never ran.
	OmitVerification bool
}

func emptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case M:
		return len(t) == 0
	}
	return false
}

// BuildDigest is the factual evidence digest handed to the reporting passes.
func BuildDigest(sp, cell, state, tel, verify, environment M, o DigestOpts) string {
	includeVerification := !o.OmitVerification
	ex := util.MapOr(state["execute"])
	s := TaskSummary(state, tel)
	start := ex["started_at"]
	model := util.Str(util.Or(cell["model"], "harness default"))
	ids := strings.Join(util.Strings(s["model_ids"]), ", ")
	if ids == "" {
		ids = "not exposed"
	}
	tokens := "unavailable"
	if s["output_tokens"] != nil {
		tokens = number(s["output_tokens"])
	}
	gaps := "none"
	if l := util.List(s["gaps_over_60s"]); len(l) > 0 {
		gaps = spec.Repr(l)
	}
	lines := []string{
		"# Evidence digest (factual, generated by ajx)",
		"",
		"All numbers below were computed by ajx from recorded telemetry. Use them as given; do not recompute.",
		"",
		"## Run facts",
		"",
		fmt.Sprintf("- Harness: %s (%s)", util.Str(cell["harness"]), spec.PyStr(util.Get(environment, "harness", "version_before"))),
		fmt.Sprintf("- Model requested: %s; model ids observed: %s", model, ids),
		fmt.Sprintf("- Harness config: %s", spec.PyStr(cell["config"])),
		fmt.Sprintf("- Task window: %s to %s (%s s, %s)", spec.PyStr(start), spec.PyStr(ex["stopped_at"]),
			spec.PyStr(s["elapsed_seconds"]), s["elapsed_source"]),
		fmt.Sprintf("- Stop: %s (harness: %s), exit code %s", spec.PyStr(s["stop_reason"]),
			spec.PyStr(s["harness_stop_reason"]), number(s["exit_code"])),
		fmt.Sprintf("- Tool calls: %s (%s); failed: %s; same-tool retries after an error: %s",
			number(s["tool_calls"]), spec.PyStr(s["tool_status"]), number(s["failed_tool_calls"]), number(s["retries_after_error"])),
		fmt.Sprintf("- Output tokens: %s (%s)", tokens, spec.PyStr(s["usage_status"])),
		fmt.Sprintf("- Gaps of 60 s or more between recorded events: %s", gaps),
		fmt.Sprintf("- Timestamp source: %s", spec.PyStr(tel["timestamp_source"])),
	}
	if hr := util.MapOr(s["harness_reported"]); len(hr) > 0 {
		var parts []string
		for _, k := range util.SortedKeys(hr) {
			if !emptyValue(hr[k]) {
				parts = append(parts, k+"="+number(hr[k]))
			}
		}
		lines = append(lines, "- Harness-reported (estimates, labeled as such): "+strings.Join(parts, "; "))
	}
	lines = append(lines, "", "## Timeline", "",
		"| Event | +mm:ss | Kind | Tool | Input / text | Duration s | Error | Output excerpt |",
		"|---|---|---|---|---|---|---|---|")
	events := util.List(tel["events"])
	for _, raw := range DigestEvents(events) {
		ev := util.MapOr(raw)
		if ev["kind"] == "omitted" {
			lines = append(lines, fmt.Sprintf("| %s..%s | | omitted | | %s routine events omitted from this "+
				"digest for length (%s error or text events in that range are shown); "+
				"the full list is in events.jsonl | | | |", util.Str(ev["first"]), util.Str(ev["last"]),
				number(ev["count"]), number(ev["kept"])))
			continue
		}
		off := "?"
		if util.Truthy(ev["at"]) && util.Truthy(start) {
			off = mmss(secondsBetween(start, ev["at"]))
		}
		if ev["kind"] == "tool" {
			dur := "?"
			if ev["duration_s"] != nil {
				dur = spec.PyStr(ev["duration_s"])
			}
			errText := "?"
			if util.Truthy(ev["is_error"]) {
				errText = "yes"
			} else if ev["is_error"] == false {
				errText = "no"
			}
			lines = append(lines, fmt.Sprintf("| %s | %s | tool | %s | %s | %s | %s | %s |", util.Str(ev["eid"]), off,
				oneLine(ev["name"], 30), oneLine(ev["input"], 140), dur, errText, oneLine(ev["output"], 220)))
		} else {
			lines = append(lines, fmt.Sprintf("| %s | %s | text |  | %s |  |  |  |", util.Str(ev["eid"]), off,
				oneLine(ev["text"], 260)))
		}
	}
	if len(events) == 0 {
		lines = append(lines, "| - | - | - | - | No event-level telemetry for this harness | - | - | - |")
	}
	final := util.Str(util.Or(tel["final_text"], "(none captured)"))
	final = util.Clip(pyStrip(final), 6000)
	f := fence(final)
	lines = append(lines, "", "## Final message at the stopping point (verbatim declared outcome)", "", f+"text", final, f, "")
	if includeVerification {
		lines = append(lines, "## Reporter verification (run by ajx AFTER the task agent stopped; not done by the task agent)", "")
		if checks := util.Maps(verify["checks"]); len(checks) > 0 {
			lines = append(lines, fmt.Sprintf("Result: %s/%s checks passed -> %s", number(verify["passed"]),
				number(verify["total"]), spec.PyStr(verify["outcome"])))
			lines = append(lines, "")
			for _, c := range checks {
				mark := "FAIL"
				if util.Truthy(c["passed"]) {
					mark = "PASS"
				}
				line := fmt.Sprintf("- %s: %s (%s; %s)", mark, spec.PyStr(c["name"]), spec.PyStr(c["type"]), spec.PyStr(c["detail"]))
				if util.Truthy(c["scope"]) {
					line += " scope: " + spec.PyStr(c["scope"])
				}
				if util.Truthy(c["auth_errors"]) {
					line += " [this check's own output shows credential errors: the result reflects ajx's " +
						"verification credentials, not the agent's work]"
				}
				lines = append(lines, line)
			}
		} else {
			lines = append(lines, "No verification checks were defined.")
		}
	}
	if flags := util.Maps(tel["isolation_flags"]); includeVerification && len(flags) > 0 {
		lines = append(lines, "", "## Isolation flags", "")
		for _, fl := range flags {
			lines = append(lines, fmt.Sprintf("- %s: %s", util.Str(fl["eid"]), strings.Join(util.Strings(fl["reasons"]), ", ")))
		}
	}
	var notes []string
	if includeVerification {
		notes = envcheck.RecordedNotes(environment)
	}
	if len(notes) > 0 {
		lines = append(lines, "", "## Environment notes (the trial machine's configuration, recorded by ajx before the task)", "")
		for _, n := range notes {
			lines = append(lines, "- "+n)
		}
	}
	lines = append(lines, "", "## Telemetry limitations", "")
	lim := util.List(tel["limitations"])
	if len(lim) == 0 {
		lines = append(lines, "- none recorded")
	}
	for _, l := range lim {
		lines = append(lines, "- "+spec.PyStr(l))
	}
	return strings.Join(lines, "\n") + "\n"
}

// DigestEvents bounds the digest for very long tasks: the first and last
// DigestEdge events verbatim and, in between, only failed tool calls and
// assistant text; the rest is summarized as one "omitted" row.
func DigestEvents(events []any) []any {
	if len(events) <= DigestMaxEvents {
		return append([]any{}, events...)
	}
	head, middle, tail := events[:DigestEdge], events[DigestEdge:len(events)-DigestEdge], events[len(events)-DigestEdge:]
	var kept []M
	for _, raw := range middle {
		e := util.MapOr(raw)
		if e["kind"] == "text" || util.Truthy(e["is_error"]) {
			kept = append(kept, e)
			if len(kept) == 80 {
				break
			}
		}
	}
	marker := M{"kind": "omitted", "first": util.MapOr(middle[0])["eid"], "last": util.MapOr(middle[len(middle)-1])["eid"],
		"count": int64(len(middle) - len(kept)), "kept": int64(len(kept))}
	sort.SliceStable(kept, func(i, j int) bool { return tsKey(kept[i]) < tsKey(kept[j]) })
	out := append([]any{}, head...)
	out = append(out, marker)
	for _, k := range kept {
		out = append(out, k)
	}
	return append(out, tail...)
}

// AskCosts attaches measured costs to each ask (in place) from the events it cites.
func AskCosts(asks []M, tel M) []M {
	byID := map[string]M{}
	for _, e := range util.Maps(tel["events"]) {
		byID[util.Str(e["eid"])] = e
	}
	usage := map[string]float64{}
	for _, u := range util.Maps(tel["usage"]) {
		f, _ := util.Num(u["output_tokens"])
		usage[util.Str(u["id"])] = f
	}
	refKey := func(r any) (string, bool) {
		s, ok := r.(string)
		return s, ok
	}
	owners := map[string][]string{}
	for _, ask := range asks {
		for _, r := range util.List(ask["event_refs"]) {
			if k, ok := refKey(r); ok {
				owners[k] = append(owners[k], util.Str(ask["id"]))
			}
		}
	}
	for _, ask := range asks {
		var refs []string
		invalid := []any{}
		for _, r := range util.List(ask["event_refs"]) {
			if k, ok := refKey(r); ok && byID[k] != nil {
				refs = append(refs, k)
			} else {
				invalid = append(invalid, r)
			}
		}
		ask["invalid_refs"] = invalid
		var evs, tools []M
		for _, r := range refs {
			evs = append(evs, byID[r])
		}
		var starts, ends []time.Time
		mids := map[string]bool{}
		for _, e := range evs {
			if e["kind"] == "tool" {
				tools = append(tools, e)
			}
			if util.Truthy(e["at"]) {
				if t, ok := parse(e["at"]); ok {
					starts = append(starts, t)
				}
				endv := e["at"]
				if util.Truthy(e["end"]) {
					endv = e["end"]
				}
				if t, ok := parse(endv); ok {
					ends = append(ends, t)
				}
			}
			if util.Truthy(e["message_id"]) {
				mids[util.Str(e["message_id"])] = true
			}
		}
		var tokens any
		if len(usage) > 0 && len(mids) > 0 {
			sum := 0.0
			for m := range mids {
				sum += usage[m]
			}
			if sum == math.Trunc(sum) {
				tokens = int64(sum)
			} else {
				tokens = sum
			}
		}
		failed := int64(0)
		var toolSeconds any
		if len(tools) > 0 {
			sum := 0.0
			for _, e := range tools {
				if util.Truthy(e["is_error"]) {
					failed++
				}
				if f, ok := util.Num(e["duration_s"]); ok {
					sum += f
				}
			}
			toolSeconds = pyRound(sum, 1)
		}
		var span any
		if len(starts) > 0 && len(ends) > 0 {
			lo, hi := starts[0], ends[0]
			for _, t := range starts {
				if t.Before(lo) {
					lo = t
				}
			}
			for _, t := range ends {
				if t.After(hi) {
					hi = t
				}
			}
			span = pyRound(float64(hi.Sub(lo).Microseconds())/1e6, 1)
		}
		shared := []string{}
		for _, r := range refs {
			if len(owners[r]) > 1 {
				shared = append(shared, r)
			}
		}
		sort.Strings(shared)
		ask["measured"] = M{
			"events":            int64(len(evs)),
			"tool_calls":        int64(len(tools)),
			"failed_tool_calls": failed,
			"tool_seconds":      toolSeconds,
			"span_seconds":      span,
			"output_tokens":     tokens,
			"shared_events":     util.List(shared),
			"basis":             AskCostBasis,
		}
	}
	return asks
}

type stamp struct {
	t     time.Time
	aware bool
}

func window(start, stop any, eventsAt []any) (string, string) {
	var times []stamp
	for _, v := range eventsAt {
		if s, ok := v.(string); ok && s != "" {
			if t, aware, err := measure.ParseISO(s); err == nil {
				times = append(times, stamp{t, aware})
			}
		}
	}
	pick := func(anchor any, less bool) (stamp, bool) {
		var cands []stamp
		if s, ok := anchor.(string); ok && s != "" {
			t, aware, err := measure.ParseISO(s)
			if err != nil {
				return stamp{}, false
			}
			cands = append(cands, stamp{t, aware})
		}
		if len(cands) == 0 && len(times) == 0 {
			return stamp{}, false
		}
		cands = append(cands, times...)
		best := cands[0]
		for _, c := range cands[1:] {
			if (less && c.t.Before(best.t)) || (!less && c.t.After(best.t)) {
				best = c
			}
		}
		return best, true
	}
	s, ok1 := pick(start, true)
	e, ok2 := pick(stop, false)
	if !ok1 || !ok2 {
		return "", ""
	}
	e.t = e.t.Add(time.Millisecond)
	f := func(x stamp) string { return strings.ReplaceAll(measure.ISOFormat(x.t, x.aware), "+00:00", "Z") }
	return f(s), f(e)
}

// MeasureRun writes measure.manifest.json, measure.events.jsonl and
// measurements.json for a run directory and returns the measurements record.
// A measurement that does not reconcile is recorded as measurement_error;
// an unreadable telemetry file is returned as an error.
func MeasureRun(sp, cell M, runDir string, state M) (M, error) {
	sessions := []any{}
	events := []any{}
	add := func(sid, scope string, proc M, tel M, reportedTokens any) {
		if len(proc) == 0 || !util.Truthy(proc["started_at"]) {
			return
		}
		if tel == nil {
			tel = M{}
		}
		var toolEvs []M
		for _, e := range util.Maps(tel["events"]) {
			if e["kind"] == "tool" && util.Truthy(e["at"]) {
				toolEvs = append(toolEvs, e)
			}
		}
		var usage []M
		for _, u := range util.Maps(tel["usage"]) {
			if util.Truthy(u["at"]) {
				usage = append(usage, u)
			}
		}
		var ats []any
		for _, e := range toolEvs {
			ats = append(ats, e["at"])
		}
		for _, u := range usage {
			ats = append(ats, u["at"])
		}
		start, end := window(proc["started_at"], proc["stopped_at"], ats)
		if start == "" {
			return
		}
		session := M{"id": sid, "start": start, "end": end,
			"phases": []any{M{"id": sid, "start": start, "end": end, "scope": scope}}}
		var usageStatus any = "unavailable"
		if v, ok := tel["usage_status"]; ok {
			usageStatus = v
		}
		if reportedTokens != nil && len(usage) == 0 {
			usage = []M{{"id": sid + "-total", "at": start, "output_tokens": reportedTokens}}
			usageStatus = "partial"
		}
		if usageStatus == "unavailable" {
			usage = nil
		}
		session["usage_status"] = usageStatus
		if usageStatus == "complete" {
			session["expected_output_tokens"] = tel["expected_output_tokens"]
		}
		var toolStatus any = "unavailable"
		if len(toolEvs) > 0 {
			if v, ok := tel["tool_status"]; ok {
				toolStatus = v
			}
		}
		session["tool_status"] = toolStatus
		if toolStatus == "complete" {
			session["expected_tool_calls"] = tel["expected_tool_calls"]
		}
		sessions = append(sessions, session)
		for _, u := range usage {
			n, _ := util.Num(u["output_tokens"])
			events = append(events, M{"id": "usage:" + util.Str(u["id"]), "session_id": sid, "at": u["at"],
				"kind": "usage", "output_tokens": int64(n)})
		}
		if toolStatus != "unavailable" {
			for _, e := range toolEvs {
				id := util.Or(e["tool_use_id"], e["eid"])
				events = append(events, M{"id": "tool:" + util.Str(id), "session_id": sid, "at": e["at"], "kind": "tool_call"})
			}
		}
	}
	readTel := func(name string, def any) (M, error) {
		v, err := util.ReadJSON(filepath.Join(runDir, name), def)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return util.MapOr(v), nil
	}
	execTel, err := readTel("telemetry.execute.json", M{})
	if err != nil {
		return nil, err
	}
	add("execute", "task", util.Map(state["execute"]), execTel, nil)
	verify, err := readTel("verify.json", M{})
	if err != nil {
		return nil, err
	}
	if w := util.Map(verify["window"]); util.Truthy(verify["window"]) {
		add("verify", "verification", M{"started_at": w["start"], "stopped_at": w["end"]}, nil, nil)
	}
	narr := util.MapOr(state["narrate"])
	if util.Truthy(narr["reconstruction"]) {
		add("narrate", "report", narr, nil, narr["output_tokens"])
	} else {
		narrTel, err := readTel("telemetry.narrate.json", M{})
		if err != nil {
			return nil, err
		}
		add("narrate", "report", narr, narrTel, nil)
	}
	ext := util.MapOr(state["extract"])
	add("extract", "report", ext, nil, ext["output_tokens"])

	manifest := M{"sessions": sessions}
	if err := util.WriteJSON(filepath.Join(runDir, "measure.manifest.json"), manifest); err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, e := range events {
		b.WriteString(util.PyJSON(e))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(runDir, "measure.events.jsonl"), []byte(b.String()), 0o644); err != nil {
		return nil, err
	}
	var result, measurementError any
	if r, err := measure.Measure(manifest, events); err != nil {
		var me *measure.Error
		if !errors.As(err, &me) {
			return nil, err
		}
		measurementError = me.Msg
	} else {
		result = r
	}
	tel, err := readTel("telemetry.execute.json", M{"events": []any{}})
	if err != nil {
		return nil, err
	}
	out := M{"schema_version": int64(1), "phase_measurements": result, "measurement_error": measurementError,
		"task_summary": TaskSummary(state, tel), "note": MeasurementsNote}
	if err := util.WriteJSON(filepath.Join(runDir, "measurements.json"), out); err != nil {
		return nil, err
	}
	return out, nil
}
