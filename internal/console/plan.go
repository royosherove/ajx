package console

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/royosherove/ajx/internal/planedit"
	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/reportui"
	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/util"
)

// ---------------------------------------------------------------- the Plan tab's editor

// writeTrialHook is writeTrial; tests stand in a failing one.
var writeTrialHook = writeTrial

// validated is (spec, "") when AJX can load this trial text, else (nil, why).
// Loaded beside the real file, so relative paths resolve the same way; prompt
// (text) stands in for the task prompt file when the edit changes it.
func validated(trial, text string, prompt *string) (M, string) {
	if _, err := planedit.ParseTOML(text); err != nil {
		return nil, "not valid TOML: " + err.Error()
	}
	dir, tag := filepath.Dir(trial), "check-"+tmpTag()
	tmp := filepath.Join(dir, "."+filepath.Base(trial)+"."+tag+".tmp")
	defer os.Remove(tmp)
	if prompt != nil {
		tmpPrompt := filepath.Join(dir, ".task-prompt."+tag+".tmp")
		defer os.Remove(tmpPrompt)
		if err := os.WriteFile(tmpPrompt, []byte(*prompt), 0o644); err != nil {
			return nil, err.Error()
		}
		t, err := planedit.SetValue(text, "task", "prompt_file", filepath.Base(tmpPrompt), 0)
		if err != nil {
			return nil, err.Error()
		}
		text = t
	}
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return nil, err.Error()
	}
	sp, err := specLoad(tmp)
	if err != nil {
		return nil, err.Error()
	}
	return sp, ""
}

var lockedInputs = [][2]string{{"verify", "the success checks"}, {"setup", "the setup commands"},
	{"preflight", "the preflight commands"}, {"teardown", "the cleanup commands"}}

func orEmptyList(v any) any {
	if util.Truthy(v) {
		return v
	}
	return []any{}
}

