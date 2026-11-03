package export

// Ports skills/ajx/tests/test_export.py ExportRunTest (the CLI and status
// --tail tests belong to the CLI package). Synthetic run directories only.

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/util"
)

// Unnamed attempt directories look like secrets.token_hex(5); these are invented.
const (
	hexWS, hexHome, hexCfg, hexCache, hexSibling = "8b41708955", "1f2e3d4c5b", "a0b1c2d3e4", "f9e8d7c6b5", "9c1e0f2a3b"
)

type layoutT struct {
	home, root string
	paths      map[string]string
}

// layout is a coordinator home with the trial's workspace_root inside it;
// config and cache nested in the agent home so longest-first ordering is exercised.
func layout(tmp string) layoutT {
	home := filepath.Join(tmp, "home", "me")
	root := filepath.Join(home, "ajx", "work")
	agentHome := filepath.Join(root, hexHome)
	return layoutT{home, root, map[string]string{"workspace": filepath.Join(root, hexWS), "home_dir": agentHome,
		"config_dir": filepath.Join(agentHome, hexCfg), "cache_dir": filepath.Join(agentHome, hexCache)}}
}

func reportHTML(l layoutT) string {
	p := l.paths
	encWS := strings.ReplaceAll(p["workspace"], "/", "-")
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>prod: product asks</title></head><body>
<aside class="rail"><a class="brand" href="../../index.html">AJX<small>Agent Journey Experience</small></a>
<nav aria-label="Report views"><a href="report.html" aria-current="page">Product asks</a><a href="pretty-journey.html">Agent journey</a><a href="../../index.html">Compare runs</a><a href="asks.md">Markdown asks</a><a href="run.json">Run data</a></nav></aside>
<main id="content">
<p>Workspace ` + p["workspace"] + ` with transcript ` + l.home + `/.claude/projects/` + encWS + `/sid.jsonl, settings in ` + p["config_dir"] + `/settings.json, caches under ` + p["cache_dir"] + `/npm, agent home ` + p["home_dir"] + `.</p>
<p>A stale sibling attempt ` + l.root + `/` + hexSibling + ` was visible; the trial lives in ` + l.home + `/ajx/trials/demo.</p>
<div class="links"><a class="mono" href="pretty-journey.html#E-003">E-003</a><a href="#ASK-001-detail">Inspect evidence and verification</a></div>
<ul><li>Harness: fake (1.0)</li><li><a href="verify.json">Inspect verification checks</a> after the run</li><li>Stop: completed</li></ul>
<div class="links"><a href="environment.json">Environment evidence</a> <a href="agent-configuration.json#top">Agent configuration manifest</a><a href="run.json">Complete run evidence</a></div>
<a href="pretty-journey.html">Open journey map and event table</a>
</main></body></html>
`
}

func journeyHTML(l layoutT) string {
	p := l.paths
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>prod: agent journey</title></head><body>
<aside class="rail"><a class="brand" href="../../index.html">AJX<small>Agent Journey Experience</small></a>
<nav aria-label="Report views"><a href="report.html">Product asks</a><a href="pretty-journey.html" aria-current="page">Agent journey</a><a href="../../index.html">Compare runs</a><a href="asks.md">Markdown asks</a><a href="run.json">Run data</a></nav></aside>
<main id="content"><table><tr id="E-003"><td><a class="mono" href="#E-003">E-003</a></td><td>cd ` + p["workspace"] + ` && ls ` + p["config_dir"] + `</td><td><a class="mono" href="report.html#ASK-001">ASK-001</a></td></tr></table>
<p>Journey written from ` + p["workspace"] + `.</p></main></body></html>
`
}

