package builtin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// ------------------------------------------------------------ kiro (test_ajx.py)

func TestKiroNormalizeACPStream(t *testing.T) {
	tmp := t.TempDir()
	ctx := testCtx(t, tmp, M{"id": "k", "harness": "kiro-cli", "config": "clean", "model": "claude-haiku-4.5"}, M{"execute": M{}}, "")
	writeRawLines(t, filepath.Join(ctx.RunDir, "execute.raw.jsonl"), fixtureLines(t, "kiro-stream.jsonl"))
	tel := mustNormalize(t, harness(t, "kiro-cli"), ctx, "execute", nil)
	eq(t, tel["session_id"], "00000000-0000-4000-8000-000000000002", "session")
	eq(t, tel["usage_status"], "unavailable", "usage")
	eq(t, tel["tool_status"], "partial", "tools")
	var tools []M
	for _, e := range util.Maps(tel["events"]) {
		if e["kind"] == "tool" {
			tools = append(tools, e)
		}
	}
	if len(tools) != 1 || tools[0]["name"] != "shell" || tools[0]["is_error"] != false {
		t.Fatal(tools)
	}
	eq(t, tel["final_text"], "It printed `hi` to stdout.", "final_text")
	hr := tel["harness_reported"].(M)
	eq(t, hr["final_text_concatenated"], "It printed `hi` to stdout.", "concatenated")
	if num(hr["credits"]) <= 0.04 {
		t.Fatal(hr["credits"])
	}
	eq(t, tel["stop_reason"], "end_turn", "stop")
}

func TestKiroRestrictedAuthArgs(t *testing.T) {
	auth := newAuth(t, "env", M{"args": []any{"--trust-all-tools", "--trust-tools", "fs_read,shell", "--model-provider", "x",
		"--trust-tools=fs_write", "--trust-tools", "--other"}})
	eq(t, restrictedAuthArgs(&plugin.Ctx{Auth: auth}), []string{"--model-provider", "x", "--other"}, "restricted")
}

// ------------------------------------------------------------ reporters (test_ajx.py)

func TestReporterAdaptersUseRestrictedCommandsAndParseResponses(t *testing.T) {
	data := M{"asks": []any{}, "journey_errata": []any{}, "strengths": []any{}, "gates": []any{}, "no_obstacles_observed": true}
	text := newScope().dumps(data, true)
	schema := M{"type": "object", "properties": M{"asks": M{"type": "array"}}}
	for _, name := range []string{"codex", "kiro-cli"} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			ctx := testCtx(t, tmp, M{"id": "reporter", "harness": name, "config": "clean"}, M{}, filepath.Join(tmp, "cfg"))
			ctx.StageName = "extract"
			f := &fakeRun{write: func(ctx *plugin.Ctx, argv []string, stage string) {
				var events []any
				if name == "codex" {
					events = []any{M{"type": "item.completed", "item": M{"id": "fake", "type": "agent_message", "text": text}},
						M{"type": "turn.completed", "usage": M{"output_tokens": int64(12)}}}
					os.WriteFile(argv[indexOf(argv, "--output-last-message")+1], []byte(text), 0o644)
				} else {
					events = []any{M{"type": "runFinished", "data": M{"finalText": text, "stopReason": "end_turn"}}}
				}
				writeRaw(t, filepath.Join(ctx.RunDir, stage+".raw.jsonl"), events, "")
			}}
			f.install(t)
			res, err := harness(t, name).Report(ctx, "Synthetic evidence only.", schema)
			if err != nil {
				t.Fatal(err)
			}
			eq(t, res["text"], text, "text")
			proc := res["proc"].(M)
			eq(t, proc["is_error"], false, "is_error")
			argv := f.argvs[0]
			if indexOf(argv, "--trust-all-tools") >= 0 || indexOf(argv, "--dangerously-bypass-approvals-and-sandbox") >= 0 {
				t.Fatal(argv)
			}
			if name == "codex" {
				eq(t, argv[indexOf(argv, "--sandbox")+1], "read-only", "sandbox")
				schemaPath := argv[indexOf(argv, "--output-schema")+1]
				written, _ := os.ReadFile(schemaPath)
				eq(t, string(written), `{"properties": {"asks": {"type": "array"}}, "type": "object"}`, "schema file")
				eq(t, proc["output_tokens"], 12, "tokens")
				eq(t, proc["tools_policy"], "read-only sandbox; read-only commands may still run", "policy")
				eq(t, argv[len(argv)-1], "-", "stdin marker")
			} else {
				if indexOf(argv, "--trust-tools=") < 0 || strings.Contains(util.Compact(proc["argv"]), "Synthetic evidence only.") {
					t.Fatal(argv, proc["argv"])
				}
				eq(t, proc["result_subtype"], "end_turn", "subtype")
			}
		})
	}
}

