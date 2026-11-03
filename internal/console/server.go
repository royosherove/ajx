package console

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/royosherove/ajx/internal/planedit"
	"github.com/royosherove/ajx/internal/render/pyfmt"
	"github.com/royosherove/ajx/internal/res"
	"github.com/royosherove/ajx/internal/runner"
	"github.com/royosherove/ajx/internal/util"
)

// ---------------------------------------------------------------- server

type app struct {
	trial, token string
	idle         time.Duration
	port         int

	childMu  sync.Mutex
	children map[int]*exec.Cmd

	lastRequest atomic.Int64 // unix nanoseconds
	lock        sync.Mutex   // serializes start/again decisions
	checking    atomic.Bool
	wg          sync.WaitGroup // background actions
	shutdown    func()
}

func (a *app) touch() { a.lastRequest.Store(time.Now().UnixNano()) }

func (a *app) idleFor() time.Duration {
	return time.Duration(time.Now().UnixNano() - a.lastRequest.Load())
}

// watch reaps a command the server launched and records its exit code.
func (a *app) watch(cmd *exec.Cmd, entry M) {
	pid := cmd.Process.Pid
	a.childMu.Lock()
	a.children[pid] = cmd
	a.childMu.Unlock()
	go func() {
		_ = cmd.Wait()
		finishJob(a.trial, entryN(entry), pid, util.ExitCode(cmd.ProcessState), entry["job"])
		a.childMu.Lock()
		delete(a.children, pid)
		a.childMu.Unlock()
	}()
}

func (a *app) childPids() map[int]bool {
	a.childMu.Lock()
	defer a.childMu.Unlock()
	out := map[int]bool{}
	for pid := range a.children {
		out[pid] = true
	}
	return out
}

func (a *app) busy() bool {
	a.childMu.Lock()
	n := len(a.children)
	a.childMu.Unlock()
	return n > 0 || a.checking.Load()
}

// background runs an action after the request returned 202; console errors
// are already in the journal for the page, anything else is recorded.
func (a *app) background(fn func() error) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			if r := recover(); r != nil && exists(a.trial) {
				record(a.trial, "console", "start_blocked", M{"reason": fmt.Sprintf("%s: %s", typeName(r), clip(fmt.Sprint(r), 200))})
			}
		}()
		err := fn()
		if err == nil {
			return
		}
		if _, ok := asConsoleError(err); ok {
			return
		}
		if exists(a.trial) {
			record(a.trial, "console", "start_blocked", M{"reason": fmt.Sprintf("%s: %s", typeName(err), clip(err.Error(), 200))})
		}
	}()
}

const needKey = "this page needs the link printed by `ajx console` (it carries a key)"

type response struct {
	status int
	body   []byte
	ctype  string
	extra  map[string]string
}

func jsonResponse(status int, payload any, extra map[string]string) response {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		buf.Reset()
		status = 500
		_ = json.NewEncoder(&buf).Encode(M{"error": fmt.Sprintf("%s: %s", typeName(err), err.Error())})
	}
	return response{status: status, body: bytes.TrimRight(buf.Bytes(), "\n"), ctype: "application/json; charset=utf-8", extra: extra}
}

func errorResponse(err error) response {
	if ce, ok := asConsoleError(err); ok {
		status := ce.Status
		if status == 0 {
			status = 400
		}
		return jsonResponse(status, M{"error": ce.Msg}, nil)
	}
	return jsonResponse(500, M{"error": fmt.Sprintf("%s: %s", typeName(err), err.Error())}, nil)
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.touch()
	resp := a.dispatch(r)
	h := w.Header()
	h.Set("Server", "AJXConsole/1")
	h.Set("Content-Type", resp.ctype)
	h.Set("Content-Length", strconv.Itoa(len(resp.body)))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	for k, v := range resp.extra {
		h.Set(k, v)
	}
	w.WriteHeader(resp.status)
	_, _ = w.Write(resp.body)
}