func asksMD(l layoutT) string {
	p := l.paths
	return "# Asks: prod (f-r1)\n\nHarness fake, model m. Task outcome declared by the agent is recorded in run.json; verified outcome: **succeeded**.\n\n" +
		"- **How observed:** ran `ls " + p["workspace"] + "` and read " + p["config_dir"] + "/settings.json\n" +
		"- [Environment evidence](environment.json)\n- [Agent configuration manifest](agent-configuration.json)\n\n" +
		"See [E-003](pretty-journey.html#E-003) and [Complete run evidence](run.json#top).\n"
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func makeRunDir(t *testing.T, out string, l layoutT, withPaths bool) string {
	t.Helper()
	d := filepath.Join(out, "runs", "f-r1")
	state := util.M{"stages": util.M{"prepare": util.M{"status": "done"}, "execute": util.M{"status": "done"}}}
	if withPaths {
		paths := util.M{}
		for k, v := range l.paths {
			paths[k] = v
		}
		state["paths"] = paths
	}
	if err := pyfmt.WriteJSON(filepath.Join(d, "state.json"), state); err != nil {
		t.Fatal(err)
	}
	if err := pyfmt.WriteJSON(filepath.Join(d, "run.json"), util.M{"run_id": "f-r1", "agent": util.M{"harness_name": "fake-tail"}}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(d, "report.html"), reportHTML(l))
	writeFile(t, filepath.Join(d, "pretty-journey.html"), journeyHTML(l))
	writeFile(t, filepath.Join(d, "asks.md"), asksMD(l))
	return d
}

func texts(t *testing.T, dest string) map[string]string {
	out := map[string]string{}
	for _, n := range []string{"report.html", "journey.html", "asks.md"} {
		data, err := os.ReadFile(filepath.Join(dest, n))
		if err != nil {
			t.Fatal(err)
		}
		out[n] = string(data)
	}
	return out
}

func doExport(t *testing.T, withPaths bool) (layoutT, string, util.M) {
	t.Helper()
	tmp := t.TempDir()
	l := layout(tmp)
	runDir := makeRunDir(t, filepath.Join(tmp, "out"), l, withPaths)
	dest := filepath.Join(tmp, "share", "f-r1")
	summary, err := ExportRun(runDir, dest, ExportOpts{Home: l.home})
	if err != nil {
		t.Fatal(err)
	}
	return l, dest, summary
}

func mustContain(t *testing.T, text, want, name string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Errorf("%s: missing %q", name, want)
	}
}

func mustNotContain(t *testing.T, text, bad, name string) {
	t.Helper()
	if strings.Contains(text, bad) {
		t.Errorf("%s: unexpected %q", name, bad)
	}
}

func TestBundleIsExactlyThreeFilesWithTheJourneyRenamed(t *testing.T) {
	_, dest, summary := doExport(t, true)
	entries, _ := os.ReadDir(dest)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "asks.md,journey.html,report.html" {
		t.Fatalf("files %v", names)
	}
	if got := util.Strings(summary["files"]); strings.Join(got, ",") != "report.html,journey.html,asks.md" {
		t.Fatalf("summary files %v", got)
	}
	if summary["dest"] != dest || len(util.List(summary["warnings"])) != 0 {
		t.Fatalf("summary %v", summary)
	}
	for _, n := range names {
		st, _ := os.Stat(filepath.Join(dest, n))
		if st.Mode().Perm() != 0o644 {
			t.Errorf("%s mode %v", n, st.Mode().Perm())
		}
	}
}

func TestWorkerDirectoriesBecomeLabelsLongestFirst(t *testing.T) {
	l, dest, _ := doExport(t, true)
	all := texts(t, dest)
	attempt := regexp.MustCompile(regexp.QuoteMeta(l.root) + `/[0-9a-f]{6,}`)
	for name, text := range all {
		for _, raw := range l.paths {
			mustNotContain(t, text, raw, name)
			mustNotContain(t, text, strings.ReplaceAll(raw, "/", "-"), name)
		}
		mustNotContain(t, text, l.home, name)
		mustNotContain(t, text, hexSibling, name)
		mustNotContain(t, text, hexCfg, name) // nested config dir got its own label, not /agent-home/<hex>
		if attempt.MatchString(text) {
			t.Errorf("%s: attempt path left", name)
		}
	}
	report := all["report.html"]
	mustContain(t, report, "Workspace /workspace with transcript ~/.claude/projects/-workspace/sid.jsonl", "report")
	mustContain(t, report, "settings in /agent-home/.config/harness/settings.json", "report")
	mustContain(t, report, "caches under /agent-home/.cache/npm, agent home /agent-home.", "report")
	mustContain(t, report, "A stale sibling attempt /agent-home/attempt was visible; the trial lives in ~/ajx/trials/demo.", "report")
	mustContain(t, all["journey.html"], "cd /workspace && ls /agent-home/.config/harness", "journey")
	mustContain(t, all["asks.md"], "ran `ls /workspace` and read /agent-home/.config/harness/settings.json", "asks")
}

