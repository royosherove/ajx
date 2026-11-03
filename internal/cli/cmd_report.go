package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/royosherove/ajx/internal/console"
	"github.com/royosherove/ajx/internal/demo"
	"github.com/royosherove/ajx/internal/export"
	"github.com/royosherove/ajx/internal/render"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

var demoGenerate = demo.Generate

func cmdReport(ns *Namespace) (int, error) {
	sp, err := spec.Load(ns.Str("trial"), spec.LoadOpts{})
	if err != nil {
		return 2, err
	}
	jsonMode := ns.Bool("json")
	out := util.S(util.Get(sp, "trial", "output_dir"))
	var rendered []any
	for _, d := range runDirs(out) {
		if isFile(filepath.Join(d, "run.json")) {
			if err := render.RunReport(d); err != nil {
				return 1, err
			}
			rendered = append(rendered, filepath.Base(d))
		}
	}
	var log func(string)
	if jsonMode {
		log = func(m string) { fmt.Fprintln(Stderr, m) }
	} else {
		log = func(m string) { fmt.Fprintln(Stdout, m) }
	}
	if err := render.MatrixReport(sp, render.MatrixOpts{Synthesize: !ns.Bool("no_synthesis"), Log: log, Ctx: context.Background()}); err != nil {
		return 1, err
	}
	index := filepath.Join(out, "index.html")
	if jsonMode {
		if rendered == nil {
			rendered = []any{}
		}
		emitJSON(util.M{"schema_version": SchemaVersion, "command": "report", "ok": true, "exit_code": 0, "output_dir": out,
			"index": index, "matrix_markdown": filepath.Join(out, "matrix.md"), "matrix_json": filepath.Join(out, "matrix.json"),
			"runs_rendered": rendered, "synthesis": !ns.Bool("no_synthesis")})
		return 0, nil
	}
	outln(index)
	return 0, nil
}

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h := util.Home(); h != "" {
			return h + p[1:]
		}
	}
	return p
}

func dirNonEmpty(p string) bool {
	entries, err := os.ReadDir(p)
	return err == nil && len(entries) > 0
}

func absClean(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		if r, err := filepath.EvalSymlinks(a); err == nil {
			return r
		}
		return a
	}
	return p
}

func cmdExport(ns *Namespace) (int, error) {
	out, _, err := outputOf(ns.Str("trial"))
	if err != nil {
		return 2, err
	}
	jsonMode := ns.Bool("json")
	failJ := func(msg string) (int, error) {
		if jsonMode {
			fmt.Fprintln(Stderr, msg)
			emitJSON(util.M{"schema_version": SchemaVersion, "command": "export", "ok": false, "exit_code": 1,
				"error": util.M{"type": "export_error", "message": msg}})
			return 1, nil
		}
		return fail(msg)
	}
	id := ns.Str("run")
	if id == "" || filepath.Base(id) != id || id == "." || id == ".." || strings.HasSuffix(id, "/") {
		return failJ(fmt.Sprintf("export: --run takes a run id such as my-cell-r1, not %s", spec.Repr(id)))
	}
	runsDir := filepath.Join(out, "runs")
	runDir := filepath.Join(runsDir, id)
	if !isDir(runDir) {
		var known []string
		for _, d := range runDirs(out) {
			known = append(known, filepath.Base(d))
		}
		msg := fmt.Sprintf("export: run %s not found in %s", spec.Repr(id), runsDir)
		if len(known) > 0 {
			msg += "; runs: " + strings.Join(known, ", ")
		} else {
			msg += "; no runs recorded there"
		}
		return failJ(msg)
	}
	dest := filepath.Join(out, "share", id)
	if d := ns.Str("dest"); d != "" {
		dest = expandUser(d)
	}
	if absClean(dest) == absClean(runDir) {
		return failJ(fmt.Sprintf("export: --dest must not be the run directory itself (%s)", runDir))
	}
	if isDir(dest) && dirNonEmpty(dest) && !ns.Bool("force") {
		return failJ(fmt.Sprintf("export: %s is not empty; pass --force to overwrite its report.html, journey.html and asks.md", dest))
	}
	summary, err := export.ExportRun(runDir, dest, export.ExportOpts{})
	if err != nil {
		var ee *export.Error
		var pe *fs.PathError
		if errors.As(err, &ee) || errors.As(err, &pe) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			return failJ("export: " + err.Error())
		}
		return failJ("export: " + err.Error())
	}
	counts := util.MapOr(summary["replacements"])
	order := util.Strings(summary["replacement_order"])
	if len(order) == 0 {
		order = util.SortedKeys(counts)
	}
	if jsonMode {
		emitJSON(util.M{"schema_version": SchemaVersion, "command": "export", "ok": true, "exit_code": 0,
			"run_id": id, "dest": summary["dest"], "files": util.Or(summary["files"], []any{}), "replacements": counts,
			"replacement_order": strs(order), "warnings": util.Or(summary["warnings"], []any{})})
		return 0, nil
	}
	outln(util.Str(summary["dest"]))
	for _, name := range util.Strings(summary["files"]) {
		outf("  %s\n", name)
	}
	var parts []string
	for _, label := range order {
		parts = append(parts, fmt.Sprintf("%s x %s", util.Str(counts[label]), label))
	}
	if len(parts) == 0 {
		outln("replacements: none")
	} else {
		outln("replacements: " + strings.Join(parts, ", "))
	}
	for _, note := range util.Strings(summary["warnings"]) {
		outf("note: %s\n", note)
	}
	outln("Review before sharing: exported reports still contain the agent's commands and outputs, " +
		"account identifiers, and URLs.")
	return 0, nil
}

