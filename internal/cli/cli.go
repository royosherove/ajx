// Package cli is the ajx command line: the same commands, options, output
// lines and exit codes as skills/ajx/bin/ajx, plus a setup wizard for
// `ajx init`, `--json` output for agents and pipelines, `ajx ci`,
// `ajx version` and `ajx completion`.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	ajx "github.com/royosherove/ajx"
	"github.com/royosherove/ajx/internal/console"
	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

// Output streams; tests may replace them.
var (
	Stdout io.Writer = os.Stdout
	Stderr io.Writer = os.Stderr
)

// SchemaVersion is the version of every --json document and ci-result.json.
const SchemaVersion = 1

const description = `ajx: deterministic runner for AJX v1 trials (matrix of harness x model x config x repetition).

Commands:
  init <dir> --product NAME [--harness NAME]   scaffold a trial for your chosen CLI
                                   (asks the rest in a terminal; --no-input never asks)
  example <dir>                   generate fictional reports offline; no agent calls
  doctor [trial.toml]              harness/auth/runner availability; model identity vs task identity;
                                   env overrides and collisions; preflight (no secrets printed)
  validate <trial.toml>            check the trial file, print the run plan, prompt hash and env warnings
  run <trial.toml> [--cells a,b] [--stages s1,s2] [--dry-run] [--keep-workspace] [--no-synthesis]
  status <trial.toml|matrix-dir> [--tail N]   per-run stage table; --tail adds each executing worker's last N events
  report <trial.toml> [--no-synthesis]   re-render run reports and the matrix report
  export <trial.toml|matrix-dir> --run ID [--dest DIR] [--force]   shareable copy of one run: report.html,
                                   journey.html, asks.md with the worker's directories relabeled
  console [open] <trial>           local web page to review, start and watch a trial (background server)
  console demo <dir>               copy a simulated trial (no agent or model) and open it in the console
  console list | stop [--all]      running console servers; stopping one never stops a run
  console since|wait|note|start|again|state <trial>   the coordinating agent's view of the same trial
  plugins                          list registered harness/auth/runner/check plugins
  ci <trial.toml> [--output DIR]   validate, doctor, run and decide passed|failed|inconclusive;
                                   writes ci-result.json and junit.xml (never waits for input)
  version | completion bash|zsh|fish

--json on init, validate, doctor, run, status, report, export and plugins prints one JSON
document for agents and pipelines (see docs/GO-CLI.md).
`

// ConsoleActions are the console subcommands; anything else after
// `ajx console` is a trial (`ajx console trials/x` opens it).
var ConsoleActions = []string{"open", "serve", "stop", "list", "state", "since", "wait", "note", "start", "again", "demo"}

type cmdFunc func(*Namespace) (int, error)

var handlers = map[*Parser]cmdFunc{}

func on(p *Parser, f cmdFunc) *Parser {
	handlers[p] = f
	return p
}

func jsonFlag(p *Parser, what string) {
	p.Flag("--json", "print "+what+" as one JSON document (schema_version "+fmt.Sprint(SchemaVersion)+")")
}

