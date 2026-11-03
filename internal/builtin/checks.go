package builtin

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

type checkFunc struct {
	name string
	fn   func(check M, ctx *plugin.Ctx) (M, error)
}

func (c *checkFunc) Name() string                            { return c.name }
func (c *checkFunc) Run(check M, ctx *plugin.Ctx) (M, error) { return c.fn(check, ctx) }

func registerChecks() {
	plugin.RegisterCheck("shell", "builtin", func() plugin.Check { return &checkFunc{"shell", ShellCheck} })
	plugin.RegisterCheck("file_exists", "builtin", func() plugin.Check { return &checkFunc{"file_exists", FileExistsCheck} })
	plugin.RegisterCheck("http", "builtin", func() plugin.Check { return &checkFunc{"http", HTTPCheck} })
}

func checkTimeout(check M) int {
	if f, ok := util.Num(check["timeout"]); ok && f > 0 {
		if f < 1 {
			return 1
		}
		return int(f)
	}
	return 600
}

func checkInt(check M, key string, def int) int {
	v, ok := check[key]
	if !ok {
		return def
	}
	if s, isStr := v.(string); isStr {
		var n int
		if _, err := fmt.Sscan(strings.TrimSpace(s), &n); err == nil {
			return n
		}
		return def
	}
	return util.Int(v, def)
}

// FileCount accepts integer strings and integral JSON numbers for compatibility,
// but never coerces a boolean, fraction, zero or negative count into success.
func FileCount(check M) (int, error) {
	v, present := check["min_count"]
	if !present {
		return 1, nil
	}
	var text string
	switch n := v.(type) {
	case int:
		text = strconv.Itoa(n)
	case int64:
		text = strconv.FormatInt(n, 10)
	case float64:
		if !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n {
			text = strconv.FormatFloat(n, 'f', -1, 64)
		}
	case string:
		text = strings.TrimSpace(n)
	}
	n, err := strconv.ParseInt(text, 10, strconv.IntSize)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("min_count must be a positive integer")
	}
	return int(n), nil
}

var checkEnvRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// ValidateHTTPURL checks syntax without resolving credentials or making a
// request. A complete ${VAR} URL remains deferred until runtime.
func ValidateHTTPURL(raw string) error {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\x00\r\n\t ") {
		return fmt.Errorf("url must be a non-empty http:// or https:// URL with a host")
	}
	if checkEnvRef.FindString(raw) == raw {
		return nil
	}
	candidate := checkEnvRef.ReplaceAllString(raw, "1")
	if strings.HasPrefix(raw, "${") && strings.Contains(candidate, "://") {
		candidate = "http" + candidate[strings.Index(candidate, "://"):]
	}
	u, err := url.Parse(candidate)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("url must be a non-empty http:// or https:// URL with a host")
	}
	return nil
}

// ShellCheck: run = "<shell command>"; expect_exit (default 0);
// expect_stdout = "<regex>" (optional).
func ShellCheck(check M, ctx *plugin.Ctx) (M, error) {
	location := util.S(check["location"])
	if location == "" {
		location = "host"
	}
	res := plugin.ContextShell(util.Str(check["run"]), ctx, checkTimeout(check), location)
	timedOut := util.Truthy(res["timed_out"])
	code, hasCode := res["exit_code"]
	passed := !timedOut && hasCode && code != nil && util.Int(code, -1<<31) == checkInt(check, "expect_exit", 0)
	if pattern := util.Str(check["expect_stdout"]); passed && pattern != "" {
		re, err := regexp.Compile("(?m)" + pattern)
		passed = err == nil && re.MatchString(util.S(res["stdout"]))
	}
	detail := "exit=" + newScope().pyStr(code)
	if timedOut {
		detail += " timeout"
	}
	out := util.Copy(res)
	out["passed"] = passed
	out["detail"] = detail
	return out, nil
}

// FileExistsCheck: path = "<glob relative to workspace>"; min_count
// (default 1); contains = "<regex>" (optional). Every component beneath the
// workspace is opened without following worker-created links.
func FileExistsCheck(check M, ctx *plugin.Ctx) (M, error) {
	started := util.Now()
	minCount, err := FileCount(check)
	if err != nil {
		return nil, err
	}
	workspace := ctx.Workspace
	pattern, isStr := check["path"].(string)
	if !isStr || pattern == "" || strings.Contains(pattern, "\x00") || strings.HasPrefix(pattern, "/") ||
		util.Contains(strings.Split(pattern, "/"), "..") {
		return M{"passed": false, "detail": "file_exists requires a glob beneath the workspace",
			"started_at": started, "stopped_at": util.Now()}, nil
	}
	var regex *regexp.Regexp
	if c := util.Str(check["contains"]); c != "" {
		re, err := regexp.Compile("(?m)" + c)
		if err != nil {
			return nil, fmt.Errorf("file_exists contains is not a valid regular expression: %v", err)
		}
		regex = re
	}
	var hits []string
	if info, err := os.Lstat(workspace); err == nil && info.IsDir() {
		if root, err := os.OpenRoot(workspace); err == nil {
			for _, rel := range globRelative(workspace, pattern) {
				if fileMatches(root, rel, regex) {
					hits = append(hits, rel)
				}
			}
			root.Close()
		}
	}
	shown := hits
	if len(shown) > 20 {
		shown = shown[:20]
	}
	return M{"passed": len(hits) >= minCount, "detail": fmt.Sprintf("%d match(es): %s", len(hits), newScope().repr(anyList(shown))),
		"started_at": started, "stopped_at": util.Now()}, nil
}

