// Package measure ports lib/ajx/measure.py: validate normalized AJX event
// exports and compute reconciled phase costs.
package measure

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/royosherove/ajx/internal/util"
)

// Doc is the measure tool's description (measure.py's module docstring).
const Doc = "Validate normalized AJX event exports and compute reconciled phase costs."

// Error is a measurement validation failure (Python ValueError/KeyError/TypeError).
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// keyError mirrors str(KeyError(key)).
func keyError(key string) error { return &Error{Msg: pyQuote(key)} }

func require(cond bool, format string, a ...any) error {
	if cond {
		return nil
	}
	return fail(format, a...)
}

func field(m util.M, key string) (any, error) {
	v, ok := m[key]
	if !ok {
		return nil, keyError(key)
	}
	return v, nil
}

// Timestamp parses an offset-aware ISO-8601 timestamp (measure.timestamp).
func Timestamp(value any) (time.Time, error) {
	s, ok := value.(string)
	if !ok {
		return time.Time{}, fail("Timestamp must be a string")
	}
	t, aware, err := ParseISO(s)
	if err != nil {
		return time.Time{}, &Error{Msg: err.Error()}
	}
	if !aware {
		return time.Time{}, fail("Timestamp needs timezone: %s", s)
	}
	return t, nil
}

// count accepts a nonnegative integer: an int, an integral JSON number
// decoded in-process, or a json.Number integer literal (Python type(v) is int).
func count(value any, label string) (int64, error) {
	bad := fail("%s must be a nonnegative integer", label)
	switch t := value.(type) {
	case bool:
		return 0, bad
	case json.Number:
		n, err := strconv.ParseInt(string(t), 10, 64)
		if err != nil || n < 0 {
			return 0, bad
		}
		return n, nil
	case float64:
		if t != float64(int64(t)) || t < 0 {
			return 0, bad
		}
		return int64(t), nil
	}
	if f, ok := util.Num(value); ok {
		if f < 0 || f != float64(int64(f)) {
			return 0, bad
		}
		return int64(f), nil
	}
	return 0, bad
}

func identifier(value any, label string) (string, error) {
	s, ok := value.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fail("%s must be nonempty", label)
	}
	return s, nil
}

func asMap(v any) (util.M, bool) {
	m, ok := v.(util.M)
	return m, ok
}

type phase struct {
	rec        util.M
	start, end time.Time
}

type prepared struct {
	id         string
	definition util.M
	phases     []*phase
	seen       map[string]bool
	expected   map[string]int64
}

// Measure validates a manifest and its events and computes per-phase costs.
// Errors are *Error values carrying Python's message text.
func Measure(manifest any, events []any) (util.M, error) {
	i := 0
	return measureIter(manifest, func() (any, bool, error) {
		if i >= len(events) {
			return nil, false, nil
		}
		i++
		return events[i-1], true, nil
	})
}

var statusFields = [][2]string{{"usage_status", "expected_output_tokens"}, {"tool_status", "expected_tool_calls"}}