func TestLinksOutsideTheBundleBecomeTextAndJourneyLinksFollowTheRename(t *testing.T) {
	_, dest, _ := doExport(t, true)
	all := texts(t, dest)
	for _, name := range []string{"report.html", "journey.html"} {
		text := all[name]
		for _, outside := range Outside {
			mustNotContain(t, text, `href="`+outside, name)
		}
		mustNotContain(t, text, `href="pretty-journey.html`, name)
		mustContain(t, text, "Run data", name) // link text survives the unwrap
		mustContain(t, text, "Compare runs", name)
	}
	report := all["report.html"]
	for _, want := range []string{`<a class="mono" href="journey.html#E-003">E-003</a>`, `<a href="journey.html">Agent journey</a>`,
		`<a href="journey.html">Open journey map and event table</a>`, `<a href="report.html" aria-current="page">Product asks</a>`,
		`<a href="#ASK-001-detail">Inspect evidence and verification</a>`,
		`<div class="links">Environment evidence Agent configuration manifestComplete run evidence</div>`,
		"<ul><li>Harness: fake (1.0)</li><li>Stop: completed</li></ul>"} {
		mustContain(t, report, want, "report")
	}
	// the list item that only existed to carry a verify.json link is gone; its neighbours stay
	mustNotContain(t, report, "Inspect verification checks", "report")
	mustNotContain(t, report, "after the run", "report")
	journey := all["journey.html"]
	for _, want := range []string{`<a href="journey.html" aria-current="page">Agent journey</a>`,
		`<a class="mono" href="report.html#ASK-001">ASK-001</a>`, `<a class="mono" href="#E-003">E-003</a>`} {
		mustContain(t, journey, want, "journey")
	}
}

func TestBrandLinkKeepsItsClassAndLosesItsHref(t *testing.T) {
	_, dest, _ := doExport(t, true)
	for _, name := range []string{"report.html", "journey.html"} {
		text := texts(t, dest)[name]
		mustContain(t, text, `<a class="brand">AJX<small>Agent Journey Experience</small></a>`, name)
		mustNotContain(t, text, `<a class="brand" href=`, name)
	}
}

func TestMarkdownLinksFollowTheSameRules(t *testing.T) {
	_, dest, _ := doExport(t, true)
	asks := texts(t, dest)["asks.md"]
	mustContain(t, asks, "See [E-003](journey.html#E-003) and Complete run evidence.", "asks")
	mustNotContain(t, asks, "](run.json", "asks")
	mustNotContain(t, asks, "pretty-journey", "asks")
	mustNotContain(t, asks, "Environment evidence", "asks") // bullet that only carried a link
	mustNotContain(t, asks, "Agent configuration manifest", "asks")
	mustContain(t, asks, "recorded in run.json;", "asks") // prose mentions are not links
}

func TestSummaryCountsAreSane(t *testing.T) {
	_, _, summary := doExport(t, true)
	counts := util.MapOr(summary["replacements"])
	want := []string{"/workspace", "/agent-home", "/agent-home/.config/harness", "/agent-home/.cache",
		"/agent-home/attempt", "/agent-home/attempts", "~"}
	if got := util.SortedKeys(counts); strings.Join(got, ",") != strings.Join(util.Uniq(want), ",") {
		t.Fatalf("keys %v", got)
	}
	expect := map[string]int{"/workspace": 5, "/agent-home/.config/harness": 3, "/agent-home/.cache": 1,
		"/agent-home": 1, "/agent-home/attempt": 1, "~": 2}
	for k, n := range expect {
		if util.Int(counts[k], -1) != n {
			t.Errorf("%s: %v, want %d", k, counts[k], n)
		}
	}
	if order := util.Strings(summary["replacement_order"]); len(order) != len(counts) {
		t.Errorf("replacement_order %v", order)
	}
}

func TestMissingSourceFailsBeforeAnythingIsWritten(t *testing.T) {
	tmp := t.TempDir()
	l := layout(tmp)
	runDir := makeRunDir(t, filepath.Join(tmp, "out"), l, true)
	os.Remove(filepath.Join(runDir, "asks.md"))
	dest := filepath.Join(tmp, "share")
	_, err := ExportRun(runDir, dest, ExportOpts{Home: l.home})
	var ee *Error
	if !errors.As(err, &ee) || !strings.Contains(err.Error(), "asks.md") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("dest was created")
	}
	if _, err := ExportRun(filepath.Join(tmp, "nope"), dest, ExportOpts{Home: l.home}); !errors.As(err, &ee) {
		t.Fatalf("missing run dir: %v", err)
	}
}