func (a *app) dispatch(r *http.Request) (resp response) {
	defer func() {
		if rec := recover(); rec != nil {
			resp = jsonResponse(500, M{"error": fmt.Sprintf("%s: %v", typeName(rec), rec)}, nil)
		}
	}()
	method := r.Method
	if method != "GET" && method != "POST" {
		return jsonResponse(501, M{"error": "Unsupported method (" + method + ")"}, nil)
	}
	if !a.hostOK(r) {
		return jsonResponse(403, M{"error": "unexpected Host header"}, nil)
	}
	path := r.URL.EscapedPath()
	if method == "GET" && (path == "/" || path == "/index.html") {
		return a.static("console.html", "text/html; charset=utf-8", true)
	}
	if method == "GET" && strings.HasPrefix(path, "/static/") {
		if ctype, ok := staticFiles[path[len("/static/"):]]; ok {
			return a.static(path[len("/static/"):], ctype, false)
		}
	}
	if method == "POST" && path == "/api/session" {
		return a.session(r)
	}
	if !a.authorized(r, method) {
		return jsonResponse(403, M{"error": needKey}, nil)
	}
	if method == "GET" && strings.HasPrefix(path, "/files/") {
		return a.file(path[len("/files/"):])
	}
	if method == "GET" && strings.HasPrefix(path, "/logs/") {
		return a.log(path[len("/logs/"):])
	}
	if method == "GET" && strings.HasPrefix(path, "/workspace/") {
		return a.workspaceFile(path[len("/workspace/"):])
	}
	body, raw := M{}, []byte("{}")
	if method == "POST" {
		b, data, err := readBody(r)
		if err != nil {
			return errorResponse(err)
		}
		body, raw = b, data
	}
	status, data, err := a.route(method, path, body, raw)
	if err != nil {
		return errorResponse(err)
	}
	if status == 0 {
		return jsonResponse(404, M{"error": "not found"}, nil)
	}
	return jsonResponse(status, data, nil)
}

// planModel is body.get("model") with the page's key order kept.
func planModel(raw []byte) any {
	v, err := planedit.DecodeJSON(raw)
	if err != nil {
		return nil
	}
	if d, ok := v.(*planedit.Dict); ok {
		return d.Get("model")
	}
	return nil
}