func TestReporterRejectsAWorkerConfigurationContext(t *testing.T) {
	// agentconfig.EnsureReporterContext owns the rule; the adapters call it first.
	for _, name := range []string{"claude-code", "codex", "kiro-cli"} {
		h := harness(t, name)
		if !h.Base().CanReport {
			t.Fatal(name)
		}
	}
	for _, name := range []string{"gemini-cli", "cursor-agent", "copilot-cli"} {
		if _, err := harness(t, name).Report(&plugin.Ctx{}, "x", nil); err == nil || err.Error() != name+" has no reporter adapter" {
			t.Fatal(name, err)
		}
	}
}

// ------------------------------------------------------------ launch argv

func TestAdapterExecuteAndNarrateArgv(t *testing.T) {
	tmp := t.TempDir()
	f := &fakeRun{}
	f.install(t)
	cell := func(h string, extra M) M {
		c := M{"id": "c", "harness": h, "config": "clean", "model": "m1"}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	state := M{"execute": M{"session_id": "sid"}}
	cases := []struct {
		harness  string
		cell     M
		execute  []string
		narrate  []string
		stdin    bool
		cleanEnv map[string]string
	}{
		{"codex", cell("codex", M{"effort": "high"}),
			[]string{"codex", "exec", "--json", "--skip-git-repo-check", "--cd", tmp, "--model", "m1", "-c", "model_reasoning_effort=high",
				"--dangerously-bypass-approvals-and-sandbox", "-"},
			[]string{"codex", "exec", "resume", "sid", "--json", "--skip-git-repo-check", "--sandbox", "read-only", "--model", "m1",
				"-c", "model_reasoning_effort=high", "-"}, true, map[string]string{"CODEX_HOME": filepath.Join(tmp, "cfg")}},
		{"kiro-cli", cell("kiro-cli", M{"effort": "low", "args": []any{"--a"}}),
			[]string{"kiro-cli", "chat", "--output-format", "stream-json", "--trust-all-tools", "--model", "m1", "--effort", "low", "--a", "the prompt"},
			[]string{"kiro-cli", "chat", "--output-format", "stream-json", "--resume-id", "sid", "--trust-tools=", "--model", "m1", "tell"}, false, map[string]string{}},
		{"gemini-cli", cell("gemini-cli", nil),
			[]string{"gemini", "--output-format", "stream-json", "--model", "m1", "--yolo", "-p", "the prompt"},
			[]string{"gemini", "--output-format", "stream-json", "--resume", "sid", "--approval-mode", "default", "--model", "m1", "-p", "tell"}, false, nil},
		{"cursor-agent", cell("cursor-agent", M{"permission_args": []any{"--p"}}),
			[]string{"cursor-agent", "-p", "--output-format", "stream-json", "--model", "m1", "--p", "the prompt"},
			[]string{"cursor-agent", "-p", "--output-format", "stream-json", "--resume", "sid", "--model", "m1", "tell"}, false, nil},
		{"copilot-cli", cell("copilot-cli", nil),
			[]string{"copilot", "-p", "the prompt", "--model", "m1", "--allow-all-tools"}, nil, false, nil},
	}
	for _, c := range cases {
		f.argvs, f.opts = nil, nil
		ctx := testCtx(t, tmp, c.cell, state, filepath.Join(tmp, "cfg"))
		ctx.PromptText = "the prompt"
		h := harness(t, c.harness)
		proc, err := h.Execute(ctx)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, f.argvs[0], c.execute, c.harness+" execute")
		if c.stdin != (f.opts[0].StdinData != nil) {
			t.Fatal(c.harness, "stdin")
		}
		if c.cleanEnv != nil {
			eq(t, f.opts[0].ExtraEnv, c.cleanEnv, c.harness+" env")
		}
		if !c.stdin && !strings.Contains(util.Compact(proc["argv"]), "<prompt>") {
			t.Fatal(c.harness, proc["argv"])
		}
		np, err := h.Narrate(ctx, "tell")
		if err != nil {
			t.Fatal(err)
		}
		if c.narrate == nil {
			if np != nil || len(f.argvs) != 1 {
				t.Fatal(c.harness, "narrate unsupported")
			}
			continue
		}
		eq(t, f.argvs[1], c.narrate, c.harness+" narrate")
		eq(t, np["session_id"], "sid", c.harness+" narrate sid")
		if !util.Truthy(np["tools_disabled"]) {
			t.Fatal(c.harness)
		}
		// without an execute session there is nothing to resume
		ctx.State = M{}
		if np, _ := h.Narrate(ctx, "tell"); np != nil && c.harness != "claude-code" {
			t.Fatal(c.harness, np)
		}
	}
}

