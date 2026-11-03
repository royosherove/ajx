package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/util"
)

// ---------------------------------------------------------------- journal and presence

func journalPath(trial string) string { return filepath.Join(StateDir(trial), "actions.jsonl") }

// record appends one action and returns it with its line number "n". A single
// O_APPEND write keeps lines whole across processes. The line is what Python
// writes: json.dumps(entry, ensure_ascii=False, sort_keys=True).
func record(trial, actor, kind string, fields M) M {
	entry := M{"at": util.Now(), "actor": actor, "kind": kind}
	for k, v := range fields {
		if v != nil {
			entry[k] = v
		}
	}
	raw := []byte(util.PyJSON(entry))
	path := journalPath(trial)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err == nil {
		_, _ = f.Write(append(append([]byte{}, raw...), '\n'))
		f.Close()
	}
	data, _ := os.ReadFile(path)
	lines := bytes.Split(data, []byte("\n"))
	lines = lines[:len(lines)-1]
	n := len(lines)
	for i := len(lines); i > 0; i-- {
		if bytes.Equal(lines[i-1], raw) {
			n = i
			break
		}
	}
	entry["n"] = n
	return entry
}

// readEntries is (entries after cursor, new cursor). The cursor counts complete
// lines; each entry carries its 1-based line number "n". A line still being
// written is left for the next read.
func readEntries(trial string, cursor int) ([]M, int) {
	data, _ := os.ReadFile(journalPath(trial))
	complete := bytes.Split(data, []byte("\n"))
	complete = complete[:len(complete)-1]
	var out []M
	for i, raw := range complete {
		n := i + 1
		if n <= cursor {
			continue
		}
		v, err := runner.DecodeJSON(raw)
		entry, ok := v.(M)
		if err != nil || !ok {
			entry = M{"kind": "unreadable"}
		}
		entry["n"] = n
		out = append(out, entry)
	}
	return out, len(complete)
}

func touchAgent(trial, mode string, until any) {
	_ = writePrivate(filepath.Join(StateDir(trial), "agent.json"), M{"last_seen": util.Now(), "mode": mode, "until": until})
}

func presence(trial string) M {
	info := readMap(filepath.Join(StateDir(trial), "agent.json"))
	if !util.Truthy(info["last_seen"]) {
		return M{"state": "never", "last_seen": nil}
	}
	until, _ := util.Num(info["until"])
	if info["mode"] == "watching" && until > float64(time.Now().UnixNano())/1e9 {
		return M{"state": "watching", "last_seen": info["last_seen"]}
	}
	return M{"state": "seen", "last_seen": info["last_seen"]}
}

func last(entries []M, kinds ...string) M {
	for i := len(entries) - 1; i >= 0; i-- {
		if util.Contains(kinds, util.S(entries[i]["kind"])) {
			return entries[i]
		}
	}
	return nil
}

func entryN(e M) int { return util.Int(e["n"], 0) }

// ---------------------------------------------------------------- jobs

// ajxCommand is the argv prefix that runs this CLI: AJX_BIN when set (tests, or
// running the Python CLI side by side), else this executable.
func ajxCommand() []string {
	if bin := os.Getenv("AJX_BIN"); bin != "" {
		return []string{bin}
	}
	exe, err := os.Executable()
	if err != nil {
		return []string{"ajx"}
	}
	return []string{exe}
}

func jobEnv() map[string]string {
	return util.ChildEnv(map[string]string{"PYTHONUNBUFFERED": "1"}, nil)
}

var (
	planIDRe   = regexp.MustCompile(`^[0-9a-f]{40,}$`)
	plainArgRe = regexp.MustCompile(`^[\p{L}\p{N}_@%+=:,./~-]+$`)
)

func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// display is the equivalent command line for people: the home directory as ~
// (unquoted, to stay readable) and plan ids shortened.
func display(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		a = Short(a)
		switch {
		case planIDRe.MatchString(a):
			parts[i] = a[:12] + "…"
		case plainArgRe.MatchString(a):
			parts[i] = a
		default:
			parts[i] = shQuote(a)
		}
	}
	return "ajx " + strings.Join(parts, " ")
}

