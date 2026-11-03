package console

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
	"sync/atomic"
	"time"

	"github.com/royosherove/ajx/internal/planedit"
	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/util"
)

// ---------------------------------------------------------------- editing trial files

// SetValue sets (or with value nil, removes) key in [table], or in the
// index-th [[cells]], keeping the rest of the file as written (see planedit).
func SetValue(text, table, key string, value any, index int) (string, error) {
	out, err := planedit.SetValue(text, table, key, planedit.FromM(value), index)
	if err != nil {
		var pe *planedit.Error
		if errors.As(err, &pe) {
			return "", Errorf("%s", pe.Msg)
		}
		return "", err
	}
	return out, nil
}

// tmpSeq makes temporary file names unique per call (Python adds the thread id).
var tmpSeq atomic.Int64

func tmpTag() string { return fmt.Sprintf("%d-%d", os.Getpid(), tmpSeq.Add(1)) }

func writeTrial(trial, text, baseSha, actor, kind string, fields M) (string, error) {
	current, err := os.ReadFile(trial)
	if err != nil {
		return "", err
	}
	if baseSha != "" && baseSha != sha256Hex(current) {
		return "", Conflict("trial.toml changed on disk (by your agent or an editor). Reload to see the new version.")
	}
	if _, err := parseTOML(text); err != nil {
		return "", Errorf("not valid TOML: %s", err.Error())
	}
	tmp := filepath.Join(filepath.Dir(trial), fmt.Sprintf(".%s.console-%s.tmp", filepath.Base(trial), tmpTag()))
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return "", err
	}
	if _, err := specLoad(tmp); err != nil {
		os.Remove(tmp)
		return "", Errorf("not saved: %s", err.Error())
	}
	if err := os.Rename(tmp, trial); err != nil {
		os.Remove(tmp)
		return "", err
	}
	newSha := sha256Hex([]byte(text))
	all := M{"before": util.Clip(sha256Hex(current), 12), "after": util.Clip(newSha, 12)}
	for k, v := range fields {
		all[k] = v
	}
	record(trial, actor, kind, all)
	return newSha, nil
}

func anyStateFile(out string) bool {
	matches, _ := filepath.Glob(filepath.Join(out, "runs", "*", "state.json"))
	return len(matches) > 0
}

// contractGuard refuses a raw edit that would mix configurations in an output folder that holds attempts.
func contractGuard(trial, text string) error {
	sp, _ := load(trial)
	out := outputDir(trial, sp)
	meta := readMap(filepath.Join(out, "matrix-plan.json"))
	saved := util.Map(meta["cells_contract"])
	if len(saved) == 0 || !anyStateFile(out) {
		return nil
	}
	tmp := filepath.Join(filepath.Dir(trial), fmt.Sprintf(".%s.contract-%d.tmp", filepath.Base(trial), os.Getpid()))
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return nil
	}
	newSpec, err := specLoad(tmp)
	os.Remove(tmp)
	if err != nil || newSpec == nil {
		return nil // the normal validation reports it
	}
	if util.S(util.Get(newSpec, "trial", "output_dir")) != out {
		return nil // a new output folder starts a new set of attempts
	}
	changed := runner.ContractChanges(saved, runner.CellsContract(newSpec))
	if len(changed) > 0 {
		if len(changed) > 3 {
			changed = changed[:3]
		}
		return Errorf("not saved: this output folder holds attempts made with the current settings (%s). "+
			"Change output_dir too, or use Run again in a fresh folder.", strings.Join(changed, "; "))
	}
	return nil
}

func saveTrialText(trial string, text any, baseSha any, actor string) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	if _, err := ensureIdle(trial, nil); err != nil {
		return nil, err
	}
	t := strings.ReplaceAll(pstr(text), "\r\n", "\n")
	if err := contractGuard(trial, t); err != nil {
		return nil, err
	}
	sha, err := writeTrial(trial, t, pstr(baseSha), actor, "trial_edited", nil)
	if err != nil {
		return nil, err
	}
	return M{"sha256": sha}, nil
}

// pyFloat is Python float(value) for a JSON value.
func pyFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		s := strings.ReplaceAll(strings.TrimSpace(t), "_", "")
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			l := strings.ToLower(s)
			switch l {
			case "inf", "+inf", "infinity", "+infinity":
				return math.Inf(1), true
			case "-inf", "-infinity":
				return math.Inf(-1), true
			case "nan", "+nan", "-nan":
				return math.NaN(), true
			}
			return 0, false
		}
		return f, true
	}
	return util.Num(v)
}