func (a *app) route(method, path string, body M, raw []byte) (int, any, error) {
	t := a.trial
	if method == "GET" {
		switch path {
		case "/api/ping":
			return 200, M{"ok": true, "trial": t, "pid": os.Getpid()}, nil
		case "/api/state":
			snap, err := snapshot(t, 14, a.childPids())
			return 200, snap, err
		case "/api/trial-text":
			data, err := os.ReadFile(t)
			if err != nil {
				return 0, nil, err
			}
			return 200, M{"text": strings.ToValidUTF8(string(data), "�"), "sha256": sha256Hex(data)}, nil
		}
		return 0, nil, nil
	}
	switch path {
	case "/api/check":
		if a.checking.Load() {
			return 0, nil, Conflict("a readiness check is already running")
		}
		if _, err := ensureIdle(t, nil); err != nil {
			return 0, nil, err
		}
		a.checking.Store(true)
		a.background(func() error {
			defer a.checking.Store(false)
			_, err := check(t, "user")
			return err
		})
		return 202, M{"ok": true}, nil
	case "/api/start":
		plan := planArg(body)
		a.lock.Lock()
		err := func() error {
			if a.busy() {
				return Conflict("a readiness check or command is already running")
			}
			if _, _, err := preflightStart(t, plan); err != nil {
				return err
			}
			record(t, "user", "start_requested", M{"plan": util.Clip(pstr(body["plan"]), 12)})
			a.checking.Store(true)
			return nil
		}()
		a.lock.Unlock()
		if err != nil {
			return 0, nil, err
		}
		accept := util.Truthy(body["accept_warnings"])
		a.background(func() error {
			defer a.checking.Store(false)
			_, err := start(t, StartOpts{Actor: "user", Via: "console", AcceptWarnings: accept}, plan, "", a.watch)
			return err
		})
		return 202, M{"ok": true}, nil
	case "/api/stop":
		res, err := Stop(t, "user")
		return 200, res, err
	case "/api/again", "/api/restart":
		return a.again(t, path, body)
	case "/api/shutdown":
		record(t, "user", "shutdown_requested", nil)
		time.AfterFunc(300*time.Millisecond, a.shutdown)
		return 200, M{"ok": true, "reopen": "ajx console open " + Short(filepath.Dir(t))}, nil
	case "/api/stages":
		entry, err := runStages(t, "user", body["run"], body["stages"], a.watch)
		return 200, M{"ok": true, "entry": entry}, err
	case "/api/report":
		entry, err := rebuildReports(t, "user", a.watch)
		return 200, M{"ok": true, "entry": entry}, err
	case "/api/export":
		res, err := exportRun(t, "user", body["run"])
		return 200, res, err
	case "/api/plan/preview":
		res, err := savePlan(t, planModel(raw), nil, nil, "user", true)
		return 200, res, err
	case "/api/plan/save":
		res, err := savePlan(t, planModel(raw), body["base"], body["prompt_base"], "user", false)
		return 200, res, err
	case "/api/prompt":
		res, err := savePrompt(t, getOr(body, "text", ""), body["base"], "user")
		return 200, res, err
	case "/api/settings":
		res, err := saveSettings(t, util.MapOr(body["changes"]), body["base"], "user")
		return 200, res, err
	case "/api/trial-text":
		res, err := saveTrialText(t, getOr(body, "text", ""), body["base"], "user")
		return 200, res, err
	case "/api/fresh-output":
		res, err := freshOutput(t, pstr(body["base"]), "user")
		return 200, res, err
	case "/api/add-attempt":
		res, err := addAttempt(t, pstr(body["base"]), "user")
		return 200, res, err
	case "/api/message":
		entry, err := message(t, body["text"], body["about"])
		return 200, M{"entry": entry}, err
	}
	return 0, nil, nil
}