// folderLock is why these settings cannot be used in the output folder they
// point at, or nil. The folder's own record decides (as `ajx run` does); for
// the current folder the checks and task inputs are kept too.
func folderLock(trial string, newSpec M, beforeText, text string, promptBytes []byte) []any {
	out := util.Str(util.Get(newSpec, "trial", "output_dir"))
	if !anyStateFile(out) {
		return nil
	}
	meta := readMap(filepath.Join(out, "matrix-plan.json"))
	current, _ := load(trial)
	same := current != nil && util.Str(util.Get(current, "trial", "output_dir")) == out
	profilesOf := func(sp M) M {
		return M{"schema_version": 1, "environments": getOr(sp, "environments", M{}),
			"agent_configurations": getOr(sp, "agent_configurations", M{})}
	}
	promptSha := meta["task_prompt_sha256"]
	recorded := util.Map(meta["normalized_profiles"])
	contract := util.Map(meta["cells_contract"])
	if same {
		promptSha = util.Or(promptSha, util.Get(current, "task", "prompt_sha256"))
		if len(recorded) == 0 {
			recorded = profilesOf(current)
		}
		if len(contract) == 0 {
			contract = runner.CellsContract(current)
		}
	}
	var found []string
	if util.Truthy(promptSha) && pstr(promptSha) != sha256Hex(promptBytes) {
		found = append(found, "the task prompt")
	}
	if len(recorded) > 0 && runner.ProfileContract(recorded) != runner.ProfileContract(profilesOf(newSpec)) {
		found = append(found, "the environments or agent configurations")
	}
	if len(contract) > 0 {
		found = append(found, runner.ContractChanges(contract, runner.CellsContract(newSpec))...)
	}
	if inputs := util.Map(meta["task_inputs"]); len(inputs) > 0 { // recorded by `ajx run`: comparable for any folder
		now := jsonRoundTrip(runner.TaskInputs(newSpec))
		for _, k := range lockedInputs {
			if !runner.PyEqual(orEmptyList(inputs[k[0]]), orEmptyList(now[k[0]])) {
				found = append(found, k[1])
			}
		}
		for _, key := range runner.TaskInputKeys {
			if !runner.PyEqual(inputs[key], now[key]) {
				found = append(found, "task "+strings.ReplaceAll(key, "_", " "))
			}
		}
	} else if same { // folders made before task_inputs were recorded: compare with the current file
		old, _ := parseTOML(beforeText)
		new, _ := parseTOML(text)
		for _, k := range lockedInputs {
			if !runner.PyEqual(orEmptyList(old[k[0]]), orEmptyList(new[k[0]])) {
				found = append(found, k[1])
			}
		}
		for _, key := range runner.TaskInputKeys {
			if !runner.PyEqual(util.MapOr(old["task"])[key], util.MapOr(new["task"])[key]) {
				found = append(found, "task "+strings.ReplaceAll(key, "_", " "))
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	if len(found) > 4 {
		found = found[:4]
	}
	return []any{"Locked for this output folder: its attempts used other settings (" + strings.Join(found, "; ") +
		"). Switch to a fresh output folder to change them."}
}

// jsonRoundTrip is json.loads(json.dumps(v)): the values a JSON record holds.
func jsonRoundTrip(v M) M {
	data, err := util.MarshalIndent(v)
	if err != nil {
		return v
	}
	out, err := util.DecodeJSON(data)
	if err != nil {
		return v
	}
	return util.MapOr(out)
}

func withNewline(t string) string {
	if strings.HasSuffix(t, "\n") {
		return t
	}
	return t + "\n"
}

func lines(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

// savePlan previews (dryRun) or saves the Plan tab's model: trial.toml section
// by section, and the prompt. An output folder that holds attempts takes only
// more attempts, new workers, or a switch of folder. model is the page's
// model as decoded JSON (planedit.DecodeJSON keeps its key order).
func savePlan(trial string, model any, baseSha, promptSha any, actor string, dryRun bool) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	if !dryRun {
		if _, err := ensureIdle(trial, nil); err != nil {
			return nil, err
		}
	}
	beforeBytes, err := os.ReadFile(trial)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(beforeBytes) {
		return nil, fmt.Errorf("'utf-8' codec can't decode %s", trial)
	}
	beforeText := strings.ReplaceAll(string(beforeBytes), "\r\n", "\n") // edited and written with LF
	current, err := planedit.ReadModelText(trial, beforeText)
	var rendered *planedit.Rendered
	var changeList []planedit.Change
	if err == nil {
		rendered, err = planedit.Render(trial, model, planedit.RenderOpts{Text: &beforeText, Current: current})
	}
	if err == nil {
		changeList, err = planedit.Changes(current, model)
	}
	if err != nil {
		var pe *planedit.Error
		if !errors.As(err, &pe) {
			return nil, err
		}
		if dryRun {
			return M{"changes": []any{}, "diff": []any{}, "problems": []any{pe.Msg}, "added_auth": []any{}, "text": beforeText}, nil
		}
		return nil, Errorf("%s", pe.Msg)
	}
	changes := make([]any, len(changeList))
	var texts []any
	for i, c := range changeList {
		changes[i] = M{"node": c.Node, "text": c.Text}
		if i < 40 {
			texts = append(texts, c.Text)
		}
	}
	task := current.Get("task").(*planedit.Dict)
	promptName := "task-prompt.md"
	if pf, ok := task.Get("prompt_file").(string); ok && pf != "" {
		promptName = pf
	}
	promptFile := promptName
	if !filepath.IsAbs(promptFile) {
		promptFile = filepath.Join(filepath.Dir(trial), promptName)
	}
	currentPrompt := pstr(util.Or(task.Get("prompt"), ""))
	text, prompt := rendered.Text, rendered.Prompt
	promptChanged := prompt != nil && withNewline(*prompt) != withNewline(currentPrompt)
	var body *string
	if promptChanged {
		b := withNewline(*prompt) // the prompt is saved with one final newline
		body = &b
	}
	problems := []any{}
	if prompt != nil && pyfmt.Strip(*prompt) == "" {
		problems = append(problems, "the task prompt cannot be empty")
	}
	// Always validated: an unchanged trial.toml that no longer loads cannot show whether its output
	// folder would accept the edit, so a prompt-only edit is refused until the file loads again.
	newSpec, why := validated(trial, text, body)
	if why != "" {
		problems = append(problems, why)
	}
	if newSpec != nil {
		var promptBytes []byte
		if body != nil {
			promptBytes = []byte(*body)
		} else if isFile(promptFile) {
			promptBytes, _ = os.ReadFile(promptFile)
		}
		problems = append(problems, folderLock(trial, newSpec, beforeText, text, promptBytes)...)
	}
	diff := lines(planedit.DiffLines(beforeText, text, "trial.toml"))
	if promptChanged {
		diff = append(diff, lines(planedit.DiffLines(currentPrompt, *prompt, promptName))...)
	}
	if dryRun {
		added := lines(rendered.AddedAuth)
		return M{"changes": changes, "diff": diff, "problems": problems, "added_auth": added, "text": text}, nil
	}
	if len(problems) > 0 {
		return nil, Errorf("%s", py(problems[0]))
	}
	if !util.Truthy(baseSha) {
		return nil, Errorf("the save did not say which version of trial.toml it edits; reload the page")
	}
	base, ok := baseSha.(string)
	if !ok || base != sha256Hex(beforeBytes) {
		return nil, Conflict("trial.toml changed on disk (by your agent or an editor). Reload to see the new version.")
	}
	tmp := ""
	if promptChanged {
		var onDisk []byte
		if isFile(promptFile) {
			onDisk, _ = os.ReadFile(promptFile)
		}
		if p, ok := promptSha.(string); !ok || p == "" || p != sha256Hex(onDisk) {
			return nil, Conflict("the task prompt changed on disk (by your agent or an editor). Reload to see it.")
		}
		tmp = filepath.Join(filepath.Dir(promptFile), fmt.Sprintf(".%s.console-%s.tmp", filepath.Base(promptFile), tmpTag()))
		if err := os.WriteFile(tmp, []byte(*body), 0o644); err != nil {
			return nil, err
		}
	}
	if text == beforeText && !promptChanged {
		return M{"sha256": baseSha, "changes": changes}, nil // nothing to write or to journal
	}
	var oldPrompt []byte
	hadPrompt := false
	if promptChanged && isFile(promptFile) {
		oldPrompt, _ = os.ReadFile(promptFile)
		hadPrompt = true
	}
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()
	replaced := false
	if tmp != "" { // the prompt first, so the journal never claims a change that failed
		if err := os.Rename(tmp, promptFile); err != nil {
			return nil, err
		}
		tmp, replaced = "", true
	}
	sha := base
	if text != beforeText {
		sha, err = writeTrialHook(trial, text, base, actor, "plan_changed", M{"changes": listOr(texts)})
	} else {
		record(trial, actor, "plan_changed", M{"changes": listOr(texts)})
	}
	if err != nil {
		if replaced { // the trial file was not written: put the old prompt back, all or nothing
			if hadPrompt {
				_ = os.WriteFile(promptFile, oldPrompt, 0o644)
			} else {
				os.Remove(promptFile)
			}
		}
		return nil, err
	}
	return M{"sha256": sha, "changes": changes}, nil
}

// ---------------------------------------------------------------- the worker's files and journey

var skipOutputs = map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true,
	".mypy_cache": true, ".pytest_cache": true}

// workspaceDir is the run's workspace: the archived copy, the live folder, or <run>/workspace.
func workspaceDir(d string, st M) string {
	for _, c := range []any{util.Get(st, "archived", "workspace"), util.Get(st, "paths", "workspace"), filepath.Join(d, "workspace")} {
		if s, ok := c.(string); ok && s != "" && isDir(s) {
			return s
		}
	}
	return ""
}

// outputs are the worker's files: what is in its workspace (names, sizes,
// times). Bounded, and never inside dependency or VCS folders, so it stays
// cheap on every poll.
// outputs is (files, truncated): the worker's files in its workspace, each
// marked starting_file when it is an unchanged copy of the trial's starting
// files (fixture). Bounded, never inside dependency or VCS folders.
func outputs(d string, st M, fixture string) ([]any, bool) {
	const limit, budget = 200, 4000
	root := workspaceDir(d, st)
	found := []any{}
	if root == "" {
		return found, false
	}
	seen, truncated := 0, false
	var walk func(dir string) bool
	walk = func(dir string) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		var dirs, files []string
		for _, e := range entries {
			isDirEntry := e.IsDir()
			if e.Type()&os.ModeSymlink != 0 {
				if st, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && st.IsDir() {
					isDirEntry = true
				}
			}
			if isDirEntry {
				if !skipOutputs[e.Name()] {
					dirs = append(dirs, e.Name())
				}
			} else {
				files = append(files, e.Name())
			}
		}
		for _, name := range files {
			seen++
			if seen > budget || len(found) >= limit {
				truncated = true
				return false
			}
			p := filepath.Join(dir, name)
			info, err := os.Lstat(p)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			rel, _ := filepath.Rel(root, p)
			t := info.ModTime()
			starting := fixture != "" && sameFile(p, info, filepath.Join(fixture, rel))
			found = append(found, M{"path": rel, "size": info.Size(),
				"mtime": float64(t.Unix()) + float64(t.Nanosecond())*1e-9, "starting_file": starting})
		}
		for _, name := range dirs {
			p := filepath.Join(dir, name)
			if lst, err := os.Lstat(p); err == nil && lst.Mode()&os.ModeSymlink != 0 {
				continue // os.walk does not follow links
			}
			if !walk(p) {
				return false
			}
		}
		return true
	}
	walk(root)
	return found, truncated
}

// sameFile reports whether a workspace file is still the starting file it was
// copied from (bytes compared up to 1 MB).
func sameFile(path string, info os.FileInfo, original string) bool {
	o, err := os.Lstat(original)
	if err != nil || !o.Mode().IsRegular() || o.Size() != info.Size() {
		return false
	}
	if info.Size() > 1<<20 {
		return true
	}
	a, err1 := os.ReadFile(path)
	b, err2 := os.ReadFile(original)
	return err1 == nil && err2 == nil && string(a) == string(b)
}

var spacesRe = regexp.MustCompile(`[` + pyfmt.WS + `]+`)

// JourneyHighlights are the labeled steps of the journey's chronological
// account: [{label, text, refs}] in order.
func JourneyHighlights(markdown string) []any {
	out := []any{}
	for _, block := range reportui.ChronologicalSteps(markdown) {
		plain := make([]string, len(block))
		for i, line := range block {
			plain[i] = pyfmt.Strip(line)
		}
		step := strings.Join(plain, " ")
		first := reportui.StepContent(step)
		var label any
		text := first
		for _, name := range reportui.LabelNames {
			icon := reportui.Labels[name][0]
			if reportui.StartsWithLabel(first, icon, name) {
				label = name
				prefix := regexp.MustCompile(`^(?:` + regexp.QuoteMeta(icon) + `[` + pyfmt.WS + `]*)?(?:\*\*)?` +
					regexp.QuoteMeta(name) + `(?:\*\*)?[.:]?[` + pyfmt.WS + `]*`)
				text = prefix.ReplaceAllString(first, "")
				break
			}
		}
		out = append(out, M{"label": label, "text": clip(spacesRe.ReplaceAllString(text, " "), 280), "refs": lines(reportui.EventReferences(step))})
	}
	return out
}

func journeyView(d string, st M) any {
	path := filepath.Join(d, "journey.md")
	if !isFile(path) {
		return nil
	}
	data, _ := os.ReadFile(path)
	steps := JourneyHighlights(strings.ToValidUTF8(string(data), "�"))
	counts := M{}
	for _, s := range steps {
		if l, ok := s.(M)["label"].(string); ok {
			counts[l] = util.Int(counts[l], 0) + 1
		}
	}
	total := int64(len(steps))
	if len(steps) > 30 {
		steps = steps[:30]
	}
	narrate := util.MapOr(st["narrate"])
	ntel := util.ReadMap(filepath.Join(d, "telemetry.narrate.json"))
	stops := util.Get(ntel, "harness_reported", "safety_stops")
	if !util.Truthy(stops) {
		stops = int64(0)
	}
	// Why the journey is a labeled reconstruction rather than the worker's own account.
	return M{"provenance": st["journey_provenance"], "steps": steps, "total_steps": total, "counts": counts,
		"reconstruction": util.Truthy(narrate["reconstruction"]), "self_narration_rejected": narrate["self_narration_rejected"],
		"safety_stops": stops}
}

// pyUnquote is urllib.parse.unquote: %XX escapes decoded as UTF-8 (invalid
// sequences replaced), anything else kept as it is.
func pyUnquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		b = append(b, s[i])
	}
	return strings.ToValidUTF8(string(b), "�")
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}