func number(value any, label string, low, high float64) (float64, error) {
	n, ok := pyFloat(value)
	if !ok {
		return 0, Errorf("%s must be a number", label)
	}
	if !(low <= n && n <= high) {
		return 0, Errorf("%s must be between %s and %s", label, pyG(low), pyG(high))
	}
	return n, nil
}

// modelName is a model name as an agent CLI takes it: an alias such as sonnet,
// or the provider's model id. "" means the agent's default.
func modelName(value any) (any, error) {
	name := ""
	if util.Truthy(value) {
		name = strings.TrimSpace(pstr(value))
	}
	if name == "" {
		return nil, nil
	}
	if strings.ContainsFunc(name, isSpace) || strings.HasPrefix(name, "-") || util.RuneLen(name) > 200 {
		return nil, Errorf("%s is not a model name an agent CLI accepts: use an alias such as sonnet, opus or "+
			"haiku, or the provider's model id (no spaces), or leave it empty for the agent's default", runner.PyRepr(name))
	}
	return name, nil
}

func isSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// saveSettings edits the few values the page exposes. changes keys:
// timeout_minutes, repetitions, max_budget_usd, reporter_model, cell_models
// ({raw cell index: model or ""}).
func saveSettings(trial string, changes M, baseSha any, actor string) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	sp, err := ensureIdle(trial, nil)
	if err != nil {
		return nil, err
	}
	out := outputDir(trial, sp)
	var locked []string
	for k := range changes {
		if k != "repetitions" {
			locked = append(locked, k)
		}
	}
	sort.Strings(locked)
	if len(locked) > 0 && anyStateFile(out) {
		words := make([]string, len(locked))
		for i, k := range locked {
			words[i] = strings.ReplaceAll(k, "_", " ")
		}
		return nil, Errorf("these settings are locked for this output folder because its attempts used them; run "+
			"again in a fresh output folder to change %s", strings.Join(words, ", "))
	}
	old := util.MapOr(util.Get(sp, "trial"))
	cellsBefore := map[int]any{}
	for i, c := range util.Maps(rawTOML(trial)["cells"]) {
		cellsBefore[i] = c["model"]
	}
	data, err := os.ReadFile(trial)
	if err != nil {
		return nil, err
	}
	text := string(data)
	var notes []any
	if v, ok := changes["timeout_minutes"]; ok {
		minutes, err := number(v, "time limit", 1, 24*60)
		if err != nil {
			return nil, err
		}
		if text, err = SetValue(text, "trial", "timeout_seconds", int64(math.RoundToEven(minutes*60)), 0); err != nil {
			return nil, err
		}
		before, _ := util.Num(old["timeout_seconds"])
		notes = append(notes, fmt.Sprintf("time limit %s -> %s min", pyG(before/60), pyG(minutes)))
	}
	if v, ok := changes["repetitions"]; ok {
		reps, err := number(v, "attempts", 1, 50)
		if err != nil {
			return nil, err
		}
		if reps != math.Trunc(reps) {
			return nil, Errorf("attempts must be a whole number")
		}
		if text, err = SetValue(text, "trial", "repetitions", int64(reps), 0); err != nil {
			return nil, err
		}
		notes = append(notes, fmt.Sprintf("attempts %s -> %d", py(old["repetitions"]), int64(reps)))
	}
	if raw, ok := changes["max_budget_usd"]; ok {
		var budget any
		if raw != nil && raw != "" {
			b, err := number(raw, "budget", 0.01, 10000)
			if err != nil {
				return nil, err
			}
			budget = b
		}
		if text, err = SetValue(text, "trial", "max_budget_usd", budget, 0); err != nil {
			return nil, err
		}
		note := "budget "
		if before, isNum := util.Num(old["max_budget_usd"]); isNum && before != 0 {
			note += "$" + pyG(before)
		} else {
			note += "none"
		}
		note += " -> "
		if budget != nil {
			note += "$" + pyG(budget.(float64))
		} else {
			note += "none"
		}
		notes = append(notes, note)
	}
	if v, ok := changes["reporter_model"]; ok {
		model, err := modelName(v)
		if err != nil {
			return nil, err
		}
		if text, err = SetValue(text, "reporter", "model", model, 0); err != nil {
			return nil, err
		}
		before := util.Get(sp, "reporter", "model")
		notes = append(notes, fmt.Sprintf("reporter model %s -> %s", orDefault(before), orDefault(model)))
	}
	cellModels := util.MapOr(changes["cell_models"])
	keys := util.SortedKeys(cellModels)
	sort.SliceStable(keys, func(i, j int) bool {
		a, errA := strconv.Atoi(keys[i])
		b, errB := strconv.Atoi(keys[j])
		if errA != nil || errB != nil {
			return keys[i] < keys[j]
		}
		return a < b
	})
	for _, key := range keys {
		index, err := strconv.Atoi(strings.TrimSpace(key))
		if err != nil {
			return nil, Errorf("cell index %s is not a number", runner.PyRepr(key))
		}
		model, err := modelName(cellModels[key])
		if err != nil {
			return nil, err
		}
		if text, err = SetValue(text, "cells", "model", model, index); err != nil {
			return nil, err
		}
		notes = append(notes, fmt.Sprintf("cell %d model %s -> %s", index+1, orDefault(cellsBefore[index]), orDefault(model)))
	}
	if len(notes) == 0 {
		return nil, Errorf("nothing to change")
	}
	sha, err := writeTrial(trial, text, pstr(baseSha), actor, "settings_changed", M{"changes": notes})
	if err != nil {
		return nil, err
	}
	return M{"sha256": sha}, nil
}