// fileMatches opens rel beneath root refusing symlinks at every component and
// anything but a regular file, then searches its text.
func fileMatches(root *os.Root, rel string, regex *regexp.Regexp) bool {
	parts := strings.Split(rel, "/")
	if len(parts) == 0 {
		return false
	}
	for i := range parts {
		if parts[i] == "" || parts[i] == "." || parts[i] == ".." {
			return false
		}
		info, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if i < len(parts)-1 && !info.IsDir() {
			return false
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return false
		}
	}
	before, _ := root.Lstat(rel)
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || before == nil || !os.SameFile(before, info) {
		return false
	}
	if regex == nil {
		return true
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	return regex.MatchString(validUTF8(string(data)))
}

// globRelative expands a pathlib-style glob beneath dir without following
// symlinks, returning slash-separated relative paths (deduplicated, sorted
// per directory).
func globRelative(dir, pattern string) []string {
	if strings.HasSuffix(pattern, "/") {
		return nil // a trailing separator selects directories only
	}
	var segs []string
	for _, s := range strings.Split(pattern, "/") {
		if s != "" && s != "." {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(rel string) {
		if rel != "" && !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	var walk func(rel string, segs []string)
	join := func(a, b string) string {
		if a == "" {
			return b
		}
		return a + "/" + b
	}
	abs := func(rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }
	isDir := func(rel string) bool {
		info, err := os.Lstat(abs(rel))
		return err == nil && info.IsDir()
	}
	entries := func(rel string) []os.DirEntry {
		list, _ := os.ReadDir(abs(rel))
		return list
	}
	var descendants func(rel string, fn func(string, bool))
	descendants = func(rel string, fn func(string, bool)) {
		for _, e := range entries(rel) {
			child := join(rel, e.Name())
			realDir := e.IsDir() && e.Type()&os.ModeSymlink == 0
			fn(child, realDir)
			if realDir {
				descendants(child, fn)
			}
		}
	}
	walk = func(rel string, segs []string) {
		if len(segs) == 0 {
			add(rel)
			return
		}
		seg, rest := segs[0], segs[1:]
		switch {
		case seg == "**":
			for len(rest) > 0 && rest[0] == "**" {
				rest = rest[1:]
			}
			if len(rest) == 0 {
				descendants(rel, func(child string, _ bool) { add(child) })
				return
			}
			walk(rel, rest)
			descendants(rel, func(child string, realDir bool) {
				if realDir {
					walk(child, rest)
				}
			})
		case strings.ContainsAny(seg, "*?["):
			re := fnmatchRegexp(seg)
			for _, e := range entries(rel) {
				if re == nil || !re.MatchString(e.Name()) {
					continue
				}
				child := join(rel, e.Name())
				if len(rest) == 0 {
					add(child)
				} else if e.IsDir() && e.Type()&os.ModeSymlink == 0 {
					walk(child, rest)
				}
			}
		default:
			child := join(rel, seg)
			if len(rest) == 0 {
				if _, err := os.Lstat(abs(child)); err == nil {
					add(child)
				}
			} else if isDir(child) {
				walk(child, rest)
			}
		}
	}
	walk("", segs)
	return out
}

// fnmatchRegexp translates one glob path segment like Python's fnmatch.
func fnmatchRegexp(pat string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`^(?s:`)
	r := []rune(pat)
	n := len(r)
	for i := 0; i < n; {
		c := r[i]
		i++
		switch c {
		case '*':
			for i < n && r[i] == '*' {
				i++
			}
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		case '[':
			j := i
			if j < n && r[j] == '!' {
				j++
			}
			if j < n && r[j] == ']' {
				j++
			}
			for j < n && r[j] != ']' {
				j++
			}
			if j >= n {
				b.WriteString(`\[`)
				continue
			}
			stuff := r[i:j]
			i = j + 1
			neg := false
			if len(stuff) > 0 && stuff[0] == '!' {
				neg = true
				stuff = stuff[1:]
			}
			if len(stuff) == 0 {
				if neg {
					b.WriteString(`.`)
				} else {
					b.WriteString(`[^\x00-\x{10FFFF}]`)
				}
				continue
			}
			b.WriteByte('[')
			if neg {
				b.WriteByte('^')
			}
			for k, ch := range stuff {
				switch {
				case ch == '-' && k > 0 && k < len(stuff)-1:
					b.WriteByte('-')
				case ch < 0x80 && !('a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' || '0' <= ch && ch <= '9'):
					b.WriteByte('\\')
					b.WriteRune(ch)
				default:
					b.WriteRune(ch)
				}
			}
			b.WriteByte(']')
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`)$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// HTTPCheck: url = "..."; expect_status (default 200); expect_body =
// "<regex>" (optional). Explicit environments never use ambient proxies.
func HTTPCheck(check M, ctx *plugin.Ctx) (M, error) {
	started := util.Now()
	isolated := ctx.EnvironmentProfile != nil
	proxyPolicy := "urllib_default"
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = urllibProxy
	if isolated {
		proxyPolicy = "disabled"
		transport.Proxy = nil
	}
	timeout := time.Duration(checkTimeout(check)) * time.Second
	if f, ok := util.Num(check["timeout"]); ok && f > 0 {
		timeout = time.Duration(f * float64(time.Second))
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	target := plugin.Resolve(check["url"])
	failed := func(err error) (M, error) {
		return M{"passed": false, "detail": "request failed: " + pyErrorText(err),
			"proxy_policy": proxyPolicy, "started_at": started, "stopped_at": util.Now()}, nil
	}
	req, err := http.NewRequestWithContext(ctx.Context(), http.MethodGet, target, nil)
	if err != nil {
		return failed(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return failed(err)
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	body := ""
	if status < 400 { // urllib raises HTTPError (no body kept) from 400 up
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
		body = validUTF8(string(data))
	}
	passed := status == checkInt(check, "expect_status", 200)
	if pattern := util.Str(check["expect_body"]); passed && pattern != "" {
		re, err := regexp.Compile(pattern)
		passed = err == nil && re.MatchString(body)
	}
	return M{"passed": passed, "detail": fmt.Sprintf("status=%d", status), "stdout": util.Clip(body, 2000),
		"proxy_policy": proxyPolicy, "started_at": started, "stopped_at": util.Now()}, nil
}

// urllibProxies is urllib.request.getproxies_environment: <scheme>_proxy
// variables, lowercase names winning (an empty lowercase one removes it).
func urllibProxies() map[string]string {
	env := util.Environ()
	proxies := map[string]string{}
	for name, value := range env {
		lower := strings.ToLower(name)
		if value != "" && strings.HasSuffix(lower, "_proxy") {
			proxies[strings.TrimSuffix(lower, "_proxy")] = value
		}
	}
	if _, cgi := env["REQUEST_METHOD"]; cgi {
		delete(proxies, "http")
	}
	for name, value := range env {
		if strings.HasSuffix(name, "_proxy") {
			key := strings.TrimSuffix(strings.ToLower(name), "_proxy")
			if value != "" {
				proxies[key] = value
			} else {
				delete(proxies, key)
			}
		}
	}
	return proxies
}

// urllibBypass is urllib.request.proxy_bypass_environment.
func urllibBypass(host string, proxies map[string]string) bool {
	noProxy, ok := proxies["no"]
	if !ok {
		return false
	}
	if noProxy == "*" {
		return true
	}
	host = strings.ToLower(host)
	hostOnly := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostOnly = h
	}
	for _, name := range strings.Split(noProxy, ",") {
		name = strings.ToLower(strings.TrimLeft(strings.TrimSpace(name), "."))
		if name == "" {
			continue
		}
		if hostOnly == name || host == name || strings.HasSuffix(hostOnly, "."+name) || strings.HasSuffix(host, "."+name) {
			return true
		}
	}
	return false
}

// urllibProxy selects a proxy the way urllib's default opener does from the
// environment (unlike net/http, loopback hosts are proxied too).
func urllibProxy(req *http.Request) (*url.URL, error) {
	proxies := urllibProxies()
	proxy := proxies[req.URL.Scheme]
	if proxy == "" || urllibBypass(req.URL.Host, proxies) {
		return nil, nil
	}
	if !strings.Contains(proxy, "://") {
		proxy = "http://" + proxy
	}
	return url.Parse(proxy)
}

// pyErrorText approximates "<ExceptionType>: <message>" of urllib failures.
func pyErrorText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "TimeoutError: timed out"
		}
		var op *net.OpError
		if errors.As(ue.Err, &op) {
			return fmt.Sprintf("URLError: <urlopen error %v>", op.Err)
		}
		return fmt.Sprintf("URLError: <urlopen error %v>", ue.Err)
	}
	return fmt.Sprintf("ValueError: %v", err)
}
