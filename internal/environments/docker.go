package environments

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

// DockerResult is one Docker management command's outcome.
type DockerResult struct {
	Code   int
	Stdout string
	Stderr string
}

// DockerFunc runs `docker <args>` for environment management (without the
// --config/--host prefix). handle is the persisted plan, or nil before one
// exists; timeout is in seconds. It returns an *Error when Docker cannot be
// run. Tests replace it with an in-memory engine.
type DockerFunc func(args []string, handle util.M, timeout float64) (DockerResult, error)

// SetDocker replaces the Docker management boundary (binary used in wrapped
// argv, and the command function; nil keeps the real CLI). For tests.
func (r *EnvironmentRunner) SetDocker(binary string, fn DockerFunc) {
	r.dockerBinary = binary
	if fn != nil {
		r.docker = fn
	} else {
		r.docker = r.realDocker
	}
}

// SetReport replaces the post-preparation report/inventory step. For tests.
func (r *EnvironmentRunner) SetReport(fn func(handle util.M, fresh bool) (util.M, error)) {
	if fn == nil {
		r.reportFn = r.report
		return
	}
	r.reportFn = func(_ *plugin.Ctx, handle util.M, fresh bool) (util.M, error) { return fn(handle, fresh) }
}

func (r *EnvironmentRunner) dockerPrefix(handle util.M) ([]string, error) {
	if r.dockerBinary == "" {
		binary := util.WhichIn("docker", map[string]string{"PATH": managerPath})
		if binary == "" {
			return nil, envError("Docker CLI not found on the fixed management PATH; install Docker explicitly")
		}
		resolved, err := filepath.EvalSymlinks(binary)
		if err != nil {
			resolved = binary
		}
		r.dockerBinary = resolved
	}
	var config string
	if len(handle) > 0 {
		config = util.S(handle["docker_config_dir"])
	}
	if config == "" {
		// Deliberately absent: read-only Docker operations must not load ~/.docker.
		config = pathJoin(os.TempDir(), "ajx-docker-empty-"+uuidHex())
	}
	var host any = r.profile["docker_host"]
	if len(handle) > 0 {
		host = handle["docker_host"]
	}
	if _, err := dockerHost(host, "environment.docker_host"); err != nil {
		return nil, err
	}
	return []string{r.dockerBinary, "--config", config, "--host", util.S(host)}, nil
}

// realDocker is the fakeable command interface; management never receives worker/host secrets.
func (r *EnvironmentRunner) realDocker(args []string, handle util.M, timeout float64) (DockerResult, error) {
	prefix, err := r.dockerPrefix(handle)
	if err != nil {
		return DockerResult{}, err
	}
	env := util.MergeEnv(baseEnv, map[string]string{"PATH": managerPath, "HOME": "/nonexistent"})
	argv := append(prefix, args...)
	return runManagement(argv, env, timeout, args[0])
}

func runManagement(argv []string, env map[string]string, timeout float64, verb string) (DockerResult, error) {
	if timeout <= 0 {
		timeout = 30
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = util.EnvList(env)
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return DockerResult{}, envError("Docker " + verb + " unavailable (TimeoutExpired); " +
			"check the local daemon; ownership/cleanup is unconfirmed")
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return DockerResult{}, envError("Docker " + verb + " unavailable (" + osErrorName(err) + "); " +
			"check the local daemon; ownership/cleanup is unconfirmed")
	}
	code := 0
	if cmd.ProcessState != nil {
		code = util.ExitCode(cmd.ProcessState)
	}
	return DockerResult{Code: code, Stdout: decodeReplace(stdout.Bytes()), Stderr: decodeReplace(stderr.Bytes())}, nil
}

// decodeReplace is bytes.decode(errors="replace").
func decodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			out.WriteRune(utf8.RuneError)
		} else {
			out.WriteRune(r)
		}
		b = b[size:]
	}
	return out.String()
}

func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// stderrTail is the last few stderr lines as one bounded, whitespace-collapsed, printable line.
func stderrTail(text string) string { return stderrTailN(text, 6, 600) }