// ---------------------------------------------------------------- console

type consoleOpenOpts struct {
	Port        int
	OpenBrowser bool
	Foreground  bool
}

func consoleOpen(trial string, o consoleOpenOpts) (int, error) {
	if o.Foreground {
		err := console.Serve(trial, console.ServeOpts{Port: o.Port, OpenBrowser: o.OpenBrowser, Announce: func(url string) {
			outf("AJX Console: %s\nPress Ctrl-C to stop it; runs keep going.\n", url)
		}})
		if err != nil {
			return 2, err
		}
		return 0, nil
	}
	info, err := console.OpenConsole(trial, console.OpenOpts{Port: o.Port, OpenBrowser: o.OpenBrowser})
	if err != nil {
		return 2, err
	}
	tf, err := console.TrialFile(trial)
	if err != nil {
		return 2, err
	}
	where := shortHome(filepath.Dir(tf))
	outf("AJX Console: %s\n", util.Str(info["url"]))
	started := "Already running"
	if util.Truthy(info["started"]) {
		started = "Started"
	}
	outf("%s in the background (pid %s); it stops after 30 idle minutes or with: ajx console stop %s\n", started,
		num(info["pid"]), where)
	outln("Closing the page, this terminal or the chat never stops a run.")
	outf("Agents catch up with: ajx console since %s --cursor 0\n", where)
	others := 0
	for _, c := range console.ListConsoles() {
		if util.Int(c["pid"], -1) != util.Int(info["pid"], -2) {
			others++
		}
	}
	if others > 0 {
		outf("%d other console(s) running: ajx console list (ajx console stop --all stops them)\n", others)
	}
	return 0, nil
}

