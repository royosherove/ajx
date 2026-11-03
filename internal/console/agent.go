package console

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

// ---------------------------------------------------------------- agent view

func headline(snap M) string {
	proc, out := util.MapOr(snap["process"]), util.MapOr(snap["output"])
	runs := util.Maps(snap["runs"])
	state := util.S(proc["state"])
	switch {
	case state == "running" || state == "stopping":
		var active []string
		for _, r := range runs {
			if util.Truthy(r["current"]) {
				active = append(active, fmt.Sprintf("%s at %s", py(r["id"]), py(r["current"])))
			}
		}
		head := "A run is in progress"
		if state == "stopping" {
			head = "Stopping the run"
		}
		if len(active) > 0 {
			head += ": " + strings.Join(active, ", ")
		}
		return head
	case state == "checking" || state == "starting":
		if state == "starting" {
			return "Checking readiness before starting"
		}
		return "Checking readiness"
	case state == "busy":
		return "Rebuilding the reports"
	case util.Truthy(out["started"]):
		var parts []string
		for _, r := range runs {
			if !util.Truthy(r["started"]) {
				continue
			}
			verified := util.OrStr(pstr(util.Get(r, "verify", "outcome")), "not verified")
			asks := util.MapOr(r["asks"])
			text := "asks " + py(asks["status"])
			if asks["status"] == "ready" {
				text = py(asks["count"]) + " asks"
			}
			parts = append(parts, fmt.Sprintf("%s: %s, %s", py(r["id"]), verified, text))
		}
		head := fmt.Sprintf("%s of %s attempts finished", py(out["complete"]), py(out["planned"]))
		if len(parts) > 0 {
			head += "; " + strings.Join(parts, "; ")
		}
		return head
	case util.Get(snap, "plan", "approval", "state") == "valid":
		return "Approved but not started"
	}
	return "Not started. The user reviews and starts the trial in the console."
}

func pick(m M, keys ...string) M {
	out := M{}
	for _, k := range keys {
		out[k] = m[k]
	}
	return out
}

// Since is what changed after cursor, for an agent catching up before it
// replies (`ajx console since`). touch records the agent's check-in.
func Since(trial string, cursor int, touch bool) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	snap, err := snapshot(trial, 3, nil)
	if err != nil {
		return nil, err
	}
	entries, _ := readEntries(trial, cursor)
	newCursor := util.Get(snap, "journal", "cursor")
	if touch {
		touchAgent(trial, "seen", nil)
	}
	all, _ := readEntries(trial, 0)
	replied := map[int]bool{}
	for _, e := range all {
		if e["kind"] == "note" {
			if n, ok := intOf(e["reply_to"]); ok {
				replied[n] = true
			}
		}
	}
	messages := []any{}
	for _, e := range all {
		if e["kind"] == "message" && !replied[entryN(e)] {
			messages = append(messages, M{"n": e["n"], "at": e["at"], "text": e["text"], "about": e["about"]})
		}
	}
	plan := util.MapOr(snap["plan"])
	ready := util.MapOr(snap["readiness"])
	output := util.MapOr(snap["output"])
	process := util.MapOr(snap["process"])
	where, short := Short(filepath.Dir(trial)), util.S(plan["short"])
	doNot := []any{fmt.Sprintf("approve, start, stop or retry for the user. If they approve in chat, run `ajx console start %s "+
		"--plan %s --quote \"<their words>\"`", where, short)}
	kind := output["blocker_kind"]
	switch util.S(process["state"]) {
	case "running", "stopping", "busy", "starting", "checking":
		doNot = append(doNot, "start another run or edit the trial while this one is in progress")
	default:
		switch kind {
		case "complete":
			doNot = append(doNot, fmt.Sprintf("start again: every planned attempt finished. To run it again, use `ajx console again %s "+
				"--same-folder` (one more comparable attempt) or `--fresh-folder`, with the user's approval", where))
		case "changed":
			doNot = append(doNot, "start in this output folder: the task or profiles changed. Offer a fresh output folder")
		}
	}
	if util.Truthy(output["started"]) && kind != "changed" {
		doNot = append(doNot, "edit the task prompt, models or limits for this output folder; to change them, offer "+
			fmt.Sprintf("`ajx console again %s --fresh-folder`", where))
	}
	if util.Truthy(ready["needs_ack"]) && util.Truthy(ready["warnings"]) {
		doNot = append(doNot, "pass --accept-warnings unless the user accepted those readiness warnings in their own words")
	}
	info := ServerInfo(trial)
	outDir := util.S(output["dir"])
	changes := []any{}
	for _, e := range entries {
		changes = append(changes, M{"n": e["n"], "at": e["at"], "actor": e["actor"], "kind": e["kind"],
			"text": clip(describe(e), 600)})
	}
	runs := []any{}
	for _, r := range util.Maps(snap["runs"]) {
		if !util.Truthy(r["started"]) {
			continue
		}
		result := util.MapOr(r["result"])
		asks := util.MapOr(r["asks"])
		var asksV any = asks["status"]
		if asks["status"] == "ready" {
			asksV = asks["count"]
		}
		files := M{}
		for k, v := range util.MapOr(r["links"]) {
			files[k] = filepath.Join(outDir, util.S(v))
		}
		runs = append(runs, M{"id": r["id"], "stage": r["current"], "complete": r["complete"], "model": r["model"],
			"verified": util.Get(r, "verify", "outcome"), "cleanup": r["teardown"],
			"exit_code": result["exit_code"], "stop_reason": result["stop_reason"], "error": result["error"],
			"could_not_start": result["could_not_start"], "asks": asksV, "review_status": r["review_status"],
			"files": files})
	}
	head := strings.TrimRight(headline(snap), ".") + "."
	if len(messages) > 0 {
		head += fmt.Sprintf(" %d message(s) for you.", len(messages))
	}
	console := M{"running": info != nil, "url": nil, "results_url": nil}
	if info != nil {
		console["url"] = info["url"]
		console["results_url"] = util.S(info["url"]) + "&tab=results"
	}
	problems, warnings := listOr(ready["problems"]), listOr(ready["warnings"])
	return M{
		"schema": Schema, "trial": trial, "since": cursor, "cursor": newCursor,
		"headline":           head,
		"changes":            changes,
		"messages_for_agent": messages,
		"plan":               M{"sha": short, "complete": plan["complete"], "approval": plan["approval"]},
		"readiness": M{"status": ready["status"], "current": ready["current"], "problems": problems, "warnings": warnings,
			"warnings_need_acceptance": util.Truthy(ready["needs_ack"])},
		"process": pick(process, "state", "job", "pid", "since", "by", "command"),
		"output": pick(output, "dir", "started", "planned", "complete", "blocker", "blocker_kind", "index",
			"pending_reports", "unstarted"),
		"runs":    runs,
		"console": console,
		"do_not":  doNot,
	}, nil
}

