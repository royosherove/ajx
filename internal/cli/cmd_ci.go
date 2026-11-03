package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/royosherove/ajx/internal/ci"
	"github.com/royosherove/ajx/internal/util"
)

// exit is replaceable in tests.
var exit = os.Exit

func cmdCI(ns *Namespace) (int, error) {
	var rate *float64
	if v := ns.Str("min_success_rate"); v != "" {
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 2, usageErr(ns, "argument --min-success-rate: invalid float value: "+pyRepr(v))
		}
		rate = &f
	}
	say := func(m string) { fmt.Fprintln(Stdout, m) }
	log := func(m string) { fmt.Fprintf(Stdout, "[%s] %s\n", time.Now().Format("15:04:05"), m) }
	sess := ci.New(ci.Options{
		Trial: ns.Str("trial"), Output: ns.Str("output"), JUnit: ns.Str("junit"), Result: ns.Str("result"),
		Policy: ns.Str("policy"), MinSuccessRate: rate, RequireCleanup: ns.Bool("require_cleanup"),
		AllowReuse: ns.Bool("allow_reuse"), Cells: splitList(ns.Str("cells")), NoSynthesis: ns.Bool("no_synthesis"),
		SkipDoctor: ns.Bool("skip_doctor"), Log: log, Say: say,
	}, ci.Hooks{
		Validate: Validate,
		Doctor: func(trial string) (int, []string, error) {
			code, lines, _, err := Doctor(trial, true)
			return code, lines, err
		},
		Run: runMatrix,
	})
	grace := ns.IntPtr("timeout_grace")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopPipe := survivePipe()
	defer stopPipe()
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		first := true
		for {
			select {
			case sig := <-ch:
				if sig == syscall.SIGHUP {
					hangupOutput()
				}
				cancel()
				if first && grace != nil && *grace >= 0 {
					g := *grace
					time.AfterFunc(time.Duration(g)*time.Second, func() {
						code := sess.Abort(g)
						fmt.Fprintln(Stderr, ci.Summary(sess.Result()))
						if p, _ := sess.Paths(); p != "" {
							fmt.Fprintln(Stderr, "ci: result "+p)
						}
						exit(code)
					})
				}
				first = false
			case <-done:
				return
			}
		}
	}()
	code := sess.Run(ctx)
	close(done)
	signal.Stop(ch)

	result := sess.Result()
	say(ci.Summary(result))
	for _, r := range util.Maps(result["reasons"]) {
		if util.S(r["severity"]) != ci.SevWarning {
			say("ci:   " + util.S(r["code"]) + ": " + util.S(r["message"]))
		}
	}
	if cov := util.MapOr(result["coverage"]); len(cov) > 0 {
		say(fmt.Sprintf("ci: %s run(s) planned, %s passed, %s failed, %s without a trustworthy outcome; cleanup confirmed for %s",
			num(cov["planned"]), num(cov["passed"]), num(cov["failed"]), num(cov["unknown"]), num(cov["cleanup_confirmed"])))
	}
	resultPath, junitPath := sess.Paths()
	if resultPath != "" {
		say("ci: result " + resultPath)
	} else {
		say("ci: result not written (the output folder is unknown; pass --output or --result)")
	}
	if junitPath != "" {
		say("ci: junit  " + junitPath)
	}
	if code == ci.ExitInterrupted {
		fmt.Fprintln(Stderr, "ajx: interrupted. Captured evidence is kept; rerun the same command to finish the remaining stages.")
	}
	return code, nil
}