// BuildParser defines every command and option.
func BuildParser() *Parser {
	p := NewParser("ajx", description)
	p.RawDesc = true
	p.Version = "AJX " + ajx.Version
	p.Add(&Arg{Flags: []string{"--version"}, Dest: "version", Kind: VersionFlag,
		Help: "show program's version number and exit"})
	sub := p.Subcommands("cmd", true)

	s := on(sub.Add("init", ""), cmdInit)
	s.Add(&Arg{Dest: "dir", Deferred: true})
	s.Add(&Arg{Flags: []string{"--product"}, Deferred: true, Help: ""})
	s.Opt("--id", "", "")
	s.Flag("--force", "")
	s.Add(&Arg{Flags: []string{"--harness"}, Default: "claude-code"})
	s.Opt("--reporter", "", "")
	s.Opt("--model", "", "worker model (default: the harness default)")
	s.Opt("--reporter-model", "MODEL", "reporter model (default: the harness default)")
	s.Opt("--task", "TEXT", "the outcome the user wants, written into task-prompt.md")
	s.Add(&Arg{Flags: []string{"--material"}, Kind: Append, Metavar: "TEXT",
		Help: "a URL or document the worker may use (repeatable)"})
	s.Add(&Arg{Flags: []string{"--check"}, Kind: Append, Metavar: "CHECK",
		Help: "success check: a shell command, file:GLOB or http:URL (repeatable)"})
	s.IntOpt("--timeout", "SECONDS", "worker time limit (default 1800)", nil)
	s.IntOpt("--repetitions", "N", "attempts per cell (default 1)", nil)
	s.Opt("--fixture", "DIR", "starting files copied into every fresh workspace")
	s.Opt("--environment", "NAME", "local (default) or container")
	s.Opt("--image", "IMAGE", "immutable local image id for --environment container")
	s.Flag("--wizard", "ask for the trial's settings even when flags are given (needs a terminal)")
	s.Flag("--no-input", "never prompt; missing required flags are an error")
	s.Flag("--yes", "same as --no-input")
	jsonFlag(s, "what was written")

	s = on(sub.Add("example", ""), cmdExample)
	s.Pos("dir", "")

	s = on(sub.Add("plugins", ""), cmdPlugins)
	jsonFlag(s, "the plugins")

	s = on(sub.Add("doctor", ""), cmdDoctor)
	s.OptPos("trial", "")
	jsonFlag(s, "the readiness report")

	s = on(sub.Add("validate", ""), cmdValidate)
	s.Pos("trial", "")
	s.Opt("--cells", "", "")
	jsonFlag(s, "the plan")

	s = on(sub.Add("run", ""), cmdRun)
	s.Pos("trial", "")
	s.Opt("--cells", "", "")
	s.Opt("--stages", "", "")
	s.Flag("--dry-run", "")
	s.Flag("--keep-workspace", "")
	s.Flag("--no-synthesis", "")
	s.Opt("--expect-plan", "SHA", "refuse to start unless the trial still matches this approved plan id (set by `ajx console`)")
	jsonFlag(s, "the final summary (progress lines go to stderr)")

	s = on(sub.Add("status", ""), cmdStatus)
	s.Pos("trial", "")
	s.IntOpt("--tail", "N", "after the table, the last N worker events of each run still executing (of every run when none is)", nil)
	jsonFlag(s, "the run table")

	s = on(sub.Add("report", ""), cmdReport)
	s.Pos("trial", "")
	s.Flag("--no-synthesis", "")
	jsonFlag(s, "the rendered files")

	s = on(sub.Add("export", ""), cmdExport)
	s.Pos("trial", "")
	s.Add(&Arg{Flags: []string{"--run"}, Required: true, Metavar: "RUN_ID", Help: "run id, for example my-cell-r1"})
	s.Opt("--dest", "DIR", "default <output>/share/<run-id>")
	s.Flag("--force", "overwrite the files in a non-empty --dest")
	jsonFlag(s, "the export summary")

	s = sub.Add("console", "local web page for a trial; see `ajx console --help`")
	acts := s.Subcommands("action", true)
	c := on(acts.Add("open", "open (or reuse) this trial's console; the server runs in the background"), cmdConsole)
	c.Pos("trial", "trial.toml or the directory holding it")
	c.IntOpt("--port", "", "default: a free port", 0)
	c.Flag("--no-open", "print the link without opening a browser")
	c.Flag("--foreground", "serve in this terminal until Ctrl-C")
	c = on(acts.Add("demo", "copy a simulated trial (no agent or model) into DIR and open it"), cmdConsole)
	c.Pos("dir", "")
	c.Flag("--force", "")
	c.Flag("--no-open", "")
	c.IntOpt("--port", "", "", 0)
	c = on(acts.Add("serve", "internal: serve in this process (`open` starts it in the background)"), cmdConsole)
	c.Pos("trial", "")
	c.IntOpt("--port", "", "", 0)
	c.Flag("--open", "")
	c = on(acts.Add("stop", "stop this trial's console server, or every one with --all (never a run)"), cmdConsole)
	c.Description = "Stop console servers. Runs keep going; reopen a console with `ajx console open`."
	c.OptPos("trial", "trial.toml or its directory; `all` is the same as --all")
	c.Flag("--all", "stop every console server running for you")
	c = on(acts.Add("list", "console servers running for you"), cmdConsole)
	c.Flag("--json", "")
	c = on(acts.Add("state", "everything the page shows, as JSON"), cmdConsole)
	c.Pos("trial", "")
	c = on(acts.Add("since", "for agents: what changed after --cursor, as JSON"), cmdConsole)
	c.Pos("trial", "")
	c.IntOpt("--cursor", "", "", 0)
	c = on(acts.Add("wait", "for agents: block until something changes, then print `since`"), cmdConsole)
	c.Pos("trial", "")
	c.IntOpt("--cursor", "", "", 0)
	c.IntOpt("--timeout", "", "seconds (default 90; keep under your tool-call limit)", 90)
	c = on(acts.Add("note", "for agents: post a short explanation to the page's activity list"), cmdConsole)
	c.Pos("trial", "")
	c.Pos("text", "")
	c.IntOpt("--reply-to", "N", "", nil)
	c = on(acts.Add("start", "for agents: start after the user approved in chat (check, approve, launch)"), cmdConsole)
	c.Pos("trial", "")
	c.Add(&Arg{Flags: []string{"--quote"}, Required: true, Help: "the user's approval, word for word"})
	c.Add(&Arg{Flags: []string{"--plan"}, Required: true, Help: "plan id from `since` (its first 7+ characters); refuses a plan that changed after the user saw it"})
	c.Flag("--accept-warnings", "only when the user accepted the readiness warnings listed by `since`, in their own words")
	c = on(acts.Add("again", "for agents: run a finished trial again after the user asked (new attempt)"), cmdConsole)
	c.Pos("trial", "")
	c.Add(&Arg{Flags: []string{"--quote"}, Required: true, Help: "the user's request, word for word"})
	c.Add(&Arg{Flags: []string{"--plan"}, Required: true, Help: "plan id from `since`; refuses a plan that changed meanwhile"})
	where := c.MutexGroup(true)
	where.Add(c, &Arg{Flags: []string{"--same-folder"}, Kind: StoreTrue, Default: false,
		Help: "one more attempt in this output folder, same settings (results stay comparable)"})
	where.Add(c, &Arg{Flags: []string{"--fresh-folder"}, Kind: StoreTrue, Default: false,
		Help: "a new, empty output folder (changed settings)"})
	c.Flag("--accept-warnings", "only when the user accepted the readiness warnings listed by `since`, in their own words")

	s = on(sub.Add("ci", "headless evaluation for pipelines: validate, doctor, run, decide; see docs/GO-CLI.md"), cmdCI)
	s.Pos("trial", "")
	s.Opt("--output", "DIR", "output folder for this evaluation (default: the trial's output_dir); must be fresh unless --allow-reuse")
	s.Opt("--junit", "FILE", "JUnit XML path (default <output>/junit.xml)")
	s.Opt("--result", "FILE", "result JSON path (default <output>/ci-result.json)")
	s.Opt("--policy", "FILE", "acceptance policy (acceptance.toml)")
	s.Opt("--min-success-rate", "R", "verified-success rate each cell needs, 0..1 (default 1)")
	s.Flag("--require-cleanup", "fail when any attempt's cleanup is not confirmed")
	s.Flag("--allow-reuse", "resume an output folder that already holds attempts")
	s.IntOpt("--timeout-grace", "S", "after a cancel signal, seconds to wait for cleanup before writing the result and exiting (default: wait)", nil)
	s.Opt("--cells", "", "only these cells (or model groups)")
	s.Flag("--no-synthesis", "skip the model-written matrix synthesis")
	s.Flag("--skip-doctor", "do not stop when doctor reports problems (still recorded)")

	on(sub.Add("version", "print the AJX version, build and skill fingerprint"), cmdVersion).Flag("--json", "")
	s = on(sub.Add("completion", "print a shell completion script"), cmdCompletion)
	s.Pos("shell", "bash, zsh or fish")
	return p
}