func stderrTailN(text string, lines, limit int) string {
	var rows []string
	for _, row := range pySplitLines(text) {
		if strings.TrimFunc(row, isPySpace) != "" {
			rows = append(rows, row)
		}
	}
	if len(rows) > lines {
		rows = rows[len(rows)-lines:]
	}
	collapsed := strings.Join(strings.FieldsFunc(strings.Join(rows, " "), isPySpace), " ")
	var b strings.Builder
	for _, ch := range collapsed {
		if unicode.IsPrint(ch) {
			b.WriteRune(ch)
		}
	}
	tail := b.String()
	if util.RuneLen(tail) <= limit {
		return tail
	}
	return util.Clip(tail, limit-3) + "..."
}

// dockerFailure is a durable record of a failed Docker command: exit code,
// bounded stderr tail, known-engine hint.
func dockerFailure(result DockerResult) util.M {
	record := util.M{"exit_code": int64(result.Code), "stderr_tail": stderrTail(result.Stderr)}
	if vmMountError.MatchString(util.S(record["stderr_tail"])) {
		record["hint"] = VMMountHint
	}
	return record
}

// dockerError is one line: summary (exit code N): stderr tail[; consequence][; hint: ...].
func dockerError(summary string, record util.M, consequence string) string {
	tail := util.S(record["stderr_tail"])
	if tail == "" {
		tail = "(no stderr)"
	}
	text := summary + " (exit code " + pyStr(record["exit_code"]) + "): " + tail
	if consequence != "" {
		text += "; " + consequence
	}
	if hint := util.S(record["hint"]); hint != "" {
		text += "; hint: " + hint
	}
	return text
}

func (r *EnvironmentRunner) dockerJSON(args []string, handle util.M) (any, error) {
	result, err := r.docker(args, handle, 30)
	if err != nil {
		return nil, err
	}
	if result.Code != 0 {
		n := 2
		if len(args) < 2 {
			n = len(args)
		}
		return nil, envError(dockerError("Docker "+strings.Join(args[:n], " ")+" failed", dockerFailure(result),
			"check the daemon and preloaded pinned image"))
	}
	data, err := util.DecodeJSON([]byte(result.Stdout))
	if err != nil {
		return nil, envError("Docker returned invalid inspection data")
	}
	return data, nil
}

