package ci

import (
	"fmt"
	"strings"

	"github.com/royosherove/ajx/internal/util"
)

func xmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		case '\t', '\n', '\r':
			b.WriteRune(r)
		default:
			if r < 0x20 || r == 0xFFFE || r == 0xFFFF {
				b.WriteString("�")
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

type testcase struct {
	class, name string
	seconds     float64
	failure     [2]string // type, message
	errorTag    [2]string
	skipped     string
	out         string
}

func (t testcase) xml() string {
	var b strings.Builder
	fmt.Fprintf(&b, `    <testcase classname="%s" name="%s" time="%.3f"`, xmlEscape(t.class), xmlEscape(t.name), t.seconds)
	if t.failure[0] == "" && t.errorTag[0] == "" && t.skipped == "" && t.out == "" {
		b.WriteString("/>\n")
		return b.String()
	}
	b.WriteString(">\n")
	if t.failure[0] != "" {
		fmt.Fprintf(&b, "      <failure type=\"%s\" message=\"%s\">%s</failure>\n", xmlEscape(t.failure[0]),
			xmlEscape(util.OneLine(t.failure[1], 300)), xmlEscape(t.failure[1]))
	}
	if t.errorTag[0] != "" {
		fmt.Fprintf(&b, "      <error type=\"%s\" message=\"%s\">%s</error>\n", xmlEscape(t.errorTag[0]),
			xmlEscape(util.OneLine(t.errorTag[1], 300)), xmlEscape(t.errorTag[1]))
	}
	if t.skipped != "" {
		fmt.Fprintf(&b, "      <skipped message=\"%s\"/>\n", xmlEscape(t.skipped))
	}
	if t.out != "" {
		fmt.Fprintf(&b, "      <system-out>%s</system-out>\n", xmlEscape(t.out))
	}
	b.WriteString("    </testcase>\n")
	return b.String()
}

type suite struct {
	name  string
	cases []testcase
}

func (s suite) counts() (tests, failures, errs, skipped int, secs float64) {
	for _, c := range s.cases {
		tests++
		if c.failure[0] != "" {
			failures++
		}
		if c.errorTag[0] != "" {
			errs++
		}
		if c.skipped != "" {
			skipped++
		}
		secs += c.seconds
	}
	return
}

// JUnit renders the result as JUnit XML: one testcase per planned run
// (ajx.<trial>.runs) and one per acceptance check (ajx.<trial>.acceptance).
func JUnit(result util.M) string {
	trialID := util.OrStr(util.Str(util.Get(result, "trial", "id")), "trial")
	runsSuite := suite{name: "ajx." + trialID + ".runs"}
	for _, r := range util.Maps(result["runs"]) {
		tc := testcase{class: "ajx." + trialID + "." + util.Str(r["cell"]), name: util.Str(r["run_id"])}
		if f, ok := util.Num(r["execute_seconds"]); ok {
			tc.seconds = f
		}
		ver := util.MapOr(r["verification"])
		var outLines []string
		for _, c := range util.Maps(ver["checks"]) {
			mark := "FAILED"
			if util.Truthy(c["passed"]) {
				mark = "passed"
			}
			outLines = append(outLines, fmt.Sprintf("check %s: %s", util.Str(c["name"]), mark))
		}
		for _, e := range util.Maps(r["stage_errors"]) {
			outLines = append(outLines, fmt.Sprintf("stage %s error: %s", util.Str(e["stage"]), util.Str(e["error"])))
		}
		cl := util.MapOr(r["cleanup"])
		outLines = append(outLines, "cleanup: "+util.Str(cl["status"]))
		if rep := util.Str(util.Get(r, "artifacts", "report")); rep != "" {
			outLines = append(outLines, "report: "+rep)
		}
		tc.out = strings.Join(outLines, "\n")
		switch util.S(r["outcome"]) {
		case "passed":
		case "failed":
			tc.failure = [2]string{"verification_failed", fmt.Sprintf("%s of %s check(s) passed", util.Str(ver["passed"]), util.Str(ver["total"]))}
		case "missing":
			tc.errorTag = [2]string{"run_missing", "no attempt was recorded for this planned run"}
		case "untrusted":
			tc.errorTag = [2]string{"verification_untrusted", "the checks printed credential or permission errors; their outcome is not trustworthy"}
		case "not_run":
			tc.errorTag = [2]string{"verification_missing", "no verification checks ran"}
		default:
			msg := "the attempt did not produce a verified outcome"
			for _, e := range util.Maps(r["stage_errors"]) {
				msg = util.Str(e["stage"]) + ": " + util.Str(e["error"])
				break
			}
			tc.errorTag = [2]string{util.OrStr(util.S(r["outcome"]), "error"), msg}
		}
		runsSuite.cases = append(runsSuite.cases, tc)
	}
	acc := suite{name: "ajx." + trialID + ".acceptance"}
	class := "ajx." + trialID + ".acceptance"
	byCode := map[string][]util.M{}
	for _, r := range util.Maps(result["reasons"]) {
		byCode[util.S(r["code"])] = append(byCode[util.S(r["code"])], r)
	}
	target := util.Get(result, "policy", "min_success_rate")
	for _, c := range util.Maps(result["cells"]) {
		tc := testcase{class: class, name: fmt.Sprintf("cell %s success rate >= %s", util.Str(c["cell"]), util.Str(target))}
		msg := fmt.Sprintf("%s of %s attempt(s) verified succeeded (%s unknown)", util.Str(c["passed"]), util.Str(c["planned"]), util.Str(c["unknown"]))
		switch util.S(c["status"]) {
		case Failed:
			tc.failure = [2]string{"success_rate_below_target", msg}
		case Inconclusive:
			tc.errorTag = [2]string{"insufficient_evidence", msg}
		default:
			tc.out = msg
		}
		acc.cases = append(acc.cases, tc)
	}
	checks := testcase{class: class, name: "verification checks defined"}
	if len(byCode["no_checks"]) > 0 {
		checks.errorTag = [2]string{"no_checks", util.S(byCode["no_checks"][0]["message"])}
	}
	acc.cases = append(acc.cases, checks)
	cleanup := testcase{class: class, name: "cleanup confirmed"}
	if rs := byCode["cleanup_not_confirmed"]; len(rs) > 0 {
		var msgs []string
		for _, r := range rs {
			msgs = append(msgs, util.S(r["message"]))
		}
		switch util.S(rs[0]["severity"]) {
		case SevFailure:
			cleanup.failure = [2]string{"cleanup_not_confirmed", strings.Join(msgs, "\n")}
		case SevInconclusive:
			cleanup.errorTag = [2]string{"cleanup_not_confirmed", strings.Join(msgs, "\n")}
		default:
			cleanup.out = "not required by the policy:\n" + strings.Join(msgs, "\n")
		}
	}
	acc.cases = append(acc.cases, cleanup)
	if util.Truthy(util.Get(result, "policy", "require_reports")) {
		reports := testcase{class: class, name: "reports complete"}
		if rs := byCode["reporting_incomplete"]; len(rs) > 0 {
			reports.errorTag = [2]string{"reporting_incomplete", util.S(rs[0]["message"])}
		}
		acc.cases = append(acc.cases, reports)
	}
	evaluation := testcase{class: class, name: "evaluation completed"}
	for _, code := range []string{"invalid_config", "output_not_fresh", "not_ready", "locked", "interrupted", "run_error", "artifact_write_error", "incomplete"} {
		if rs := byCode[code]; len(rs) > 0 {
			evaluation.errorTag = [2]string{code, util.S(rs[0]["message"])}
			break
		}
	}
	acc.cases = append(acc.cases, evaluation)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	tt, tf, te, ts, tsec := 0, 0, 0, 0, 0.0
	for _, s := range []suite{runsSuite, acc} {
		t, f, e, sk, sec := s.counts()
		tt, tf, te, ts, tsec = tt+t, tf+f, te+e, ts+sk, tsec+sec
	}
	fmt.Fprintf(&b, `<testsuites name="ajx %s" tests="%d" failures="%d" errors="%d" skipped="%d" time="%.3f">`+"\n",
		xmlEscape(trialID), tt, tf, te, ts, tsec)
	for _, s := range []suite{runsSuite, acc} {
		t, f, e, sk, sec := s.counts()
		fmt.Fprintf(&b, `  <testsuite name="%s" tests="%d" failures="%d" errors="%d" skipped="%d" time="%.3f">`+"\n",
			xmlEscape(s.name), t, f, e, sk, sec)
		fmt.Fprintf(&b, "    <properties>\n      <property name=\"decision\" value=\"%s\"/>\n      <property name=\"plan_sha256\" value=\"%s\"/>\n    </properties>\n",
			xmlEscape(util.Str(result["decision"])), xmlEscape(util.Str(util.Get(result, "plan", "sha256"))))
		for _, c := range s.cases {
			b.WriteString(c.xml())
		}
		b.WriteString("  </testsuite>\n")
	}
	b.WriteString("</testsuites>\n")
	return b.String()
}