// ------------------------------------------------------------ declarative

func TestDeclarativeAdapter(t *testing.T) {
	tmp := t.TempDir()
	conf := M{"execute": []any{"opencode", "run", "--session", "{session_id}", "{prompt}"}, "model_args": []any{"--model", "{model}"},
		"clean_env": M{"XDG_CONFIG_HOME": "{config_dir}"}, "env": M{"A": "{workspace}", "N": int64(3)},
		"resume": []any{"opencode", "run", "--session", "{session_id}", "{prompt_file}"}, "no_tools_args": []any{"--read-only"},
		"session_id_regex": "session[:= ]+([A-Za-z0-9_-]{8,})", "version": []any{"opencode", "--version"}, "strip_env": []any{"X_*"}}
	h := NewDeclarative("opencode", conf)
	b := h.Base()
	if h.Name() != "opencode" || b.BinaryName != "opencode" || !b.CanResume || !b.CleanSupported || b.CanReport ||
		b.Telemetry != "wall clock and stdout only (declarative adapter)" || util.Compact(b.StripEnv) != `["X_*"]` {
		t.Fatal(b)
	}
	ctx := testCtx(t, tmp, M{"id": "d", "harness": "opencode", "config": "clean", "model": "m1"}, M{}, filepath.Join(tmp, "cfg"))
	ctx.PromptText = "do it"
	eq(t, h.CleanEnv(ctx), map[string]string{"A": tmp, "N": "3", "XDG_CONFIG_HOME": filepath.Join(tmp, "cfg")}, "clean env")
	f := &fakeRun{write: func(ctx *plugin.Ctx, argv []string, stage string) {
		os.WriteFile(filepath.Join(ctx.RunDir, stage+".stderr.txt"), []byte("note: session: abcdef123456\n"), 0o644)
		writeRawLines(t, filepath.Join(ctx.RunDir, stage+".raw.jsonl"), []string{"hello"})
	}}
	f.install(t)
	proc, err := h.Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	argv := f.argvs[0]
	if len(argv[3]) != 36 || argv[4] != "do it" || util.Compact(argv[5:]) != `["--model","m1"]` {
		t.Fatal(argv)
	}
	eq(t, proc["session_id"], "abcdef123456", "session from regex")
	eq(t, proc["argv"].([]any)[4], "<prompt>", "prompt hidden")
	ctx.State = M{"execute": M{"session_id": "abcdef123456"}}
	np, err := h.Narrate(ctx, "narrate please")
	if err != nil {
		t.Fatal(err)
	}
	pf := filepath.Join(ctx.RunDir, "prompt-input.txt")
	eq(t, f.argvs[1], []string{"opencode", "run", "--session", "abcdef123456", pf, "--model", "m1", "--read-only"}, "narrate argv")
	if data, _ := os.ReadFile(pf); string(data) != "narrate please" {
		t.Fatal(string(data))
	}
	eq(t, np["tools_disabled"], true, "tools disabled")
	tel := mustNormalize(t, h, ctx, "execute", nil)
	eq(t, tel["limitations"], []any{"opencode: declarative adapter; no token or tool-call telemetry"}, "limitations")
	eq(t, tel["model_ids"], []any{"m1"}, "model")
	// stdin prompts
	h2 := NewDeclarative("x", M{"execute": []any{"x"}, "prompt_via": "stdin"})
	f.argvs, f.opts = nil, nil
	if _, err := h2.Execute(ctx); err != nil || f.opts[0].StdinData == nil || *f.opts[0].StdinData != "do it" {
		t.Fatal(err, f.opts)
	}
	if h2.Base().CanResume || h2.Base().CleanSupported {
		t.Fatal(h2.Base())
	}
	if np, _ := h2.Narrate(ctx, "x"); np != nil {
		t.Fatal("no resume template, no narration")
	}
}