// jobs are launched commands keyed by their job_started line number (pids can be reused).
func jobs(entries []M) ([]M, map[int]bool) {
	var started []M
	finished := map[int]bool{}
	for _, e := range entries {
		switch util.S(e["kind"]) {
		case "job_started":
			if util.Truthy(e["pid"]) {
				started = append(started, e)
			}
		case "job_finished":
			if n, ok := intOf(e["job_n"]); ok {
				finished[n] = true
			}
		}
	}
	return started, finished
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func jobAlive(e M) bool {
	verb := "run"
	if e["job"] == "report" {
		verb = "report"
	}
	return util.S(e["host"]) == hostname() && alive(e["pid"]) && isAjx(e["pid"], verb) != 0
}

// jobSummary is what a finished command left behind, in words, rather than its exit code.
func jobSummary(trial string, job any, code any) string {
	if c, ok := intOf(code); ok && code != nil {
		switch c {
		case 130, -2:
			return "was stopped; the evidence so far is kept"
		case 2:
			return "did not start: the trial file has an error (see the log)"
		case 3:
			return "did not start: another ajx run holds this output folder"
		}
	}
	var parts []string
	func() {
		defer func() { _ = recover() }() // a summary is a courtesy; never fail the record
		sp, _ := load(trial)
		for _, r := range runViews(sp, outputDir(trial, sp), -1) {
			if !util.Truthy(r["started"]) {
				continue
			}
			result := util.MapOr(r["result"])
			var worker string
			switch {
			case util.Truthy(result["could_not_start"]):
				worker = "could not start"
			case util.Truthy(result["interrupted"]):
				worker = "stopped"
			default:
				worker = util.OrStr(pstr(util.Get(r, "verify", "outcome")), "not verified")
			}
			report := "no report yet"
			switch r["review_status"] {
			case "ready":
				report = "report ready"
			case "incomplete":
				report = "report incomplete"
			}
			parts = append(parts, fmt.Sprintf("%s %s, %s", py(r["id"]), worker, report))
		}
	}()
	c, _ := intOf(code)
	lead := "finished with problems"
	if code == nil || c == 0 || job == "report" {
		lead = "finished"
	}
	if len(parts) > 0 {
		return lead + ": " + strings.Join(parts, "; ")
	}
	return lead
}

// finishJob records a launched command's end once, whichever process notices first.
func finishJob(trial string, jobN, pid int, exitCode any, job any) bool {
	marker := filepath.Join(StateDir(trial), fmt.Sprintf("finished-%d", jobN))
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	record(trial, "console", "job_finished", M{"job_n": jobN, "pid": pid, "job": job, "exit_code": exitCode,
		"summary": jobSummary(trial, job, exitCode)})
	return true
}

// reconcile records the end of commands whose process is gone. The server
// records its own children with their exit codes; others wait a few quiet
// seconds so a reaper can record the code first.
func reconcile(trial string, entries []M, skip map[int]bool) bool {
	started, finished := jobs(entries)
	changed := false
	for _, e := range started {
		pid, _ := intOf(e["pid"])
		if finished[entryN(e)] || skip[pid] || jobAlive(e) {
			continue
		}
		name := filepath.Base(util.OrStr(pstr(e["log"]), "none"))
		log := filepath.Join(StateDir(trial), "logs", name)
		if st, err := os.Stat(log); err == nil && time.Since(st.ModTime()) < 3*time.Second {
			continue
		}
		if finishJob(trial, entryN(e), pid, nil, e["job"]) {
			changed = true
		}
	}
	return changed
}

func lockInfo(out string) M {
	info := readMapOrNil(filepath.Join(out, ".lock"))
	if info == nil {
		return nil
	}
	same := util.S(info["host"]) == hostname()
	isAlive := same && alive(info["pid"]) && (isAjx(info["pid"], "run") != 0 || isAjx(info["pid"], "ci") != 0)
	res := util.Copy(info)
	res["alive"] = isAlive
	res["same_host"] = same
	return res
}

// activeJob is the command still running for this trial: one the console or an
// agent launched, or an `ajx run` started elsewhere that holds the output lock.
func activeJob(trial string, entries []M, out string) M {
	started, finished := jobs(entries)
	for i := len(started) - 1; i >= 0; i-- {
		e := started[i]
		if !finished[entryN(e)] && jobAlive(e) {
			return e
		}
	}
	if lock := lockInfo(out); lock != nil && util.Truthy(lock["alive"]) {
		return M{"job": "run", "pid": lock["pid"], "at": lock["at"], "actor": "outside",
			"command": "ajx run (started outside the console)", "outside": true}
	}
	return nil
}

// launch starts `ajx <args>` in its own session with output to a log under
// .ajx-console/logs, and journals job_started.
func launch(trial, job string, args []string, actor string, fields M) (*exec.Cmd, M, error) {
	logs := filepath.Join(StateDir(trial), "logs")
	_ = os.Mkdir(logs, 0o755)
	log := filepath.Join(logs, time.Now().Format("20060102-150405")+"-"+job+".log")
	fh, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	defer fh.Close()
	argv := append(ajxCommand(), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = filepath.Dir(trial)
	cmd.Stdout = fh
	cmd.Stderr = fh
	cmd.Env = util.EnvList(jobEnv())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	all := M{"job": job, "pid": cmd.Process.Pid, "host": hostname(), "command": display(args), "log": filepath.Base(log)}
	for k, v := range fields {
		all[k] = v
	}
	entry := record(trial, actor, "job_started", all)
	return cmd, entry, nil
}

// runCLI runs `ajx <args>` to completion and returns M{command, exit_code, timed_out, output}.
func runCLI(args []string, timeout time.Duration, cwd string) M {
	argv := append(ajxCommand(), args...)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = util.EnvList(jobEnv())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	output := strings.ToValidUTF8(stdout.String()+stderr.String(), "�")
	var code any
	timedOut := false
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		timedOut = true
		output += fmt.Sprintf("\n(timed out after %ds)", int(timeout.Seconds()))
	case err != nil && cmd.ProcessState == nil:
		output += fmt.Sprintf("%v", err)
		code = nil
		var pe *os.PathError
		if errors.As(err, &pe) || errors.Is(err, exec.ErrNotFound) {
			code = 127
		}
	default:
		code = util.ExitCode(cmd.ProcessState)
	}
	return M{"command": display(args), "exit_code": code, "timed_out": timedOut, "output": clip(output, 40000)}
}

// ---------------------------------------------------------------- readiness

func readinessPath(trial string) string { return filepath.Join(StateDir(trial), "readiness.json") }

func readiness(trial, currentPlan string) M {
	r := util.Copy(readMap(readinessPath(trial)))
	if len(r) == 0 {
		return M{"status": "unknown", "current": false}
	}
	if r["status"] == "checking" && !alive(r["pid"]) {
		r["status"] = "unknown"
	}
	r["current"] = currentPlan != "" && util.S(r["plan"]) == currentPlan
	return r
}

var problemRe = regexp.MustCompile(`(?i)NOT READY|FAILED|PROBLEMS|WARNING|trial file error|error:`)

// check runs `ajx validate` then `ajx doctor`, recorded for the plan they checked.
func check(trial, actor string) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	sp, _ := load(trial)
	plan := planSha(trial, sp)
	_ = writePrivate(readinessPath(trial), M{"status": "checking", "plan": plan, "started_at": util.Now(), "pid": os.Getpid()})
	record(trial, actor, "check_started", M{"plan": util.Clip(plan, 12)})
	var steps []any
	ok := true
	for _, step := range []struct {
		name    string
		timeout time.Duration
	}{{"validate", 120 * time.Second}, {"doctor", 300 * time.Second}} {
		res := runCLI([]string{step.name, trial}, step.timeout, filepath.Dir(trial))
		entry := M{"name": step.name}
		for k, v := range res {
			entry[k] = v
		}
		steps = append(steps, entry)
		if c, isNum := intOf(res["exit_code"]); res["exit_code"] == nil || !isNum || c != 0 {
			ok = false
			break
		}
	}
	var lines []string
	for _, s := range steps {
		for _, line := range splitLines(util.S(util.Get(s, "output"))) {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	warnings, problems := []any{}, []any{}
	for _, line := range lines {
		if strings.HasPrefix(line, "WARNING ") {
			if len(warnings) < 12 {
				warnings = append(warnings, line[len("WARNING "):])
			}
		} else if problemRe.MatchString(line) && len(problems) < 12 {
			problems = append(problems, line)
		}
	}
	account := sp != nil && (util.Truthy(util.Get(sp, "task", "identity_cmd")) || util.Truthy(sp["preflight"]))
	status := "not_ready"
	if ok {
		status = "ready"
	}
	if steps == nil {
		steps = []any{}
	}
	result := M{"status": status, "plan": plan, "checked_at": util.Now(), "steps": steps, "problems": problems,
		"warnings": warnings, "needs_ack": len(warnings) > 0 && account}
	_ = writePrivate(readinessPath(trial), result)
	record(trial, actor, "checked", M{"plan": util.Clip(plan, 12), "status": status})
	return result, nil
}

// splitLines is Python str.splitlines() for \n, \r\n and \r.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
