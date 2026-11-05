package builtin

import (
	"os"
	"strings"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// LocalRunner runs the worker directly on this machine (default).
type LocalRunner struct{ plugin.RunnerBase }

// DockerRunner runs the worker inside a container. The workspace is
// bind-mounted at the same path so paths in transcripts stay meaningful. Env
// vars are forwarded by name (values never logged).
//
//	[runner]                      # or per cell: runner = { type = "docker", image = "..." }
//	type = "docker"
//	image = "my-agent-image:latest"
//	mounts = ["~/.claude:/root/.claude"]   # optional extra -v mounts (e.g. harness config/transcripts)
//	args = ["--network", "bridge"]
//	local_transcripts = false               # set true if transcripts are mounted back to the host
type DockerRunner struct{ plugin.RunnerBase }

// WrapperRunner is any argv prefix: ssh, a CI sandbox, firejail,
// `sandbox-exec`, a devcontainer exec...
//
//	[runner]
//	type = "wrapper"
//	prefix = ["ssh", "builder@host", "cd {workspace} &&"]
//	local_transcripts = false
type WrapperRunner struct{ plugin.RunnerBase }

func runnerBase(conf M) plugin.RunnerBase {
	return plugin.RunnerBase{Conf: util.Copy(util.MapOr(conf))}
}

func registerRunners() {
	plugin.RegisterRunner("local", "builtin", func(conf M) plugin.Runner {
		return &LocalRunner{RunnerBase: runnerBase(conf)}
	})
	plugin.RegisterRunner("docker", "builtin", func(conf M) plugin.Runner {
		r := &DockerRunner{RunnerBase: runnerBase(conf)}
		r.NoLocalTranscripts = !util.Truthy(r.Conf["local_transcripts"])
		return r
	})
	plugin.RegisterRunner("wrapper", "builtin", func(conf M) plugin.Runner {
		r := &WrapperRunner{RunnerBase: runnerBase(conf)}
		r.NoLocalTranscripts = !util.Truthy(r.Conf["local_transcripts"])
		return r
	})
}

// missingKey mirrors the KeyError a Python runner raises for a missing setting.
type missingKey string

func (k missingKey) Error() string { return "KeyError: '" + string(k) + "'" }

func (r *DockerRunner) Wrap(argv []string, ctx *plugin.Ctx, env map[string]string) ([]string, error) {
	image, ok := r.Conf["image"]
	if !ok {
		return nil, missingKey("image")
	}
	ws := ctx.Workspace
	out := []string{"docker", "run", "--rm", "-i", "-v", ws + ":" + ws, "-w", ws}
	for _, extra := range []string{ctx.ConfigDir, ctx.CacheDir} { // keep transcripts/caches on the host
		if extra != "" {
			if _, err := os.Stat(extra); err == nil {
				out = append(out, "-v", extra+":"+extra)
			}
		}
	}
	for _, m := range util.Strings(r.Conf["mounts"]) {
		parts := strings.Split(m, ":")
		out = append(out, "-v", pyPath(expandUser(parts[0]))+":"+strings.Join(parts[1:], ":"))
	}
	for _, name := range util.SortedKeys(env) {
		out = append(out, "-e", name)
	}
	out = append(out, util.Strings(r.Conf["args"])...)
	out = append(out, util.Str(image))
	return append(out, argv...), nil
}

func (r *WrapperRunner) Wrap(argv []string, ctx *plugin.Ctx, env map[string]string) ([]string, error) {
	values := map[string]string{"workspace": ctx.Workspace, "run_token": ctx.RunToken}
	prefix := util.FillAll(util.Strings(r.Conf["prefix"]), values)
	// ssh and friends hand the rest to a remote shell: pass argv as ONE safely
	// quoted string whenever the prefix ends in a shell operator or the
	// profile says shell_join = true.
	var joins bool
	if v, ok := r.Conf["shell_join"]; ok && v != nil {
		joins = util.Truthy(v)
	} else if len(prefix) > 0 {
		last := pyRStrip(prefix[len(prefix)-1])
		for _, suffix := range []string{"&&", ";", "||", "|", "-c"} {
			if strings.HasSuffix(last, suffix) {
				joins = true
			}
		}
	}
	if joins {
		return append(prefix, util.ShellJoin(argv)), nil
	}
	return append(prefix, argv...), nil
}

// expandUser is Path.expanduser for "~" and "~/...".
func expandUser(p string) string {
	if p == "~" {
		return util.Home()
	}
	if strings.HasPrefix(p, "~/") {
		return util.Home() + p[1:]
	}
	return p
}

// pyPath is str(Path(p)): duplicate and trailing slashes and "." components
// dropped ("" becomes "."); ".." is kept.
func pyPath(p string) string {
	lead := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		lead = "//"
	case strings.HasPrefix(p, "/"):
		lead = "/"
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	out := lead + strings.Join(parts, "/")
	if out == "" {
		return "."
	}
	return out
}