// ------------------------------------------------------------ runners

func TestDockerAndWrapperRunnerArgv(t *testing.T) {
	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "cfg")
	os.MkdirAll(cfg, 0o755)
	r, _ := plugin.NewRunner("docker", M{"type": "docker", "image": "img:1", "mounts": []any{"~/.claude/:/root/.claude", "/a//b:/c:ro"},
		"args": []any{"--network", "bridge"}})
	if r.LocalTranscripts() {
		t.Fatal("docker transcripts are remote by default")
	}
	ctx := &plugin.Ctx{Workspace: "/ws", ConfigDir: cfg, CacheDir: filepath.Join(tmp, "missing"), RunToken: "tok"}
	argv, err := r.Wrap([]string{"claude", "-p"}, ctx, map[string]string{"B": "2", "A": "1"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, argv, []string{"docker", "run", "--rm", "-i", "-v", "/ws:/ws", "-w", "/ws", "-v", cfg + ":" + cfg,
		"-v", util.Home() + "/.claude:/root/.claude", "-v", "/a/b:/c:ro", "-e", "A", "-e", "B", "--network", "bridge", "img:1",
		"claude", "-p"}, "docker argv")
	eq(t, r.Describe(), M{"type": "docker", "image": "img:1", "mounts": []any{"~/.claude/:/root/.claude", "/a//b:/c:ro"},
		"args": []any{"--network", "bridge"}}, "describe")
	noImage, _ := plugin.NewRunner("docker", nil)
	if _, err := noImage.Wrap([]string{"x"}, ctx, nil); err == nil {
		t.Fatal("image is required")
	}
	if lt, _ := plugin.NewRunner("docker", M{"image": "i", "local_transcripts": true}); !lt.LocalTranscripts() {
		t.Fatal("local_transcripts")
	}
	w, _ := plugin.NewRunner("wrapper", M{"prefix": []any{"ssh", "builder@host", "cd {workspace} &&"}})
	argv, _ = w.Wrap([]string{"claude", "-p", "it's here", "", "a b", "safe-arg_1.2:/x=y@%+,"}, ctx, nil)
	eq(t, argv, []string{"ssh", "builder@host", "cd /ws &&", `claude -p 'it'"'"'s here' '' 'a b' safe-arg_1.2:/x=y@%+,`}, "joined")
	w, _ = plugin.NewRunner("wrapper", M{"prefix": []any{"firejail", "--token={run_token}"}})
	argv, _ = w.Wrap([]string{"claude", "a b"}, ctx, nil)
	eq(t, argv, []string{"firejail", "--token=tok", "claude", "a b"}, "not joined")
	w, _ = plugin.NewRunner("wrapper", M{"prefix": []any{"sh", "-c"}, "shell_join": false})
	argv, _ = w.Wrap([]string{"a", "b"}, ctx, nil)
	eq(t, argv, []string{"sh", "-c", "a", "b"}, "explicit shell_join=false")
	w, _ = plugin.NewRunner("wrapper", M{"prefix": []any{"bash", "-c  "}})
	argv, _ = w.Wrap([]string{"a", "b"}, ctx, nil)
	eq(t, argv, []string{"bash", "-c  ", "a b"}, "-c suffix after rstrip")
	l, _ := plugin.NewRunner("local", nil)
	if !l.LocalTranscripts() || l.Name() != "local" {
		t.Fatal(l)
	}
}