func getOr(m M, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// planArg is body.get("plan"): nil when absent or null.
func planArg(body M) *string {
	v, ok := body["plan"]
	if !ok || v == nil {
		return nil
	}
	s := pstr(v)
	return &s
}

func (a *app) again(t, path string, body M) (int, any, error) {
	mode := util.S(body["mode"])
	if !util.Contains(AgainModes, mode) {
		return 0, nil, Errorf("choose where to run again: %s", strings.Join(AgainModes, " or "))
	}
	restart := path == "/api/restart"
	a.lock.Lock()
	err := func() error {
		if a.checking.Load() {
			return Conflict("a readiness check or start is already in progress")
		}
		if restart {
			sp, _ := load(t)
			entries, _ := readEntries(t, 0)
			job := activeJob(t, entries, outputDir(t, sp))
			if job == nil {
				return Errorf("nothing is running; use Run again")
			}
			if _, err := Stop(t, "user"); err != nil {
				return err
			}
			record(t, "user", "restart_requested", M{"mode": mode, "job_n": job["n"]})
		} else if _, err := ensureIdle(t, nil); err != nil {
			return err
		}
		a.checking.Store(true)
		return nil
	}()
	a.lock.Unlock()
	if err != nil {
		return 0, nil, err
	}
	accept := util.Truthy(body["accept_warnings"])
	var plan *string
	if !restart {
		plan = planArg(body)
	}
	a.background(func() error {
		defer a.checking.Store(false)
		deadline := time.Now().Add(15 * time.Minute)
		for restart && time.Now().Before(deadline) {
			sp, _ := load(t)
			entries, _ := readEntries(t, 0)
			if activeJob(t, entries, outputDir(t, sp)) == nil {
				break
			}
			time.Sleep(time.Second)
		}
		_, err := runAgain(t, AgainOpts{Actor: "user", Via: "console", Mode: mode, AcceptWarnings: accept}, plan, a.watch)
		return err
	})
	return 202, M{"ok": true}, nil
}

func (a *app) hostOK(r *http.Request) bool {
	host := strings.ToLower(r.Host)
	p := strconv.Itoa(a.port)
	return host == "127.0.0.1:"+p || host == "localhost:"+p
}

func (a *app) cookie(r *http.Request) string {
	for _, header := range r.Header.Values("Cookie") {
		for _, part := range strings.Split(header, ";") {
			name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
			if name == fmt.Sprintf("ajx_console_%d", a.port) {
				return value
			}
		}
	}
	return ""
}

func same(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (a *app) authorized(r *http.Request, method string) bool {
	if same(r.Header.Get("X-AJX-Token"), a.token) {
		return true
	}
	// Opened links (reports, logs) carry the session cookie; actions always need the header.
	return method == "GET" && same(a.cookie(r), a.token)
}

func (a *app) session(r *http.Request) response {
	if !same(r.Header.Get("X-AJX-Token"), a.token) {
		return jsonResponse(403, M{"error": needKey}, nil)
	}
	cookie := fmt.Sprintf("ajx_console_%d=%s; HttpOnly; SameSite=Lax; Path=/", a.port, a.token)
	return jsonResponse(200, M{"ok": true}, map[string]string{"Set-Cookie": cookie})
}

func readBody(r *http.Request) (M, []byte, error) {
	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		return nil, nil, Errorf("expected a JSON body")
	}
	length := 0
	if raw := strings.TrimSpace(r.Header.Get("Content-Length")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, nil, Errorf("malformed Content-Length")
		}
		length = n
	}
	if length < 0 || length > 2_000_000 {
		return nil, nil, Errorf("request too large or malformed")
	}
	data := make([]byte, length)
	n, _ := io.ReadFull(r.Body, data)
	data = data[:n]
	if len(data) == 0 {
		data = []byte("{}")
	}
	v, err := runner.DecodeJSON(data)
	if err != nil {
		return nil, nil, Errorf("the request body is not valid JSON")
	}
	m, ok := v.(M)
	if !ok {
		return nil, nil, Errorf("expected a JSON object")
	}
	return m, data, nil
}

func (a *app) static(name, ctype string, csp bool) response {
	data, err := res.ReadFile("assets/" + name)
	if err != nil {
		return errorResponse(err)
	}
	var extra map[string]string
	if csp {
		extra = map[string]string{"Content-Security-Policy": PageCSP}
	}
	return response{status: 200, body: data, ctype: ctype, extra: extra}
}

func notFound() response { return jsonResponse(404, M{"error": "not found"}, nil) }

func (a *app) file(rel string) response {
	sp, _ := load(a.trial)
	root := resolvePath(outputDir(a.trial, sp))
	name, err := url.PathUnescape(rel)
	if err != nil {
		return notFound()
	}
	var target string
	if strings.HasPrefix(name, "/") {
		target = resolvePath(name)
	} else {
		target = resolvePath(filepath.Join(root, name))
	}
	inside := target == root || strings.HasPrefix(target, root+string(os.PathSeparator))
	ctype, known := contentTypes[pySuffix(filepath.Base(target))]
	if !inside || !isFile(target) || !known {
		return notFound()
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return notFound()
	}
	var extra map[string]string
	if s := pySuffix(filepath.Base(target)); s == ".html" || s == ".svg" {
		// Reports quote worker output: run them as an opaque origin, without the console's cookie.
		extra = map[string]string{"Content-Security-Policy": ReportCSP}
	}
	return response{status: 200, body: data, ctype: ctype, extra: extra}
}