func orDefault(v any) string {
	if !util.Truthy(v) {
		return "default"
	}
	return py(v)
}

var stampRe = regexp.MustCompile(`-\d{8}-\d{6}$`)

// freshOutput points trial.output_dir at a new, empty folder next to the current one.
func freshOutput(trial, baseSha, actor string) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	sp, err := ensureIdle(trial, nil)
	if err != nil {
		return nil, err
	}
	current := outputDir(trial, sp)
	name := stampRe.ReplaceAllString(filepath.Base(current), "") + time.Now().Format("-20060102-150405")
	target := filepath.Join(filepath.Dir(current), name)
	value, err := filepath.Rel(filepath.Dir(trial), target)
	if err != nil {
		value = target
	}
	data, err := os.ReadFile(trial)
	if err != nil {
		return nil, err
	}
	text, err := SetValue(string(data), "trial", "output_dir", value, 0)
	if err != nil {
		return nil, err
	}
	sha, err := writeTrial(trial, text, baseSha, actor, "fresh_output", M{"output_dir": value})
	if err != nil {
		return nil, err
	}
	return M{"sha256": sha, "output_dir": value}, nil
}

func addAttempt(trial, baseSha, actor string) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	sp, loadErr := load(trial)
	if loadErr != "" {
		return nil, Errorf("%s", loadErr)
	}
	reps := util.Int(util.Get(sp, "trial", "repetitions"), 1)
	return saveSettings(trial, M{"repetitions": int64(reps + 1)}, baseSha, actor)
}

func savePrompt(trial string, text any, baseSha any, actor string) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	sp, _ := load(trial)
	path := promptPath(trial, sp)
	if path == "" {
		return nil, Errorf("task.prompt_file is not set in trial.toml")
	}
	snap, err := snapshot(trial, -1, nil)
	if err != nil {
		return nil, err
	}
	if lock := util.Get(snap, "locks", "prompt"); lock != nil {
		return nil, Errorf("%s", py(lock))
	}
	current, _ := os.ReadFile(path)
	if pstr(baseSha) != sha256Hex(current) {
		return nil, Conflict("the task prompt changed on disk (by your agent or an editor). Reload to see it.")
	}
	t := strings.ReplaceAll(pstr(text), "\r\n", "\n")
	if strings.TrimSpace(t) == "" {
		return nil, Errorf("the task prompt cannot be empty")
	}
	if !strings.HasSuffix(t, "\n") {
		t += "\n"
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.console-%s.tmp", filepath.Base(path), tmpTag()))
	if err := os.WriteFile(tmp, []byte(t), 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	newSha := sha256Hex([]byte(t))
	record(trial, actor, "prompt_edited", M{"before": util.Clip(sha256Hex(current), 12), "after": util.Clip(newSha, 12)})
	return M{"sha256": newSha}, nil
}