// ------------------------------------------------------------ checks

func checkCtx(t *testing.T, ws string) *plugin.Ctx {
	r, _ := plugin.NewRunner("local", nil)
	return &plugin.Ctx{Workspace: ws, Runner: r, Env: map[string]string{}}
}

func runCheck(t *testing.T, name string, check M, ctx *plugin.Ctx) M {
	t.Helper()
	c, err := plugin.NewCheck(name)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Run(check, ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestShellCheck(t *testing.T) {
	ctx := checkCtx(t, t.TempDir())
	res := runCheck(t, "shell", M{"run": "echo hello; exit 3", "timeout": int64(5), "expect_exit": int64(3), "expect_stdout": "^hel"}, ctx)
	if res["passed"] != true || res["detail"] != "exit=3" || res["location"] != "host" {
		t.Fatal(res)
	}
	res = runCheck(t, "shell", M{"run": "echo hello", "timeout": int64(5), "expect_stdout": "^nope"}, ctx)
	if res["passed"] != false {
		t.Fatal(res)
	}
	res = runCheck(t, "shell", M{"run": "sleep 5", "timeout": int64(1)}, ctx)
	if res["passed"] != false || res["detail"] != "exit=None timeout" {
		t.Fatal(res)
	}
}

func TestFileExistsCheck(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	os.MkdirAll(filepath.Join(ws, "output", "deep"), 0o755)
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "result.txt"), []byte("synthetic external data"), 0o644)
	os.WriteFile(filepath.Join(ws, "output", "one.txt"), []byte("expected row"), 0o644)
	os.WriteFile(filepath.Join(ws, "output", "two.txt"), []byte("different row"), 0o644)
	os.WriteFile(filepath.Join(ws, "output", "deep", ".hidden.txt"), []byte("x"), 0o644)
	ctx := checkCtx(t, ws)
	check := func(path string, extra M) M {
		c := M{"path": path}
		for k, v := range extra {
			c[k] = v
		}
		return runCheck(t, "file_exists", c, ctx)
	}
	if check("output/*.txt", M{"min_count": int64(2)})["passed"] != true ||
		check("output/*.txt", M{"contains": "expected"})["passed"] != true ||
		check("output/*.txt", M{"contains": "expected", "min_count": int64(2)})["passed"] != false {
		t.Fatal("counts")
	}
	eq(t, check("output/*.txt", nil)["detail"], "2 match(es): ['output/one.txt', 'output/two.txt']", "detail")
	eq(t, check("**/*.txt", nil)["detail"], "3 match(es): ['output/one.txt', 'output/two.txt', 'output/deep/.hidden.txt']", "recursive")
	eq(t, check("output/**", nil)["detail"], "3 match(es): ['output/deep/.hidden.txt', 'output/one.txt', 'output/two.txt']", "trailing **")
	eq(t, check("output/[!t]*.txt", nil)["detail"], "1 match(es): ['output/one.txt']", "negated class")
	eq(t, check("./output/o?e.txt", nil)["detail"], "1 match(es): ['output/one.txt']", "dot and ?")
	// symlinks, inside or out, cannot supply evidence
	os.Symlink(filepath.Join(outside, "result.txt"), filepath.Join(ws, "result.txt"))
	os.Symlink(outside, filepath.Join(ws, "linked"))
	os.Symlink("output/one.txt", filepath.Join(ws, "alias.txt"))
	for _, p := range []string{"result.txt", "linked/result.txt", "linked/*.txt", "alias.txt"} {
		for _, contains := range []any{nil, "synthetic external"} {
			res := check(p, M{"contains": contains})
			if res["passed"] != false || res["detail"] != "0 match(es): []" {
				t.Fatal(p, res)
			}
		}
	}
	syscall.Mkfifo(filepath.Join(ws, "output.fifo"), 0o644)
	if check("output.fifo", nil)["passed"] != false {
		t.Fatal("fifo")
	}
	linkedWS := filepath.Join(root, "linked-workspace")
	os.Symlink(outside, linkedWS)
	if runCheck(t, "file_exists", M{"path": "result.txt"}, checkCtx(t, linkedWS))["passed"] != false {
		t.Fatal("symlinked workspace")
	}
	for _, p := range []any{"../outside/*.txt", filepath.Join(outside, "result.txt"), "", nil, "a\x00b"} {
		res := runCheck(t, "file_exists", M{"path": p}, ctx)
		if res["passed"] != false || res["detail"] != "file_exists requires a glob beneath the workspace" {
			t.Fatal(p, res)
		}
	}
	if check("*", nil)["passed"] != false {
		t.Fatal("directories are not file evidence")
	}
}