// workspaceFile is a file the worker wrote, as plain text (or a download),
// from inside its workspace only.
func (a *app) workspaceFile(rel string) response {
	runID, inner, _ := strings.Cut(pyUnquote(rel), "/")
	sp, _ := load(a.trial)
	d := filepath.Join(outputDir(a.trial, sp), "runs", pyfmt.Name(runID))
	root := workspaceDir(d, readMap(filepath.Join(d, "state.json")))
	if root == "" || inner == "" {
		return notFound()
	}
	target := filepath.Join(root, inner)
	if strings.HasPrefix(inner, "/") {
		target = inner // pathlib: an absolute part replaces the root
	}
	target, base := resolvePath(target), resolvePath(root)
	if !(target == base || strings.HasPrefix(target, base+string(os.PathSeparator))) || !isFile(target) {
		return notFound()
	}
	f, err := os.Open(target)
	if err != nil {
		return notFound()
	}
	defer f.Close()
	data, _ := io.ReadAll(io.LimitReader(f, 2_000_000))
	if text, ok := utf8Text(data); ok {
		return response{status: 200, body: text, ctype: "text/plain; charset=utf-8",
			extra: map[string]string{"Content-Security-Policy": "sandbox"}}
	}
	// The worker chose this name: percent-encode it.
	disposition := "attachment; filename*=UTF-8''" + pyfmt.Quote(filepath.Base(target), "")
	return response{status: 200, body: data, ctype: "application/octet-stream",
		extra: map[string]string{"Content-Disposition": disposition}}
}

func (a *app) log(name string) response {
	unq, err := url.PathUnescape(name)
	if err != nil {
		return notFound()
	}
	target := filepath.Join(StateDir(a.trial), "logs", filepath.Base(unq))
	if !isFile(target) {
		return notFound()
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return notFound()
	}
	return response{status: 200, body: data, ctype: "text/plain; charset=utf-8"}
}

func ping(info M, timeout time.Duration) bool {
	port, ok := intOf(info["port"])
	if !ok || port == 0 {
		return false
	}
	req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/ping", port), nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-AJX-Token", pstr(info["token"]))
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return false
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return false
	}
	return out["ok"] == true
}

func normalizeInfo(info M) M {
	out := util.Copy(info)
	for _, k := range []string{"pid", "port"} {
		if n, ok := intOf(out[k]); ok {
			out[k] = n
		}
	}
	return out
}

// ServerInfo is this trial's running console (server.json: pid, port, token,
// url, started_at, trial), or nil when none answers.
func ServerInfo(trial string) M {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil
	}
	info := readMapOrNil(serverPath(trial))
	if info == nil || !alive(info["pid"]) || !util.Truthy(info["port"]) {
		return nil
	}
	if !ping(info, 2*time.Second) {
		return nil
	}
	return normalizeInfo(info)
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// server is a bound console (Python make_server's httpd, app and info).
type server struct {
	app  *app
	http *http.Server
	ln   net.Listener
	info M
	once sync.Once
}

func newServer(trial string, port int, idle time.Duration) (*server, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	if idle <= 0 {
		idle = IdleTimeout
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	a := &app{trial: trial, token: newToken(), idle: idle, children: map[int]*exec.Cmd{}}
	a.touch()
	a.port = ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: a, ReadHeaderTimeout: 30 * time.Second}
	srv.SetKeepAlivesEnabled(false) // one request per connection, like the Python console
	s := &server{app: a, http: srv, ln: ln}
	a.shutdown = s.shutdown
	link := fmt.Sprintf("http://127.0.0.1:%d/#token=%s", a.port, a.token)
	s.info = M{"pid": os.Getpid(), "port": a.port, "token": a.token, "url": link, "started_at": util.Now(), "trial": trial}
	if err := writePrivate(serverPath(trial), s.info); err != nil {
		ln.Close()
		return nil, err
	}
	record(trial, "console", "console_started", M{"port": a.port})
	return s, nil
}

