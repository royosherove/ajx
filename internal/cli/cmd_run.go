package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/royosherove/ajx/internal/console"
	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

// hangupOutput keeps cleanup output from failing once the terminal is gone
// (Python _hangup): stdout and stderr now write to /dev/null.
func hangupOutput() {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return
	}
	_ = dup2(int(devnull.Fd()), 1)
	_ = dup2(int(devnull.Fd()), 2)
}

// WithSignals runs fn with a context that SIGINT, SIGTERM or SIGHUP cancels.
// A launcher that left SIGINT ignored (a background job of a non-interactive
// shell) does not defeat it: subscribing re-enables the signal. Later
// signals are left to the runner, which uses them to cut one cleanup step
// short, and never kill the process before cleanup finishes.
func WithSignals(fn func(ctx context.Context) (int, error)) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopPipe := survivePipe()
	defer stopPipe()
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-ch:
				if sig == syscall.SIGHUP {
					hangupOutput()
				}
				cancel()
			case <-done:
				return
			}
		}
	}()
	defer func() {
		close(done)
		signal.Stop(ch)
	}()
	return fn(ctx)
}

// survivePipe keeps a closed stdout or stderr (`ajx run | head`) from
// killing the process before cleanup: writes then fail with EPIPE instead.
// The signal is caught, not ignored, so workers still start with the default
// SIGPIPE behavior (as Python's subprocess restores it).
func survivePipe() func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGPIPE)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// runMatrix is replaceable in tests.
var runMatrix = runner.RunMatrix

func cmdRun(ns *Namespace) (int, error) {
	return WithSignals(func(ctx context.Context) (int, error) { return runCmd(ctx, ns) })
}

func runCmd(ctx context.Context, ns *Namespace) (int, error) {
	stages := splitList(ns.Str("stages"))
	sp, err := spec.Load(ns.Str("trial"), spec.LoadOpts{CleanupOnly: runner.CleanupOnly(stages)})
	if err != nil {
		return 2, err
	}
	if expected := ns.Str("expect_plan"); expected != "" {
		sha, err := console.PlanSha(util.S(sp["path"]), sp)
		if err != nil {
			return 2, err
		}
		if sha != expected {
			return 2, spec.Errorf("the trial changed after it was approved (trial.toml, the task prompt, the starting " +
				"files or the trial's plugins); review it and approve it again")
		}
	}
	if !runner.CleanupOnly(stages) {
		if problems := pythonPluginProblems(sp, nil); len(problems) > 0 {
			return 2, spec.Errorf("%s", strings.Join(problems, "; "))
		}
	}
	jsonMode := ns.Bool("json")
	logTo := Stdout
	if jsonMode {
		logTo = Stderr
	}
	log := func(m string) { fmt.Fprintf(logTo, "[%s] %s\n", time.Now().Format("15:04:05"), m) }
	meta, err := runMatrix(ctx, sp, runner.MatrixOpts{OnlyCells: splitList(ns.Str("cells")), Stages: stages,
		KeepWorkspace: ns.Bool("keep_workspace"), DryRun: ns.Bool("dry_run"), Synthesize: !ns.Bool("no_synthesis"), Log: log})
	if err != nil {
		return 1, err
	}
	if ns.Bool("dry_run") {
		if jsonMode {
			emitJSON(util.M{"schema_version": SchemaVersion, "command": "run", "ok": true, "dry_run": true, "exit_code": 0,
				"plan": meta})
			return 0, nil
		}
		pyDump(meta, true)
		return 0, nil
	}
	out := util.S(util.Get(sp, "trial", "output_dir"))
	if !jsonMode {
		outf("\nmatrix report: %s\nmarkdown:      %s\n", filepath.Join(out, "index.html"), filepath.Join(out, "matrix.md"))
		return status(out, 0, false)
	}
	code, doc, err := statusDoc(out, nil)
	if err != nil {
		return 1, err
	}
	emitJSON(util.M{"schema_version": SchemaVersion, "command": "run", "ok": code == 0, "exit_code": code,
		"dry_run": false, "output_dir": out, "matrix_report": filepath.Join(out, "index.html"),
		"markdown": filepath.Join(out, "matrix.md"), "order": meta["order"], "status": doc})
	return code, nil
}