func TestHTTPCheckProxyPolicy(t *testing.T) {
	var mu sync.Mutex
	var originCalls, proxyCalls int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		originCalls++
		mu.Unlock()
		w.Write([]byte("origin-proof"))
	}))
	defer origin.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxyCalls++
		mu.Unlock()
		w.Write([]byte("proxy-proof"))
	}))
	defer proxy.Close()
	for _, k := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY"} {
		t.Setenv(k, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	check := M{"url": origin.URL + "/result", "timeout": int64(2), "expect_body": "proxy-proof"}
	legacy := runCheck(t, "http", check, &plugin.Ctx{})
	if legacy["passed"] != true || legacy["proxy_policy"] != "urllib_default" {
		t.Fatal(legacy)
	}
	isolated := runCheck(t, "http", check, &plugin.Ctx{EnvironmentProfile: M{"backend": "local"}})
	if isolated["passed"] != false || isolated["stdout"] != "origin-proof" || isolated["proxy_policy"] != "disabled" {
		t.Fatal(isolated)
	}
	if proxyCalls != 1 || originCalls != 1 {
		t.Fatal(proxyCalls, originCalls)
	}
	t.Setenv("no_proxy", "127.0.0.1")
	direct := runCheck(t, "http", M{"url": origin.URL + "/x", "timeout": int64(2), "expect_status": int64(200)}, &plugin.Ctx{})
	if direct["passed"] != true || proxyCalls != 1 || direct["detail"] != "status=200" {
		t.Fatal(direct, proxyCalls)
	}
	failed := runCheck(t, "http", M{"url": "http://127.0.0.1:1/", "timeout": int64(2)}, &plugin.Ctx{EnvironmentProfile: M{}})
	if failed["passed"] != false || !strings.HasPrefix(util.S(failed["detail"]), "request failed: URLError: ") {
		t.Fatal(failed)
	}
}

// ------------------------------------------------------------ console demo