func (s *server) serve() error {
	err := s.http.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *server) shutdown() {
	s.once.Do(func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := s.http.Shutdown(ctx); err != nil {
				_ = s.http.Close()
			}
		}()
	})
}

// finish removes server.json when it is still this server's and journals the stop.
func (s *server) finish() {
	info := readMap(serverPath(s.app.trial))
	if pid, _ := intOf(info["pid"]); pid == os.Getpid() {
		os.Remove(serverPath(s.app.trial))
	}
	record(s.app.trial, "console", "console_stopped", nil)
}

func openBrowser(link string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		cmd = exec.Command("xdg-open", link)
	}
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}

// Serve runs this trial's console in this process until shutdown, a signal
// (SIGINT, SIGTERM, SIGHUP) or the idle timeout (30 minutes without a request
// while nothing the console started is running). Runs keep going.
func Serve(trial string, o ServeOpts) error {
	s, err := newServer(trial, o.Port, o.Idle)
	if err != nil {
		return err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)
	defer signal.Stop(sigs)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sigs:
			s.shutdown()
		case <-done:
		}
	}()
	tick := 15 * time.Second
	if s.app.idle/2 < tick {
		tick = max(s.app.idle/2, 50*time.Millisecond)
	}
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if s.app.idleFor() > s.app.idle && !s.app.busy() {
					s.shutdown()
					return
				}
			}
		}
	}()
	link := util.S(s.info["url"])
	if o.Announce != nil {
		o.Announce(link)
	} else {
		fmt.Println(link)
	}
	if o.OpenBrowser {
		openBrowser(link)
	}
	err = s.serve()
	s.finish()
	return err
}

// spawned are servers this process started, so it can reap them when it stops one.
var (
	spawnedMu sync.Mutex
	spawned   = map[int]chan struct{}{}
)

func waitSpawned(pid int, timeout time.Duration) bool {
	spawnedMu.Lock()
	done, ok := spawned[pid]
	delete(spawned, pid)
	spawnedMu.Unlock()
	if !ok {
		return false
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}
	return true
}