func measureIter(manifest any, next func() (any, bool, error)) (util.M, error) {
	man, ok := asMap(manifest)
	if !ok {
		return nil, fail("Manifest must be an object")
	}
	sessions := util.List(man["sessions"])
	if len(sessions) == 0 {
		return nil, fail("Manifest needs sessions")
	}
	var order []*prepared
	byID := map[string]*prepared{}
	for _, raw := range sessions {
		session, ok := asMap(raw)
		if !ok {
			return nil, fail("Each session must be an object")
		}
		idv, err := field(session, "id")
		if err != nil {
			return nil, err
		}
		sid, err := identifier(idv, "Session ID")
		if err != nil {
			return nil, err
		}
		if byID[sid] != nil {
			return nil, fail("Duplicate session: %s", sid)
		}
		sv, err := field(session, "start")
		if err != nil {
			return nil, err
		}
		start, err := Timestamp(sv)
		if err != nil {
			return nil, err
		}
		ev, err := field(session, "end")
		if err != nil {
			return nil, err
		}
		end, err := Timestamp(ev)
		if err != nil {
			return nil, err
		}
		if !start.Before(end) {
			return nil, fail("Invalid session window: %s", sid)
		}
		cur := &prepared{id: sid, definition: session, seen: map[string]bool{}, expected: map[string]int64{}}
		for _, fe := range statusFields {
			status := session[fe[0]]
			s, _ := status.(string)
			if s != "complete" && s != "partial" && s != "unavailable" {
				return nil, fail("%s: invalid %s", sid, fe[0])
			}
			if s == "complete" {
				n, err := count(session[fe[1]], sid+"."+fe[1])
				if err != nil {
					return nil, err
				}
				cur.expected[fe[1]] = n
			} else if _, has := session[fe[1]]; has {
				return nil, fail("%s: %s requires complete status", sid, fe[1])
			}
		}
		rawPhases := util.List(session["phases"])
		if len(rawPhases) == 0 {
			return nil, fail("%s: phases required", sid)
		}
		ids := map[string]bool{}
		cursor := start
		for _, rp := range rawPhases {
			p, ok := asMap(rp)
			if !ok {
				return nil, fail("Each phase must be an object")
			}
			pv, err := field(p, "id")
			if err != nil {
				return nil, err
			}
			pid, err := identifier(pv, "Phase ID")
			if err != nil {
				return nil, err
			}
			if ids[pid] {
				return nil, fail("%s: duplicate phase %s", sid, pid)
			}
			ids[pid] = true
			psv, err := field(p, "start")
			if err != nil {
				return nil, err
			}
			pstart, err := Timestamp(psv)
			if err != nil {
				return nil, err
			}
			pev, err := field(p, "end")
			if err != nil {
				return nil, err
			}
			pend, err := Timestamp(pev)
			if err != nil {
				return nil, err
			}
			if !pstart.Equal(cursor) {
				return nil, fail("%s/%s: phase gap or overlap", sid, pid)
			}
			if !(pstart.Before(pend) && !pend.After(end)) {
				return nil, fail("%s/%s: invalid phase window", sid, pid)
			}
			scope, _ := p["scope"].(string)
			switch scope {
			case "task", "report", "verification", "human_wait", "excluded":
			default:
				return nil, fail("%s/%s: invalid scope", sid, pid)
			}
			cur.phases = append(cur.phases, &phase{start: pstart, end: pend, rec: util.M{
				"id":                     pid,
				"scope":                  scope,
				"start":                  p["start"],
				"end":                    p["end"],
				"elapsed_seconds":        seconds(pend.Sub(pstart)),
				"observed_output_tokens": int64(0),
				"observed_tool_calls":    int64(0),
				"usage_records":          int64(0),
			}})
			cursor = pend
		}
		if !cursor.Equal(end) {
			return nil, fail("%s: phases do not cover the session", sid)
		}
		order = append(order, cur)
		byID[sid] = cur
	}

	for {
		raw, more, err := next()
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		event, ok := asMap(raw)
		if !ok {
			return nil, fail("Each event must be an object")
		}
		sv, err := field(event, "session_id")
		if err != nil {
			return nil, err
		}
		sid, err := identifier(sv, "Event session ID")
		if err != nil {
			return nil, err
		}
		cur := byID[sid]
		if cur == nil {
			return nil, fail("Unknown event session: %s", sid)
		}
		iv, err := field(event, "id")
		if err != nil {
			return nil, err
		}
		eid, err := identifier(iv, "Event ID")
		if err != nil {
			return nil, err
		}
		if cur.seen[eid] {
			return nil, fail("%s: duplicate event %s", sid, eid)
		}
		cur.seen[eid] = true
		av, err := field(event, "at")
		if err != nil {
			return nil, err
		}
		at, err := Timestamp(av)
		if err != nil {
			return nil, err
		}
		var ph *phase
		for _, p := range cur.phases {
			if !at.Before(p.start) && at.Before(p.end) {
				ph = p
				break
			}
		}
		if ph == nil {
			return nil, fail("%s/%s: event outside session window", sid, eid)
		}
		kind, err := field(event, "kind")
		if err != nil {
			return nil, err
		}
		switch kind {
		case "usage":
			if cur.definition["usage_status"] == "unavailable" {
				return nil, fail("%s: usage records contradict unavailable status", sid)
			}
			tv, err := field(event, "output_tokens")
			if err != nil {
				return nil, err
			}
			n, err := count(tv, sid+"/"+eid)
			if err != nil {
				return nil, err
			}
			ph.rec["observed_output_tokens"] = ph.rec["observed_output_tokens"].(int64) + n
			ph.rec["usage_records"] = ph.rec["usage_records"].(int64) + 1
		case "tool_call":
			if cur.definition["tool_status"] == "unavailable" {
				return nil, fail("%s: tool calls contradict unavailable status", sid)
			}
			ph.rec["observed_tool_calls"] = ph.rec["observed_tool_calls"].(int64) + 1
		default:
			return nil, fail("%s/%s: unsupported event kind %s", sid, eid, pyStr(kind))
		}
	}

	results := []any{}
	for _, cur := range order {
		def := cur.definition
		start, _ := Timestamp(def["start"])
		end, _ := Timestamp(def["end"])
		phases := make([]any, len(cur.phases))
		for i, p := range cur.phases {
			phases[i] = p.rec
		}
		result := util.M{"id": cur.id, "start": def["start"], "end": def["end"],
			"elapsed_seconds": seconds(end.Sub(start)), "phases": phases}
		for _, mf := range [][3]string{{"output_tokens", "usage_status", "expected_output_tokens"},
			{"tool_calls", "tool_status", "expected_tool_calls"}} {
			metric, statusField, expected := mf[0], mf[1], mf[2]
			status := def[statusField].(string)
			var observed int64
			for _, p := range cur.phases {
				observed += p.rec["observed_"+metric].(int64)
			}
			if status == "complete" && observed != cur.expected[expected] {
				return nil, fail("%s: %s reconciliation failed: observed %d, expected %d",
					cur.id, metric, observed, cur.expected[expected])
			}
			result[statusField] = status
			if status == "unavailable" {
				result["observed_"+metric] = nil
			} else {
				result["observed_"+metric] = observed
			}
			if status == "complete" {
				result[metric] = observed
			} else {
				result[metric] = nil
			}
			result[metric+"_reconciled"] = status == "complete"
			for _, p := range cur.phases {
				if status == "complete" {
					p.rec[metric] = p.rec["observed_"+metric]
				} else {
					p.rec[metric] = nil
				}
				if status == "unavailable" {
					p.rec["observed_"+metric] = nil
				}
			}
		}
		results = append(results, result)
	}
	return util.M{
		"schema_version": int64(1),
		"method":         "normalized incremental usage; half-open phase windows",
		"sessions":       results,
		"note":           "No combined wall-clock total: sessions may overlap or use different scopes.",
	}, nil
}