func (r *EnvironmentRunner) inspectImage(handle util.M) (util.M, error) {
	requested := util.S(r.profile["image"])
	data, err := r.dockerJSON([]string{"image", "inspect", requested}, handle)
	if err != nil {
		return nil, err
	}
	rows := util.List(data)
	if rows == nil || len(rows) != 1 || util.Map(rows[0]) == nil {
		return nil, envError("Docker image inspect did not return exactly one image")
	}
	image := util.Map(rows[0])
	imageID := util.S(image["Id"])
	if !imageIDRE.MatchString(imageID) || image["Os"] != "linux" {
		return nil, envError("Environment requires a resolved Linux sha256 image")
	}
	if imageIDRE.MatchString(requested) && imageID != requested {
		return nil, envError("Resolved image ID differs from the pinned profile")
	}
	config := util.MapOr(image["Config"])
	if util.Truthy(config["Volumes"]) {
		return nil, envError("environments." + r.id() +
			".image: images with declared VOLUMEs are unsupported (implicit writable mounts)")
	}
	var platform []string
	for _, k := range []string{"Os", "Architecture", "Variant"} {
		if util.Truthy(image[k]) {
			platform = append(platform, util.Str(image[k]))
		}
	}
	imagePlatform := strings.Join(platform, "/")
	if want := util.S(r.profile["platform"]); want != "" && want != imagePlatform {
		return nil, envError("Pinned image platform does not match environments." + r.id() + ".platform")
	}
	var names []string
	for _, entry := range util.List(config["Env"]) {
		s, ok := entry.(string)
		key, _, hasEq := strings.Cut(s, "=")
		if !ok || !hasEq || !envNameRE.MatchString(key) {
			return nil, envError("Image contains an unsupported environment-variable name")
		}
		names = append(names, key)
	}
	digests := util.List(image["RepoDigests"])
	if digests == nil {
		digests = []any{}
	}
	return util.M{"requested": requested, "id": imageID, "platform": imagePlatform,
		"repo_digests": util.DeepCopy(digests), "environment_names": toAnyStrings(nonNilStrings(util.Uniq(names))),
		"environment_values_inherited": false}, nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (r *EnvironmentRunner) daemon(handle util.M) (util.M, error) {
	data, err := r.dockerJSON([]string{"info", "--format", "{{json .}}"}, handle)
	if err != nil {
		return nil, err
	}
	info := util.Map(data)
	if info == nil || info["OSType"] != "linux" {
		return nil, envError("A local Linux Docker daemon is required")
	}
	keys := []string{"MemoryLimit", "SwapLimit", "CpuCfsQuota", "PidsLimit"}
	supported := util.M{}
	var missing []string
	for _, k := range keys {
		enabled := info[k] == true
		supported[k] = enabled
		if !enabled {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, envError("Docker cannot enforce the declared resource limits: " + strings.Join(missing, ", "))
	}
	security := util.List(info["SecurityOptions"])
	if security == nil {
		security = []any{}
	}
	seccomp := false
	for _, v := range security {
		s := util.Str(v)
		if strings.Contains(s, "seccomp") && !strings.Contains(s, "unconfined") {
			seccomp = true
		}
	}
	if !seccomp {
		return nil, envError("Docker must provide its seccomp syscall boundary; unconfined daemons are unsupported")
	}
	return util.M{"os": info["OSType"], "architecture": info["Architecture"],
		"server_version": info["ServerVersion"], "resource_support": supported, "security_options": security}, nil
}

// find inspects the owned container; nil when Docker confirms it is absent.
func (r *EnvironmentRunner) find(handle util.M, identifier string) (util.M, error) {
	ident := identifier
	if ident == "" {
		ident = util.S(handle["name"])
	}
	result, err := r.docker([]string{"container", "inspect", ident}, handle, 30)
	if err != nil {
		return nil, err
	}
	if result.Code != 0 {
		// Do not confuse daemon/auth/transport failure with "already removed".
		byID := containerIDRE.MatchString(ident)
		filter := "label=" + labelPrefix + "owner=" + util.Str(handle["owner"])
		if byID {
			filter = "id=" + ident
		}
		listing, err := r.docker([]string{"container", "ls", "--all", "--no-trunc", "--filter", filter,
			"--format", "{{.ID}}"}, handle, 30)
		if err != nil {
			return nil, err
		}
		if listing.Code == 0 && strings.TrimSpace(listing.Stdout) == "" {
			return nil, nil
		}
		if listing.Code == 0 && !byID {
			candidates := strings.Fields(listing.Stdout)
			if len(candidates) == 1 && containerIDRE.MatchString(candidates[0]) {
				// A host administrator may rename a container. Labels remain our durable handle.
				return r.find(handle, candidates[0])
			}
		}
		return nil, envError("Cannot inspect container; its presence and ownership are unknown")
	}
	data, err := util.DecodeJSON([]byte(result.Stdout))
	rows := util.List(data)
	var row util.M
	if err == nil && len(rows) == 1 {
		row = util.Map(rows[0])
	}
	if row == nil || !containerIDRE.MatchString(util.S(row["Id"])) {
		return nil, envError("Docker returned invalid container inspection data")
	}
	labels, ok := util.Get(row, "Config", "Labels").(util.M)
	if !ok {
		labels = util.M{}
	}
	if util.Get(row, "Config", "Labels") != nil && !ok {
		return nil, envError("Container ownership labels do not match; refusing to execute or remove it")
	}
	for k, v := range util.MapOr(handle["labels"]) {
		if labels[k] != v {
			return nil, envError("Container ownership labels do not match; refusing to execute or remove it")
		}
	}
	return row, nil
}

type mountSpec struct {
	source, target, access string
}

func (r *EnvironmentRunner) expectedMounts(handle util.M) []mountSpec {
	var out []mountSpec
	for _, kv := range handlePaths(handle) {
		out = append(out, mountSpec{kv[1], kv[1], "write"})
	}
	for _, m := range util.Maps(handle["mounts"]) {
		out = append(out, mountSpec{util.S(m["host_source"]), util.S(m["target"]), util.S(m["access"])})
	}
	return out
}

func strList(v any) ([]string, bool) {
	items, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(items))
	for i, x := range items {
		s, isStr := x.(string)
		if !isStr {
			return nil, false
		}
		out[i] = s
	}
	return out, true
}

func numEq(v any, want float64) bool {
	if _, isBool := v.(bool); isBool {
		return false
	}
	f, ok := util.Num(v)
	return ok && f == want
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// checkContainer verifies the running container matches the owned plan.
func (r *EnvironmentRunner) checkContainer(data, handle util.M) error {
	config, host := util.MapOr(data["Config"]), util.MapOr(data["HostConfig"])
	memory := float64(r.limitInt("memory_mb") * 1024 * 1024)
	cpus, _ := util.Num(r.limits()["cpus"])
	lifetime := r.limitInt("lifetime_seconds")
	entrypoint, _ := strList(config["Entrypoint"])
	cmdItems := util.List(config["Cmd"])
	cmd3 := make([]string, 0, 3)
	for i := 0; i < len(cmdItems) && i < 3; i++ {
		cmd3 = append(cmd3, util.Str(cmdItems[i]))
	}
	anyStr := func(v any, match func(string) bool) bool {
		for _, x := range util.List(v) {
			if match(util.Str(x)) {
				return true
			}
		}
		return false
	}
	restart := util.MapOr(host["RestartPolicy"])
	restartName, hasName := restart["Name"]
	if !hasName {
		restartName = "no"
	}
	pidMode, hasPid := host["PidMode"]
	if !hasPid {
		pidMode = ""
	}
	type check struct {
		name   string
		passed bool
	}
	checks := []check{
		{"image", data["Image"] == util.Get(handle, "image", "id")},
		{"supervisor", config["User"] == "0:0" && len(entrypoint) == 1 && entrypoint[0] == "/usr/bin/env" &&
			strings.Join(cmd3, "\x00") == strings.Join([]string{"-i", "PATH=" + containerPath, "/bin/sleep"}, "\x00")},
		{"read_only", host["ReadonlyRootfs"] == true},
		{"unprivileged", host["Privileged"] == false && !util.Truthy(host["CapAdd"]) &&
			anyStr(host["CapDrop"], func(s string) bool { return strings.ToLower(s) == "all" })},
		{"no_new_privileges", anyStr(host["SecurityOpt"], func(s string) bool {
			return s == "no-new-privileges" || s == "no-new-privileges=true"
		})},
		{"lifetime", host["AutoRemove"] == true && host["Init"] == true && (restartName == "" || restartName == "no")},
		{"network", host["NetworkMode"] == r.profile["network"] && !util.Truthy(host["PortBindings"]) &&
			!util.Truthy(host["PublishAllPorts"])},
		{"namespaces", pidMode == "" && host["IpcMode"] == "none" && host["CgroupnsMode"] == "private"},
		{"resources", numEq(host["Memory"], memory) && numEq(host["MemorySwap"], memory) &&
			numEq(host["PidsLimit"], float64(r.limitInt("pids"))) &&
			numEq(host["NanoCpus"], math.RoundToEven(cpus*1000000000))},
	}
	extra := false
	for _, k := range []string{"Binds", "VolumesFrom", "Devices", "DeviceRequests", "ExtraHosts", "Links", "GroupAdd"} {
		if util.Truthy(host[k]) {
			extra = true
		}
	}
	checks = append(checks, check{"no_extra_host_access", !extra})
	if len(cmdItems) == 4 && isASCIIDigits(util.Str(cmdItems[3])) {
		n, err := strconv.ParseInt(util.Str(cmdItems[3]), 10, 64)
		checks[1].passed = checks[1].passed && err == nil && n > 0 && n <= lifetime
	} else {
		checks[1].passed = false
	}
	key := func(source, target any, rw any) string {
		return util.Str(source) + "\x00" + util.Str(target) + "\x00" + util.Str(rw) + "\x00" + typeTag(source, target, rw)
	}
	wanted := map[string]bool{}
	for _, m := range r.expectedMounts(handle) {
		wanted[key(m.source, m.target, m.access == "write")] = true
	}
	observed := map[string]bool{}
	allKnown := true
	for _, m := range util.List(data["Mounts"]) {
		mm := util.MapOr(m)
		if mm["Type"] == "bind" {
			observed[key(mm["Source"], mm["Destination"], mm["RW"])] = true
		} else if !(mm["Type"] == "tmpfs" && mm["Destination"] == "/tmp") {
			allKnown = false
		}
	}
	sameSet := len(wanted) == len(observed)
	for k := range wanted {
		if !observed[k] {
			sameSet = false
		}
	}
	checks = append(checks, check{"mounts", sameSet && allKnown})
	tmpfs := util.Map(host["Tmpfs"])
	wantTmpfs := "rw,nosuid,nodev,noexec,size=" + strconv.FormatInt(r.limitInt("tmpfs_mb"), 10) + "m,mode=1777"
	checks = append(checks, check{"tmpfs", tmpfs != nil && len(tmpfs) == 1 && tmpfs["/tmp"] == wantTmpfs})
	configured := util.List(host["Mounts"])
	nonrecursive := len(configured) == len(wanted)
	for _, m := range configured {
		mm := util.MapOr(m)
		options := util.MapOr(mm["BindOptions"])
		if !(mm["Type"] == "bind" && options["NonRecursive"] == true && options["Propagation"] == "rprivate") {
			nonrecursive = false
		}
	}
	checks = append(checks, check{"nonrecursive_mounts", nonrecursive})
	var failed []string
	for _, c := range checks {
		if !c.passed {
			failed = append(failed, c.name)
		}
	}
	if len(failed) > 0 {
		return envError("Container configuration differs from the owned plan: " + strings.Join(failed, ", "))
	}
	state := util.MapOr(data["State"])
	if !util.Truthy(state["Running"]) || util.Truthy(state["Paused"]) || util.Truthy(state["Restarting"]) {
		return envError("Owned environment is not running; it will not be restarted or recreated")
	}
	return nil
}

func typeTag(values ...any) string {
	var b strings.Builder
	for _, v := range values {
		switch v.(type) {
		case string:
			b.WriteByte('s')
		case bool:
			b.WriteByte('b')
		case nil:
			b.WriteByte('n')
		default:
			b.WriteByte('o')
		}
	}
	return b.String()
}

func (r *EnvironmentRunner) createArgv(handle util.M) ([]string, error) {
	expires, _ := util.Num(handle["expires_at"])
	lifetime := r.limitInt("lifetime_seconds")
	left := int64(expires - timeNow())
	if lifetime < left {
		left = lifetime
	}
	if left <= 0 {
		return nil, envError("Environment plan has expired; no worker will be started")
	}
	cpus, _ := util.Num(r.limits()["cpus"])
	memory := strconv.FormatInt(r.limitInt("memory_mb"), 10) + "m"
	args := []string{"run", "--detach", "--rm", "--pull", "never", "--name", util.S(handle["name"]),
		"--init", "--user", "0:0", "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges=true", "--network", util.S(r.profile["network"]),
		"--ipc", "none", "--cgroupns", "private", "--no-healthcheck", "--log-driver", "none",
		"--cpus", pyFloatRepr(cpus), "--memory", memory,
		"--memory-swap", memory, "--pids-limit", strconv.FormatInt(r.limitInt("pids"), 10),
		"--ulimit", "core=0:0", "--stop-timeout", "1",
		"--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=" + strconv.FormatInt(r.limitInt("tmpfs_mb"), 10) + "m,mode=1777",
		"--workdir", handlePath(handle, "workspace"), "--entrypoint", "/usr/bin/env"}
	if p := util.S(r.profile["platform"]); p != "" {
		args = append(args, "--platform", p)
	}
	labels := util.MapOr(handle["labels"])
	for _, k := range util.SortedKeys(labels) {
		args = append(args, "--label", k+"="+util.Str(labels[k]))
	}
	// Clear all image ENV, including loader/startup injection, before any entrypoint starts.
	for _, key := range clearedEnv(util.MapOr(handle["image"])) {
		args = append(args, "--env", key+"=")
	}
	for _, m := range r.expectedMounts(handle) {
		value := "type=bind,source=" + m.source + ",target=" + m.target + ",bind-propagation=rprivate,bind-recursive=disabled"
		if m.access == "read" {
			value += ",readonly"
		}
		args = append(args, "--mount", value)
	}
	args = append(args, util.S(util.Get(handle, "image", "id")), "-i", "PATH="+containerPath, "/bin/sleep",
		strconv.FormatInt(left, 10))
	return args, nil
}