func TestConsoleDemoRunsWithoutAnAgent(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no sh")
	}
	RegisterConsoleDemo()
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	os.MkdirAll(ws, 0o755)
	ctx := testCtx(t, tmp, M{"id": "demo", "harness": "console-demo", "config": "inherit"}, M{}, "")
	ctx.Workspace = ws
	ctx.Env = map[string]string{"AJX_DEMO_PACE": "0"}
	h := harness(t, "console-demo")
	if h.Version() != "console-demo 1 (simulated)" || h.Plan(ctx)["session_id"] != demoSession {
		t.Fatal(h.Version())
	}
	proc, err := h.Execute(ctx)
	if err != nil || util.Int(proc["exit_code"], -1) != 0 {
		t.Fatal(proc, err)
	}
	data, _ := os.ReadFile(filepath.Join(ws, "out.csv"))
	expected, _ := os.ReadFile("../../skills/ajx/examples/console-demo/expected.csv")
	if string(data) != demoCSV || string(expected) != demoCSV {
		t.Fatalf("out.csv %q", data)
	}
	tel := mustNormalize(t, h, ctx, "execute", nil)
	if tel["stop_reason"] != "success" || num(tel["expected_tool_calls"]) != 5 || len(util.List(tel["events"])) != 9 {
		t.Fatal(tel)
	}
	items := h.Progress(ctx.RunDir, "execute", 3)
	if len(items) != 3 || items[2]["kind"] != "text" {
		t.Fatal(items)
	}
	all := h.Progress(ctx.RunDir, "execute", 0)
	eq(t, all[2], M{"at": all[2]["at"], "kind": "tool", "error": true,
		"text": "Bash: orders-cli convert orders.json --to csv  ->  error: --columns is required for csv"}, "error tool")
	// evidence ids as the runner assigns them
	var events []any
	for i, e := range util.Maps(tel["events"]) {
		e["eid"] = "E-00" + itoa(i+1)
		events = append(events, e)
	}
	util.WriteJSON(filepath.Join(ctx.RunDir, "telemetry.execute.json"), M{"events": events})
	if _, err := h.Narrate(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	ntel := mustNormalize(t, h, ctx, "narrate", nil)
	if ntel["final_text"] != strings.TrimSuffix(demoJourney, "\n") {
		t.Fatalf("%q", ntel["final_text"])
	}
	ctx.StageName = "extract"
	res, err := h.Report(ctx, "x", M{"properties": M{"asks": M{}}})
	if err != nil {
		t.Fatal(err)
	}
	structured := res["structured"].(M)
	if len(util.List(structured["asks"])) != 3 || len(util.List(structured["strengths"])) != 1 || res["proc"].(M)["is_error"] != false {
		t.Fatal(structured)
	}
	if !strings.HasPrefix(util.S(res["text"]), `{"asks": [{"title": "List the valid columns when --columns is missing", "requested_behavior"`) {
		t.Fatal(res["text"])
	}
	res, _ = h.Report(ctx, "x", M{"properties": M{"journey": M{}}})
	if res["structured"] != nil || res["proc"].(M)["result_subtype"] != "unsupported" || res["proc"].(M)["is_error"] != true {
		t.Fatal(res)
	}
	// a stopped run gets an honest account of the recorded steps
	j := stoppedJourney(util.Maps(events)[:2])
	if !strings.Contains(j, "1. I'll start with the tool's built-in help. [E-001]") ||
		!strings.Contains(j, "2. I ran `command: orders-cli --help`. [E-002]") {
		t.Fatal(j)
	}
}

func TestConsoleDemoIsRegisteredOnDemand(t *testing.T) {
	if !plugin.NativeEquivalents["console_demo.py"] || plugin.NativeRegistrars["console_demo.py"] == nil {
		t.Fatal("console_demo.py has a native equivalent")
	}
	notes := plugin.Load("../../skills/ajx/examples/console-demo")
	if !plugin.Has("harness", "console-demo") || len(notes) != 0 || plugin.Source("harness", "console-demo") != "builtin" {
		t.Fatal(notes)
	}
	t.Setenv("AJX_DEMO_PACE", "2.5")
	if demoPace(&plugin.Ctx{Env: map[string]string{"AJX_DEMO_PACE": "-3"}}) != 0 || demoPace(&plugin.Ctx{}) != 2.5 ||
		demoPace(&plugin.Ctx{Env: map[string]string{"AJX_DEMO_PACE": "fast"}}) != 1 {
		t.Fatal("pace")
	}
}

func TestCleanEnvChecker(t *testing.T) {
	for _, name := range []string{"claude-code", "codex", "kiro-cli"} {
		c, ok := harness(t, name).(CleanEnvChecker)
		if !ok {
			t.Fatal(name)
		}
		ctx := testCtx(t, t.TempDir(), M{"id": "c", "harness": name, "config": "inherit"}, M{}, "")
		if _, err := c.CleanEnvErr(ctx); err != nil {
			t.Fatal(name, err)
		}
	}
}