// seconds is timedelta.total_seconds() at microsecond resolution.
func seconds(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1e6
}

func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case json.Number:
		return t.String()
	}
	return util.Str(v)
}

// decodeNumbers parses JSON keeping numbers as json.Number, so integer
// literals stay distinguishable from floats (Python json.loads).
func decodeNumbers(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("Extra data")
	}
	return v, nil
}

func eventReader(path string) (func() (any, bool, error), func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<30)
	line := 0
	name := filepath.Base(path)
	next := func() (any, bool, error) {
		for sc.Scan() {
			line++
			text := sc.Text()
			if strings.TrimSpace(text) == "" {
				continue
			}
			v, err := decodeNumbers([]byte(text))
			if err != nil {
				return nil, false, fail("%s:%d: invalid JSON", name, line)
			}
			return v, true, nil
		}
		return nil, false, sc.Err()
	}
	return next, func() { f.Close() }, nil
}

func resolve(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
}

func osError(err error, path string) string {
	if errors.Is(err, fs.ErrNotExist) {
		return "[Errno 2] No such file or directory: " + pyQuote(path)
	}
	return err.Error()
}

const usage = "usage: measure [-h] [--output OUTPUT] manifest events\n"

// Main is measure.py's command line: `measure MANIFEST EVENTS [--output PATH]`.
// It writes the measurement JSON to stdout (or PATH) and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	var pos []string
	output := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprint(stdout, usage+"\n"+Doc+"\n\npositional arguments:\n  manifest\n  events\n\noptions:\n"+
				"  -h, --help       show this help message and exit\n  --output OUTPUT\n")
			return 0
		case a == "--output":
			if i+1 >= len(args) {
				fmt.Fprint(stderr, usage+"measure: error: argument --output: expected one argument\n")
				return 2
			}
			i++
			output = args[i]
		case strings.HasPrefix(a, "--output="):
			output = strings.TrimPrefix(a, "--output=")
		case strings.HasPrefix(a, "-") && a != "-":
			fmt.Fprintf(stderr, usage+"measure: error: unrecognized arguments: %s\n", a)
			return 2
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < 2 {
		missing := []string{"manifest", "events"}[len(pos):]
		fmt.Fprintf(stderr, usage+"measure: error: the following arguments are required: %s\n", strings.Join(missing, ", "))
		return 2
	}
	if len(pos) > 2 {
		fmt.Fprintf(stderr, usage+"measure: error: unrecognized arguments: %s\n", strings.Join(pos[2:], " "))
		return 2
	}
	manifestPath, eventsPath := pos[0], pos[1]
	failed := func(msg string) int {
		fmt.Fprintf(stderr, "AJX measurement failed: %s\n", msg)
		return 1
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return failed(osError(err, manifestPath))
	}
	manifest, err := decodeNumbers(data)
	if err != nil {
		return failed(err.Error())
	}
	next, closeFn, err := eventReader(eventsPath)
	if err != nil {
		return failed(osError(err, eventsPath))
	}
	defer closeFn()
	result, err := measureIter(manifest, next)
	if err != nil {
		return failed(err.Error())
	}
	rendered, err := util.MarshalIndent(result)
	if err != nil {
		return failed(err.Error())
	}
	if output != "" {
		out := resolve(output)
		if out == resolve(manifestPath) || out == resolve(eventsPath) {
			return failed("Output must not overwrite an input")
		}
		if err := os.WriteFile(output, rendered, 0o644); err != nil {
			return failed(osError(err, output))
		}
		return 0
	}
	stdout.Write(rendered)
	return 0
}