// Wait blocks until the journal gains an entry after cursor (an action, or a
// run milestone such as verification or a finished report), or timeoutSec
// seconds pass; then returns Since with "woke" set to "journal" or "timeout".
// Cancelling ctx returns util.ErrInterrupted.
func Wait(ctx context.Context, trial string, cursor, timeoutSec int) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	if timeoutSec > 3600 {
		timeoutSec = 3600
	}
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	touchAgent(trial, "watching", float64(deadline.UnixNano())/1e9)
	woke := "timeout"
	interrupted := false
	func() {
		defer touchAgent(trial, "seen", nil)
		for time.Now().Before(deadline) {
			sp, _ := load(trial)
			journalProgress(trial, outputDir(trial, sp), nil)
			entries, _ := readEntries(trial, 0)
			reconcile(trial, entries, nil)
			if _, n := readEntries(trial, 0); n > cursor {
				woke = "journal"
				return
			}
			select {
			case <-ctx.Done():
				interrupted = true
				return
			case <-time.After(time.Second):
			}
		}
	}()
	if interrupted {
		return nil, util.ErrInterrupted
	}
	result, err := Since(trial, cursor, false)
	if err != nil {
		return nil, err
	}
	result["woke"] = woke
	return result, nil
}

// Note posts an agent's short explanation to the page's activity list.
// replyTo 0 means none; otherwise it must be a user message's line number.
func Note(trial, text string, replyTo int, actor string) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	if actor == "" {
		actor = "agent"
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return nil, Errorf("a note needs text")
	}
	var reply any
	if replyTo != 0 {
		found := false
		entries, _ := readEntries(trial, 0)
		for _, e := range entries {
			if entryN(e) == replyTo && e["kind"] == "message" {
				found = true
				break
			}
		}
		if !found {
			return nil, Errorf("#%d is not a message from the user; `ajx console since` lists those under "+
				"messages_for_agent", replyTo)
		}
		reply = replyTo
	}
	entry := record(trial, actor, "note", M{"text": clip(text, maxText), "reply_to": reply})
	touchAgent(trial, "seen", nil)
	return entry, nil
}