func TestRunWithoutRecordedPathsExportsWithAWarning(t *testing.T) {
	l, dest, summary := doExport(t, false)
	warnings := util.Strings(summary["warnings"])
	if len(warnings) == 0 || !strings.Contains(warnings[0], "paths") {
		t.Fatalf("warnings %v", warnings)
	}
	report := texts(t, dest)["report.html"]
	mustNotContain(t, report, l.home, "report") // the home rule still applies
	mustContain(t, report, "~/ajx/work/"+hexWS, "report")
	if keys := util.SortedKeys(util.MapOr(summary["replacements"])); strings.Join(keys, ",") != "~" {
		t.Fatalf("keys %v", keys)
	}
}

func TestHomeDefaultsToTheCoordinatorsHome(t *testing.T) {
	tmp := t.TempDir()
	l := layout(tmp)
	runDir := makeRunDir(t, filepath.Join(tmp, "out"), l, true)
	realHome := util.Home()
	writeFile(t, filepath.Join(runDir, "asks.md"), "trial dir "+realHome+"/ajx/trials/demo and "+l.paths["workspace"]+"\n")
	summary, err := ExportRun(runDir, filepath.Join(tmp, "share"), ExportOpts{})
	if err != nil {
		t.Fatal(err)
	}
	asks, _ := os.ReadFile(filepath.Join(tmp, "share", "asks.md"))
	if string(asks) != "trial dir ~/ajx/trials/demo and /workspace\n" {
		t.Fatalf("asks %q", asks)
	}
	if util.Int(util.MapOr(summary["replacements"])["~"], 0) < 1 {
		t.Fatal("no home replacement")
	}
}

func TestBoundariesKeepLongerNamesIntact(t *testing.T) {
	rules := ReplacementRules(util.M{"workspace": "/w/root/8b41708955"}, "/srv/people/me", nil)
	text := "/srv/people/me/x /srv/people/meredith/x /srv/people/me.bak /srv/people/me. -srv-people-me-y -srv-people-meredith " +
		"/w/root/8b41708955/sub /w/root/9c1e0f2a3b/f /w/root/cafe /w/root/8b41708955xyz"
	got := RewritePaths(text, rules, nil)
	// The bare root is relabeled too, so a non-attempt child or a truncated attempt name keeps no layout.
	want := "~/x /srv/people/meredith/x /srv/people/me.bak ~. ~-y -srv-people-meredith " +
		"/workspace/sub /agent-home/attempt/f /agent-home/attempts/cafe " +
		"/agent-home/attempts/8b41708955xyz"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestTemperedListItemAndAnchorMatching(t *testing.T) {
	cases := map[string]string{
		// an item with an outside link is dropped even with other markup; a nested <li> stops it
		`<ul><li class="x"><b>a</b> <a id="q" href="run.json#frag">r</a> tail</li><li>keep</li></ul>`: `<ul><li>keep</li></ul>`,
		`<ul><li>before <li>inner <a href="verify.json">v</a></li></ul>`:                              `<ul><li>before </ul>`,
		`<a class="x" href="other.html" data-y href="run.json">t</a>`:                                 `t`,
		`<a href="run.jsonx">no</a><a href="environment.json">env</a>`:                                `<a href="run.jsonx">no</a>env`,
		`<abbr href="run.json">keep</abbr>`:                                                           `<abbr href="run.json">keep</abbr>`,
		`<a xhref="run.json">keep</a>`:                                                                `<a xhref="run.json">keep</a>`,
		`<li><a href="run.json">a</a><li><a href="x">y</a></li>`:                                      `<li>a<li><a href="x">y</a></li>`,
		`<lix><a href="run.json">a</a></li>`:                                                          `<lix>a</li>`,
		`<a href="run.json#a>b" class=z>T</a>`:                                                        `T`,
		`<li>x<a href="run.json">a</a> <a href="../../index.html">i</a></li>`:                         ``,
		"<a\nhref=\"run.json\">multi\nline</a>":                                                       "multi\nline",
	}
	for in, want := range cases {
		if got := RewriteHTMLLinks(in); got != want {
			t.Errorf("%q:\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestEncodedForms(t *testing.T) {
	if got := EncodedForms("/a/b.c d"); strings.Join(got, "|") != "-a-b.c d|-a-b-c-d" {
		t.Fatal(got)
	}
	if got := EncodedForms("/agent-home/attempt"); len(got) != 1 {
		t.Fatal(got)
	}
}