// Main runs the CLI and returns the exit code.
func Main(argv []string) int {
	argv = append([]string(nil), argv...)
	if len(argv) >= 2 && argv[0] == "console" && !util.Contains(ConsoleActions, argv[1]) && !strings.HasPrefix(argv[1], "-") {
		argv = append([]string{"console", "open"}, argv[1:]...) // `ajx console trials/x` opens the console
	}
	p := BuildParser()
	ns, err := p.Parse(argv)
	if err != nil {
		var ex *Exit
		var ue *UsageError
		switch {
		case errors.As(err, &ex):
			fmt.Fprint(Stdout, ex.Text)
			return ex.Code
		case errors.As(err, &ue):
			PrintUsageError(Stderr, ue)
			return 2
		}
		fmt.Fprintln(Stderr, err)
		return 2
	}
	f := handlers[ns.Leaf]
	if f == nil {
		return 2
	}
	resolveTrialDir(ns)
	code, err := f(ns)
	if err == nil {
		return code
	}
	return reportError(ns, err)
}

// ExitCode maps an error to the CLI's exit code and stderr line.
func ExitCode(err error) (int, string, string) {
	var se *spec.Error
	var ce *console.Error
	var le *runner.LockedError
	var ue *UsageError
	switch {
	case errors.As(err, &ue):
		return 2, "usage", ue.Msg
	case errors.As(err, &se):
		return 2, "trial_file_error", "trial file error: " + se.Error()
	case errors.As(err, &ce):
		return 2, "console_error", "console: " + ce.Error()
	case errors.As(err, &le):
		return 3, "locked", "locked: " + le.Error()
	case util.Interrupted(err):
		return 130, "interrupted", "ajx: interrupted. Captured evidence is kept; rerun the same command to finish the remaining stages."
	}
	return 1, "error", "ajx: error: " + err.Error()
}

