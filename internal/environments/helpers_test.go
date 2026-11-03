package environments

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

const testImage = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const imageSecret = "synthetic-image-env-value"

const bindError = "docker: Error response from daemon: invalid mount config for type \"bind\": " +
	"bind source path does not exist: /private/tmp/ajx-example/workspace.\n\n" +
	"Run 'docker run --help' for more information\n"

// tempRoot is TempTest: a resolved temporary root with a trial/ base directory.
type tempRoot struct {
	t    *testing.T
	root string
	base string
}

func newTemp(t *testing.T) *tempRoot {
	t.Helper()
	dir, err := os.MkdirTemp("", "ajx-env-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	root := realpath(dir)
	base := filepath.Join(root, "trial")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	return &tempRoot{t: t, root: root, base: base}
}

func (tr *tempRoot) fixture(name string) string {
	tr.t.Helper()
	path := filepath.Join(tr.base, name)
	if err := os.Mkdir(path, 0o755); err != nil {
		tr.t.Fatal(err)
	}
	writeFile(tr.t, filepath.Join(path, "input.txt"), "synthetic starting data\n")
	return path
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// newCtx is the Python context(root, name, create=True) helper.
func newCtx(t *testing.T, root, name string, create bool) *plugin.Ctx {
	t.Helper()
	attempt := filepath.Join(root, name)
	runDir := filepath.Join(root, "reports", name)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := &plugin.Ctx{RunDir: runDir, State: util.M{}, Env: map[string]string{}, Timeout: 10,
		Cell: util.M{"id": name}}
	for _, key := range Roots {
		path := filepath.Join(attempt, key)
		if create {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		setRoot(ctx, key, path)
	}
	return ctx
}

func setRoot(ctx *plugin.Ctx, key, path string) {
	switch key {
	case "workspace":
		ctx.Workspace = path
	case "config_dir":
		ctx.ConfigDir = path
	case "cache_dir":
		ctx.CacheDir = path
	case "home_dir":
		ctx.HomeDir = path
	}
}

func ownAll(ctx *plugin.Ctx) {
	paths := util.M{}
	var owned []any
	for _, key := range Roots {
		paths[key] = ctxRoot(ctx, key)
		owned = append(owned, ctxRoot(ctx, key))
	}
	ctx.State["paths"] = paths
	ctx.State["owned_paths"] = owned
}

// roundTrip simulates the caller's durable JSON round-trip.
func roundTrip(t *testing.T, v any) util.M {
	t.Helper()
	data, err := util.MarshalIndent(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := util.DecodeJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	return out.(util.M)
}

func persist(t *testing.T, r *EnvironmentRunner, ctx *plugin.Ctx) util.M {
	t.Helper()
	plan, err := r.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx.State["environment"] = roundTrip(t, plan)
	return ctx.State["environment"].(util.M)
}

func containerProfile(t *testing.T, base string, options util.M) util.M {
	t.Helper()
	conf := util.M{"backend": "container", "image": testImage}
	for k, v := range options {
		conf[k] = v
	}
	out, err := NormalizeProfiles(util.M{"test": conf}, base)
	if err != nil {
		t.Fatal(err)
	}
	return out["test"].(util.M)
}

func localRunner(t *testing.T, base string, options util.M) *EnvironmentRunner {
	t.Helper()
	conf := util.M{"backend": "local"}
	for k, v := range options {
		conf[k] = v
	}
	out, err := NormalizeProfiles(util.M{"local": conf}, base)
	if err != nil {
		t.Fatal(err)
	}
	return mustNew(t, out["local"].(util.M))
}

func mustNew(t *testing.T, profile util.M) *EnvironmentRunner {
	t.Helper()
	r, err := New(profile)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// wantErr asserts err is an *Error of kind (or any kind when "") whose message matches pattern.
func wantErr(t *testing.T, err error, kind, pattern string) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error matching %q", pattern)
	}
	e, ok := asError(err)
	if !ok {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if kind == KindValue && !e.IsValue() || kind == KindEnvironment && e.TypeName() != KindEnvironment {
		t.Fatalf("expected %s, got %s: %s", kind, e.TypeName(), e.Msg)
	}
	if !regexp.MustCompile(pattern).MatchString(e.Msg) {
		t.Fatalf("error %q does not match %q", e.Msg, pattern)
	}
	return e
}

func invalid(t *testing.T, base string, conf util.M, field string) {
	t.Helper()
	_, err := NormalizeProfiles(util.M{"test": conf}, base)
	wantErr(t, err, KindValue, field)
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ------------------------------------------------------------------ fake Docker

func option(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func options(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func hasPrefix(args []string, prefix ...string) bool {
	if len(args) < len(prefix) {
		return false
	}
	for i := range prefix {
		if args[i] != prefix[i] {
			return false
		}
	}
	return true
}

// fakeDocker is an in-memory Docker command boundary; it never executes Docker or shell commands.
type fakeDocker struct {
	calls           [][]string
	containers      map[string]util.M
	order           []string
	image           util.M
	info            util.M
	failCreate      bool
	failCancel      bool
	failRemove      bool
	unreachable     bool
	replaceAtRemove bool
	sequence        int
	// Diagnostics (DiagnosticDocker): engine-style stderr for a refused creation and the doctor probe.
	diagnostic   bool
	createStderr string
	probeStderr  *string
	probeError   string
	probeRuns    []util.M
	override     func(args []string) (DockerResult, bool)
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{containers: map[string]util.M{},
		image: util.M{"Id": testImage, "Os": "linux", "Architecture": "amd64", "RepoDigests": []any{},
			"Config": util.M{"Env": []any{"HOME=/root", "PATH=/image-only", "IMAGE_SECRET=" + imageSecret,
				"BASH_ENV=/image-startup"}, "Volumes": nil}},
		info: util.M{"OSType": "linux", "Architecture": "x86_64", "ServerVersion": "synthetic",
			"MemoryLimit": true, "SwapLimit": true, "CpuCfsQuota": true, "PidsLimit": true,
			"SecurityOptions": []any{"name=seccomp,profile=builtin", "name=cgroupns"}},
		createStderr: bindError}
}

func (f *fakeDocker) runner(t *testing.T, profile util.M) *EnvironmentRunner {
	r := mustNew(t, profile)
	r.SetDocker("/synthetic/docker", f.call)
	return r
}

func (f *fakeDocker) put(name string, c util.M) {
	if _, ok := f.containers[name]; !ok {
		f.order = append(f.order, name)
	}
	f.containers[name] = c
}

func (f *fakeDocker) remove(name string) {
	delete(f.containers, name)
	for i, n := range f.order {
		if n == name {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
}

func (f *fakeDocker) count(prefix ...string) int {
	n := 0
	for _, c := range f.calls {
		if hasPrefix(c, prefix...) {
			n++
		}
	}
	return n
}

func mountFields(value string) map[string]any {
	fields := map[string]any{}
	for _, v := range strings.Split(value, ",") {
		if k, val, ok := strings.Cut(v, "="); ok {
			fields[k] = val
		} else {
			fields[v] = true
		}
	}
	return fields
}

func (f *fakeDocker) call(args []string, handle util.M, timeout float64) (DockerResult, error) {
	args = append([]string(nil), args...)
	if f.override != nil {
		if res, ok := f.override(args); ok {
			f.calls = append(f.calls, args)
			return res, nil
		}
	}
	if f.diagnostic && args[0] == "run" && !util.Contains(args, "--detach") {
		f.calls = append(f.calls, args)
		if f.probeError != "" {
			return DockerResult{}, envError(f.probeError)
		}
		mount := mountFields(option(args, "--mount"))
		source := util.S(mount["source"])
		run := util.M{"args": args, "mount": mount, "source": source, "existed": isDir(source)}
		if st, err := os.Lstat(source); err == nil {
			run["mode"] = int(st.Mode().Perm())
		}
		f.probeRuns = append(f.probeRuns, run)
		if f.probeStderr != nil {
			return DockerResult{Code: 125, Stderr: *f.probeStderr}, nil
		}
		return DockerResult{}, nil
	}
	f.calls = append(f.calls, args)
	if f.unreachable {
		return DockerResult{}, envError("synthetic Docker transport failure")
	}
	switch {
	case hasPrefix(args, "image", "inspect"):
		return DockerResult{Stdout: util.Compact([]any{f.image})}, nil
	case hasPrefix(args, "info"):
		return DockerResult{Stdout: util.Compact(f.info)}, nil
	case args[0] == "run":
		f.sequence++
		identity := fmt.Sprintf("%064x", f.sequence)
		name := option(args, "--name")
		labels := util.M{}
		for _, v := range options(args, "--label") {
			k, val, _ := strings.Cut(v, "=")
			labels[k] = val
		}
		var actual, configured []any
		for _, value := range options(args, "--mount") {
			fields := mountFields(value)
			_, readonly := fields["readonly"]
			actual = append(actual, util.M{"Type": "bind", "Source": fields["source"], "Destination": fields["target"],
				"RW": !readonly, "Propagation": "rprivate"})
			configured = append(configured, util.M{"Type": "bind", "Source": fields["source"], "Target": fields["target"],
				"ReadOnly": readonly, "BindOptions": util.M{"Propagation": fields["bind-propagation"],
					"NonRecursive": fields["bind-recursive"] == "disabled"}})
		}
		tmpfsPath, tmpfsOptions, _ := strings.Cut(option(args, "--tmpfs"), ":")
		// Docker can expose tmpfs here; it must not be mistaken for an unapproved volume.
		actual = append(actual, util.M{"Type": "tmpfs", "Source": "", "Destination": tmpfsPath, "RW": true})
		mem := strings.TrimSuffix(option(args, "--memory"), "m")
		swap := strings.TrimSuffix(option(args, "--memory-swap"), "m")
		memN, _ := strconv.ParseInt(mem, 10, 64)
		swapN, _ := strconv.ParseInt(swap, 10, 64)
		pids, _ := strconv.ParseInt(option(args, "--pids-limit"), 10, 64)
		cpus, _ := strconv.ParseFloat(option(args, "--cpus"), 64)
		host := util.M{
			"ReadonlyRootfs": util.Contains(args, "--read-only"), "Privileged": false, "CapAdd": []any{},
			"CapDrop": toAnyStrings(options(args, "--cap-drop")), "SecurityOpt": toAnyStrings(options(args, "--security-opt")),
			"AutoRemove": util.Contains(args, "--rm"), "Init": util.Contains(args, "--init"),
			"RestartPolicy": util.M{"Name": "no"}, "NetworkMode": option(args, "--network"), "PortBindings": util.M{},
			"PublishAllPorts": false, "PidMode": "", "IpcMode": option(args, "--ipc"),
			"CgroupnsMode": option(args, "--cgroupns"),
			"Memory":       memN * 1024 * 1024, "MemorySwap": swapN * 1024 * 1024, "PidsLimit": pids,
			"NanoCpus": int64(cpus*1000000000 + 0.5),
			"Binds":    nil, "VolumesFrom": nil, "Devices": []any{}, "DeviceRequests": []any{},
			"ExtraHosts": nil, "Links": nil, "GroupAdd": nil,
			"Tmpfs": util.M{tmpfsPath: tmpfsOptions}, "Mounts": configured,
		}
		imageAt := 0
		for i, a := range args {
			if a == testImage {
				imageAt = i
			}
		}
		f.put(name, util.M{
			"Id": identity, "Name": "/" + name, "Image": testImage, "HostConfig": host, "Mounts": actual,
			"Config": util.M{"Labels": labels, "User": option(args, "--user"),
				"Entrypoint": []any{option(args, "--entrypoint")}, "Cmd": toAnyStrings(args[imageAt+1:])},
			"State": util.M{"Running": true, "Paused": false, "Restarting": false},
		})
		if f.failCreate {
			if f.diagnostic {
				// The engine refused the run: nothing was created and stderr says why.
				f.remove(name)
				return DockerResult{Code: 125, Stderr: f.createStderr}, nil
			}
			return DockerResult{Code: 1, Stdout: identity + "\n", Stderr: "synthetic"}, nil
		}
		return DockerResult{Stdout: identity + "\n", Stderr: "synthetic"}, nil
	case hasPrefix(args, "container", "inspect"):
		ident := args[2]
		for _, name := range f.order {
			c := f.containers[name]
			if name == ident || c["Id"] == ident {
				return DockerResult{Stdout: util.Compact([]any{util.DeepCopy(c)})}, nil
			}
		}
		return DockerResult{Code: 1, Stdout: "[]", Stderr: "No such container"}, nil
	case hasPrefix(args, "container", "ls"):
		criterion := option(args, "--filter")
		var ids []string
		for _, name := range f.order {
			c := f.containers[name]
			switch {
			case strings.HasPrefix(criterion, "id="):
				if c["Id"] == criterion[3:] {
					ids = append(ids, util.S(c["Id"]))
				}
			case strings.HasPrefix(criterion, "label="):
				k, v, _ := strings.Cut(criterion[6:], "=")
				if util.Get(c, "Config", "Labels", k) == v {
					ids = append(ids, util.S(c["Id"]))
				}
			}
		}
		return DockerResult{Stdout: strings.Join(ids, "\n")}, nil
	case hasPrefix(args, "exec"):
		if !util.Contains(args, StopWorkers) {
			panic(fmt.Sprintf("exec without the stop script: %q", args))
		}
		if f.failCancel {
			return DockerResult{Code: 1}, nil
		}
		return DockerResult{}, nil
	case hasPrefix(args, "container", "rm", "--force"):
		identity := args[3]
		found := ""
		for _, name := range f.order {
			if f.containers[name]["Id"] == identity {
				found = name
			}
		}
		if f.replaceAtRemove && found != "" {
			c := util.DeepCopy(f.containers[found]).(util.M)
			c["Id"] = strings.Repeat("f", 64)
			util.MapOr(c["Config"])["Labels"] = util.M{}
			f.containers[found] = c
			return DockerResult{Code: 1, Stderr: "original container already disappeared"}, nil
		}
		if f.failRemove {
			return DockerResult{Code: 1, Stderr: "synthetic busy container"}, nil
		}
		if found != "" {
			f.remove(found)
		}
		return DockerResult{Stdout: identity}, nil
	}
	panic(fmt.Sprintf("unexpected Docker command: %q", args))
}

func result(stdout string, exitCode any, timedOut bool) util.M {
	return util.M{"exit_code": exitCode, "timed_out": timedOut, "stdout": stdout, "stderr": "",
		"started_at": "2026-01-01T00:00:00Z", "stopped_at": "2026-01-01T00:00:01Z"}
}

// fakeProcesses replaces tailProcess for container tests (Python patches _tail_process).
type fakeProcesses struct {
	engine     *fakeDocker
	executions []util.M
	handler    func(argv []string) util.M
}

func (p *fakeProcesses) defaultResult(argv []string) util.M {
	command := argv[len(argv)-1]
	if strings.HasPrefix(command, "printf 'system='") {
		uid, gid, _ := strings.Cut(option(argv, "--user"), ":")
		return result(fmt.Sprintf("system=Linux\nrelease=synthetic\nmachine=x86_64\nuid=%s\ngid=%s\n"+
			"CapEff:=0000000000000000\nNoNewPrivs:=1\nSeccomp:=2\nsupervisor_uid=0\n"+
			"interface=lo\nos_ID=synthetic-linux\n", uid, gid), int64(0), false)
	}
	if strings.Contains(command, "version") {
		return result("synthetic-tool 1.0\n", int64(0), false)
	}
	return result("", int64(0), false)
}

func (p *fakeProcesses) run(_ context.Context, argv []string, cwd string, timeout float64, env map[string]string) (util.M, error) {
	p.executions = append(p.executions, util.M{"argv": argv, "env": env, "cwd": cwd, "timeout": timeout})
	if p.handler != nil {
		return p.handler(argv), nil
	}
	return p.defaultResult(argv), nil
}

func installFakeProcesses(t *testing.T, engine *fakeDocker) *fakeProcesses {
	p := &fakeProcesses{engine: engine}
	old := tailProcess
	tailProcess = p.run
	t.Cleanup(func() { tailProcess = old })
	return p
}

func lastArg(argv []string) string { return argv[len(argv)-1] }