func tailLines(path string, n int) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := splitLines(strings.ToValidUTF8(string(data), "�"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// OpenConsole reuses this trial's console or starts one in the background
// (`ajx console serve <trial> --port N`, detached); returns its server info,
// with "started": true when it was started now.
func OpenConsole(trial string, o OpenOpts) (M, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return nil, err
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	info := ServerInfo(trial)
	if info == nil {
		os.Remove(serverPath(trial))
		logPath := filepath.Join(StateDir(trial), "server.log")
		fh, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		argv := append(ajxCommand(), "console", "serve", trial, "--port", strconv.Itoa(o.Port))
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = filepath.Dir(trial)
		cmd.Stdout, cmd.Stderr = fh, fh
		cmd.Env = util.EnvList(jobEnv())
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		err = cmd.Start()
		fh.Close()
		if err != nil {
			return nil, Errorf("the console did not start: %s", err.Error())
		}
		pid := cmd.Process.Pid
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		spawnedMu.Lock()
		spawned[pid] = done
		spawnedMu.Unlock()
		deadline := time.Now().Add(timeout)
		for {
			got := readMapOrNil(serverPath(trial))
			if p, _ := intOf(got["pid"]); got != nil && p == pid && ping(got, 2*time.Second) {
				info = normalizeInfo(got)
				info["started"] = true
				break
			}
			exited := false
			select {
			case <-done:
				exited = true
			default:
			}
			if exited || time.Now().After(deadline) {
				tail := tailLines(logPath, 8)
				msg := "the console did not start"
				if len(tail) > 0 {
					msg += ":\n" + strings.Join(tail, "\n")
				}
				return nil, &Error{Msg: msg, Status: 400}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if o.OpenBrowser {
		openBrowser(util.S(info["url"]))
	}
	return info, nil
}

// StopConsole stops this trial's console server (never a run); false when none was running.
func StopConsole(trial string) (bool, error) {
	trial, err := TrialFile(trial)
	if err != nil {
		return false, err
	}
	info := readMapOrNil(serverPath(trial))
	if info == nil || !alive(info["pid"]) {
		os.Remove(serverPath(trial))
		return false, nil
	}
	pid, _ := intOf(info["pid"])
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			os.Remove(serverPath(trial))
			return false, nil
		}
		return false, err
	}
	if waitSpawned(pid, 5*time.Second) {
		return true, nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && util.PidAlive(pid) {
		time.Sleep(100 * time.Millisecond)
	}
	return true, nil
}

// listPS is the process table (`ps -axo pid=,etime=,command=`); a test hook.
var listPS = func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,etime=,command=").Output()
	return string(out), err
}

// ListConsoles lists console servers running for this user, from either CLI:
// [{pid, elapsed, trial, short, url, started_at}].
func ListConsoles() []M {
	text, err := listPS()
	if err != nil {
		return []M{}
	}
	found := []M{}
	for _, line := range strings.Split(text, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		pid, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		command := strings.TrimSpace(line)
		command = strings.TrimSpace(strings.TrimPrefix(command, parts[0]))
		command = strings.TrimSpace(strings.TrimPrefix(command, parts[1]))
		if !strings.Contains(" "+command+" ", " console serve ") || !strings.Contains(command, "ajx") {
			continue
		}
		_, after, ok := strings.Cut(command, " console serve ")
		if !ok {
			continue
		}
		trial, _, _ := strings.Cut(after, " --port")
		trial = strings.TrimSpace(trial)
		info := readMap(filepath.Join(filepath.Dir(trial), StateDirName, "server.json"))
		var link any
		if p, _ := intOf(info["pid"]); p == pid {
			link = info["url"]
		}
		found = append(found, M{"pid": pid, "elapsed": parts[1], "trial": trial, "short": Short(filepath.Dir(trial)),
			"url": link, "started_at": info["started_at"]})
	}
	return found
}

// listConsoles is ListConsoles, replaceable in tests so they never touch other consoles.
var listConsoles = ListConsoles

// StopAll stops every console server (never a run) and returns how many were asked to stop.
func StopAll() int {
	consoles := listConsoles()
	for _, c := range consoles {
		pid := util.Int(c["pid"], 0)
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			continue
		}
		waitSpawned(pid, 5*time.Second)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		any := false
		for _, c := range consoles {
			if util.PidAlive(util.Int(c["pid"], 0)) {
				any = true
			}
		}
		if !any {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return len(consoles)
}

// CopyDemo copies the simulated console demo trial into dest and returns its trial.toml.
func CopyDemo(dest string, force bool) (string, error) {
	dest = resolvePath(expandUser(dest))
	if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 && !force {
		return "", Errorf("%s is not empty; choose a new folder or pass --force", dest)
	}
	skip := func(rel string, isDir bool) bool {
		name := filepath.Base(rel)
		return name == "__pycache__" || strings.HasSuffix(name, ".pyc") || name == "ajx-reports" || name == StateDirName
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	if err := res.CopyDir("examples/console-demo", dest, skip); err != nil {
		return "", err
	}
	return filepath.Join(dest, "trial.toml"), nil
}

// utf8Text is data as UTF-8 text, dropping an incomplete character at the end
// (the 2 MB cut can split one), as Python's incremental decoder does.
func utf8Text(data []byte) ([]byte, bool) {
	if utf8.Valid(data) {
		return data, true
	}
	for i := 1; i <= 3 && i <= len(data); i++ {
		head, tail := data[:len(data)-i], data[len(data)-i:]
		if utf8.Valid(head) && utf8.RuneStart(tail[0]) && !utf8.FullRune(tail) {
			return head, true
		}
	}
	return nil, false
}