func reportError(ns *Namespace, err error) int {
	var ue *UsageError
	if errors.As(err, &ue) {
		PrintUsageError(Stderr, ue)
		return 2
	}
	code, kind, line := ExitCode(err)
	fmt.Fprintln(Stderr, line)
	if ns != nil && ns.Bool("json") && ns.Values["cmd"] != "console" {
		msg := line
		if i := strings.Index(line, ": "); i >= 0 && kind != "interrupted" && kind != "error" {
			msg = line[i+2:]
		}
		emitJSON(util.M{"schema_version": SchemaVersion, "command": ns.Str("cmd"), "ok": false,
			"exit_code": code, "error": util.M{"type": kind, "message": msg}})
	}
	return code
}

func outf(format string, a ...any) { fmt.Fprintf(Stdout, format, a...) }
func outln(a ...any)               { fmt.Fprintln(Stdout, a...) }

func fail(msg string) (int, error) {
	fmt.Fprintln(Stderr, msg)
	return 1, nil
}

// emitJSON prints a document with two-space indentation.
func emitJSON(v any) {
	b, err := util.MarshalIndent(v)
	if err != nil {
		fmt.Fprintln(Stderr, "ajx: cannot encode JSON:", err)
		return
	}
	Stdout.Write(b)
}

// pyDump is Python's print(json.dumps(data, indent=2)) (ASCII-escaped).
func pyDump(v any, ensureASCII bool) {
	b, err := util.MarshalIndent(v)
	if err != nil {
		fmt.Fprintln(Stderr, "ajx: cannot encode JSON:", err)
		return
	}
	if ensureASCII {
		b = []byte(asciiEscape(string(b)))
	}
	Stdout.Write(b)
}

func asciiEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 128:
			b.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

// shortHome shows a path with ~ for the home directory (Python console._short).
func shortHome(path string) string {
	home := util.Home()
	if home != "" && (path == home || strings.HasPrefix(path, home+string(os.PathSeparator))) {
		return "~" + path[len(home):]
	}
	return path
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		out = append(out, strings.TrimSpace(part))
	}
	return out
}

// trialFileCommands take a trial file; a directory holding trial.toml is
// accepted as that file (the console already accepts either).
var trialFileCommands = map[string]bool{"doctor": true, "validate": true, "run": true, "report": true, "ci": true}

func resolveTrialDir(ns *Namespace) {
	cmd := ns.Str("cmd")
	if !trialFileCommands[cmd] {
		return
	}
	p := ns.Str("trial")
	if p != "" && isDir(p) && isFile(filepath.Join(p, "trial.toml")) {
		ns.Values["trial"] = filepath.Join(p, "trial.toml")
	}
}