func cmdConsole(ns *Namespace) (int, error) {
	trial := ns.Str("trial")
	switch ns.Str("action") {
	case "demo":
		tf, err := console.CopyDemo(ns.Str("dir"), ns.Bool("force"))
		if err != nil {
			return 2, err
		}
		outf("Copied the console demo to %s. Its worker is simulated: no agent, model or network is used.\n",
			shortHome(filepath.Dir(tf)))
		return consoleOpen(tf, consoleOpenOpts{Port: ns.Int("port", 0), OpenBrowser: !ns.Bool("no_open")})
	case "open":
		return consoleOpen(trial, consoleOpenOpts{Port: ns.Int("port", 0), OpenBrowser: !ns.Bool("no_open"),
			Foreground: ns.Bool("foreground")})
	case "serve":
		err := console.Serve(trial, console.ServeOpts{Port: ns.Int("port", 0), OpenBrowser: ns.Bool("open"),
			Announce: func(url string) { outf("serving %s\n", url) }})
		if err != nil {
			return 2, err
		}
		return 0, nil
	case "stop":
		all := ns.Bool("all")
		if trial == "all" {
			if _, err := os.Stat("all"); err != nil {
				all = true
			}
		}
		if all {
			if n := console.StopAll(); n > 0 {
				outf("stopped %d console(s); runs keep going\n", n)
			} else {
				outln("no console was running")
			}
			return 0, nil
		}
		if trial == "" {
			running := console.ListConsoles()
			msg := "console stop: name a trial, or stop every console with: ajx console stop --all"
			if len(running) > 0 {
				msg += fmt.Sprintf(" (%d running; see ajx console list)", len(running))
			} else {
				msg += " (none is running)"
			}
			return fail(msg)
		}
		stopped, err := console.StopConsole(trial)
		if err != nil {
			return 2, err
		}
		if stopped {
			outln("console stopped; runs keep going")
		} else {
			outln("no console was running for this trial")
		}
		return 0, nil
	case "list":
		consoles := console.ListConsoles()
		if ns.Bool("json") {
			list := make([]any, 0, len(consoles))
			for _, c := range consoles {
				list = append(list, c)
			}
			emitJSON(list)
			return 0, nil
		}
		if len(consoles) == 0 {
			outln("no console is running")
			return 0, nil
		}
		for _, c := range consoles {
			url := util.Str(c["url"])
			if url == "" {
				url = "(starting or unreachable)"
			}
			outf("pid %7s  up %9s  %s\n    %s\n", num(c["pid"]), util.Str(c["elapsed"]), util.Str(c["short"]), url)
		}
		outln("Stop one with: ajx console stop <trial>; all with: ajx console stop --all (runs keep going)")
		return 0, nil
	case "state":
		snap, err := console.Snapshot(trial, console.SnapshotOpts{})
		if err != nil {
			return 2, err
		}
		emitJSON(snap)
		return 0, nil
	case "since":
		data, err := console.Since(trial, ns.Int("cursor", 0), true)
		if err != nil {
			return 2, err
		}
		emitJSON(data)
		return 0, nil
	case "wait":
		return WithSignals(func(ctx context.Context) (int, error) {
			data, err := console.Wait(ctx, trial, ns.Int("cursor", 0), ns.Int("timeout", 90))
			if err != nil {
				return 2, err
			}
			emitJSON(data)
			return 0, nil
		})
	case "note":
		reply := ns.Int("reply_to", 0)
		entry, err := console.Note(trial, ns.Str("text"), reply, "agent")
		if err != nil {
			return 2, err
		}
		where := "in the journal; the console is not running, so the user sees it when it is opened again"
		if console.ServerInfo(trial) != nil {
			where = "on the console's Activity tab"
		}
		line := "posted note #" + num(entry["n"])
		if ns.Has("reply_to") && reply != 0 {
			line += fmt.Sprintf(" replying to #%d", reply)
		}
		outln(line + " " + where)
		return 0, nil
	case "start":
		if err := console.PreflightStart(trial, ns.Str("plan")); err != nil {
			return 2, err
		}
		outln("checking readiness (ajx validate, then ajx doctor) for this plan...")
		entry, err := console.Start(trial, console.StartOpts{Actor: "agent", Via: "chat", Plan: ns.Str("plan"),
			Quote: ns.Str("quote"), AcceptWarnings: ns.Bool("accept_warnings")})
		if err != nil {
			return 2, err
		}
		outf("started %s (pid %s); the console shows it live, and `ajx console wait` returns when it changes\n",
			util.Str(entry["command"]), num(entry["pid"]))
		return 0, nil
	case "again":
		mode := "fresh-folder"
		if ns.Bool("same_folder") {
			mode = "same-folder"
		}
		outf("run again (%s): checking readiness (ajx validate, then ajx doctor)...\n", mode)
		entry, err := console.RunAgain(trial, console.AgainOpts{Actor: "agent", Via: "chat", Mode: mode, Plan: ns.Str("plan"),
			Quote: ns.Str("quote"), AcceptWarnings: ns.Bool("accept_warnings")})
		if err != nil {
			return 2, err
		}
		outf("started %s (pid %s); earlier attempts are kept as they were\n", util.Str(entry["command"]), num(entry["pid"]))
		return 0, nil
	}
	return 2, nil
}

// num formats a JSON number as Python prints an int.
func num(v any) string {
	if f, ok := util.Num(v); ok && f == float64(int64(f)) {
		return fmt.Sprint(int64(f))
	}
	return util.Str(v)
}
