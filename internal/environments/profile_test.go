package environments

import (
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

func TestLocalDefaultsAreUsableObservationOnly(t *testing.T) {
	tr := newTemp(t)
	out, err := NormalizeProfiles(util.M{"ajx-local": util.M{"backend": "local"}}, tr.base)
	noErr(t, err)
	conf := out["ajx-local"].(util.M)
	r := mustNew(t, conf)
	if r.Backend() != "local" || r.Name() != "local" {
		t.Fatalf("backend %q name %q", r.Backend(), r.Name())
	}
	if util.Compact(conf["permissions"]) != `{"filesystem":"observed","package_install":"unrestricted","root":"inherit"}` {
		t.Fatal(conf["permissions"])
	}
	if len(util.MapOr(conf["limits"])) != 0 || conf["network"] != "inherit" {
		t.Fatal(conf)
	}
	if _, ok := r.Describe()["_base_dir"]; ok {
		t.Fatal("describe exposes _base_dir")
	}
	if util.Get(r.Doctor(nil), "capabilities", "filesystem_isolation", "state") != "unsupported" {
		t.Fatal("local filesystem isolation must be unsupported")
	}
}

func TestUnknownNestedFieldsAndCloudBackendsFailClosed(t *testing.T) {
	tr := newTemp(t)
	examples := []struct {
		conf  util.M
		field string
	}{
		{util.M{"backend": "aws"}, "backend"},
		{util.M{"backend": "local", "privileged": true}, "privileged"},
		{util.M{"backend": "container", "image": testImage, "args": []any{"--privileged"}}, "args"},
		{util.M{"backend": "container", "image": testImage, "permissions": util.M{"sudo": true}}, "permissions.sudo"},
		{util.M{"backend": "container", "image": testImage, "limits": util.M{"disk_gb": int64(1)}}, "limits.disk_gb"},
		{util.M{"backend": "local", "tools": []any{util.M{"name": "x", "argv": []any{"x"}, "hidden": int64(1)}}}, `tools\[0\].hidden`},
		{util.M{"backend": "local", "_base_dir": tr.base}, "_base_dir"},
	}
	for _, ex := range examples {
		invalid(t, tr.base, ex.conf, ex.field)
	}
}

func TestLocalRejectsRequiredSandboxesAndPermissionClaims(t *testing.T) {
	tr := newTemp(t)
	for _, capability := range RequiredCapabilities {
		invalid(t, tr.base, util.M{"backend": "local", "required": []any{capability}}, "required")
	}
	for _, kv := range [][2]string{{"filesystem", "isolated"}, {"root", "forbid"}, {"package_install", "user"}} {
		invalid(t, tr.base, util.M{"backend": "local", "permissions": util.M{kv[0]: kv[1]}}, "permissions."+kv[0])
	}
	invalid(t, tr.base, util.M{"backend": "local", "network": "none"}, "network")
	invalid(t, tr.base, util.M{"backend": "local", "limits": util.M{"cpus": int64(1)}}, "limits")
	invalid(t, tr.base, util.M{"backend": "local", "image": testImage}, "image")
}

func TestContainerRejectsMutableImagesAndUnsupportedPermissions(t *testing.T) {
	tr := newTemp(t)
	for _, image := range []string{"python:3-alpine", "latest", "sha256:abc", "--privileged", testImage + "\n"} {
		invalid(t, tr.base, util.M{"backend": "container", "image": image}, "image")
	}
	for _, kv := range [][2]string{{"root", "allow"}, {"package_install", "system"}, {"package_install", "none"},
		{"filesystem", "observed"}} {
		invalid(t, tr.base, util.M{"backend": "container", "image": testImage, "permissions": util.M{kv[0]: kv[1]}},
			"permissions."+kv[0])
	}
	invalid(t, tr.base, util.M{"backend": "container", "image": testImage, "network": "allowlist"}, "network")
	invalid(t, tr.base, util.M{"backend": "container", "image": testImage, "network": "bridge",
		"required": []any{"network_isolation"}}, "required")
}

func TestPinsLimitsAndRequirements(t *testing.T) {
	tr := newTemp(t)
	digest := "example.invalid/tools@sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	conf := containerProfile(t, tr.base, util.M{"image": digest, "platform": "linux/amd64",
		"required": []any{"filesystem_isolation", "non_root"}})
	if conf["image"] != digest || util.Get(conf, "limits", "lifetime_seconds") != int64(3600) {
		t.Fatal(conf)
	}
	for _, limits := range []util.M{{"cpus": int64(0)}, {"memory_mb": true}, {"pids": 1.5}, {"lifetime_seconds": int64(-1)},
		{"tmpfs_mb": int64(2048)}, {"cpus": math.NaN()}, {"cpus": math.Inf(1)}} {
		invalid(t, tr.base, util.M{"backend": "container", "image": testImage, "limits": limits}, "limits")
	}
	invalid(t, tr.base, util.M{"backend": "container", "image": testImage, "required": []any{"unsupported"}}, "required")
	invalid(t, tr.base, util.M{"backend": "container", "image": testImage, "required": []any{"lifetime", "lifetime"}}, "required")
	invalid(t, tr.base, util.M{"backend": "container", "image": testImage, "docker_host": "tcp://example.invalid:2375"}, "docker_host")
	invalid(t, tr.base, util.M{"backend": "container", "image": testImage, "docker_host": "unix:///tmp/../engine"}, "docker_host")
	invalid(t, tr.base, util.M{"backend": "local", "id": "different"}, `\.id`)
}

func TestValidationMessagesMatchPython(t *testing.T) {
	tr := newTemp(t)
	cases := []struct {
		conf util.M
		msg  string
	}{
		{util.M{"backend": "aws"}, "environments.test.backend: expected container | local"},
		{util.M{"backend": "container", "image": testImage, "limits": util.M{"cpus": int64(0)}},
			"environments.test.limits.cpus: expected a number between 0.01 and 1024"},
		{util.M{"backend": "container", "image": testImage, "limits": util.M{"pids": 1.5}},
			"environments.test.limits.pids: expected an integer between 16 and 65536"},
		{util.M{"backend": "container", "image": testImage, "limits": util.M{"memory_mb": true}},
			"environments.test.limits.memory_mb: expected a number"},
		{util.M{"backend": "container", "image": testImage, "required": []any{"x"}},
			"environments.test.required: unsupported capability 'x'"},
		{util.M{"backend": "container", "image": testImage, "permissions": util.M{"root": "allow"}},
			"environments.test.permissions.root: container supports only 'forbid'; the requested restriction cannot be enforced"},
		{util.M{"backend": "local", "required": []any{"lifetime"}}, "environments.test.required: local/inherit cannot enforce lifetime"},
		{util.M{"backend": "local", "zeta": 1, "alpha": 2}, "environments.test.alpha: unknown field; unsupported constraints are not ignored"},
	}
	for _, c := range cases {
		_, err := NormalizeProfiles(util.M{"test": c.conf}, tr.base)
		if err == nil || err.Error() != c.msg {
			t.Fatalf("got %v, want %q", err, c.msg)
		}
	}
}

func TestProfileHashMatchesPythonGolden(t *testing.T) {
	// Computed with the Python CLI: EnvironmentRunner(normalize_profiles(raw, "/")[name])._profile_hash().
	raw := util.M{
		"c": util.M{"backend": "container", "image": testImage, "limits": util.M{"cpus": int64(2), "memory_mb": 2048.0},
			"platform": "linux/arm64", "required": []any{"lifetime"},
			"tools": []any{util.M{"name": "py", "argv": []any{"python3", "--version", ""}, "required": false}}},
		"l": util.M{"backend": "local"},
	}
	out, err := NormalizeProfiles(raw, "/")
	noErr(t, err)
	want := map[string]string{
		"c": "6c2097328b18b92bb6e5c321993cdaec93fc8efcc9628e543f1edc4f0793ac29",
		"l": "735911d6d41c21ac8d3675969d781380ec9d15f25d909826f214b6534d7c9aaa",
	}
	for name, hash := range want {
		r := mustNew(t, out[name].(util.M))
		if got := r.profileHash(); got != hash {
			t.Fatalf("%s: profile hash %s, Python %s (%s)", name, got, hash, pyDumps(r.Profile(), true))
		}
		// A JSON round trip (saved cleanup profiles) must not change the hash.
		again := mustNew(t, roundTrip(t, r.Profile()))
		if again.profileHash() != hash {
			t.Fatalf("%s: hash changed after a JSON round trip", name)
		}
	}
}

func TestMountTargetsAndSourcesMustBeExplicitAndBounded(t *testing.T) {
	tr := newTemp(t)
	fixture := tr.fixture("fixtures")
	good := util.M{"source": "fixtures", "target": "/fixtures", "access": "read"}
	conf := containerProfile(t, tr.base, util.M{"mounts": []any{good}})
	if util.Get(util.Maps(conf["mounts"])[0], "source") != fixture {
		t.Fatal(conf["mounts"])
	}
	with := func(key, value string) util.M {
		m := util.Copy(good)
		m[key] = value
		return util.M{"backend": "container", "image": testImage, "mounts": []any{m}}
	}
	for _, source := range []string{".", "..", "../trial/fixtures", tr.root, util.Home(), "missing"} {
		invalid(t, tr.base, with("source", source), "source")
	}
	for _, target := range []string{"/", "/etc/fixture", "/proc", "/sys", "/dev", "/run/docker.sock", "/root",
		"/tmp", "/out/../fixture", "/a,b", "/reports", "/unusual/.aws"} {
		invalid(t, tr.base, with("target", target), "target")
	}
	invalid(t, tr.base, util.M{"backend": "container", "image": testImage,
		"mounts": []any{util.M{"source": "fixtures", "target": "/fixtures"}}}, "access")
}

func TestMountOverlapIsRejected(t *testing.T) {
	tr := newTemp(t)
	source := tr.fixture("fixtures")
	noErr(t, os.Mkdir(filepath.Join(source, "nested"), 0o755))
	tr.fixture("other")
	one := util.M{"source": "fixtures", "target": "/fixtures", "access": "read"}
	for _, other := range []util.M{{"source": "fixtures/nested", "target": "/other", "access": "read"},
		{"source": "other", "target": "/fixtures/nested", "access": "write"}} {
		invalid(t, tr.base, util.M{"backend": "container", "image": testImage, "mounts": []any{one, other}}, "overlap")
	}
}

func TestSymlinkPathsAndNestedSymlinksAreRejectedEvenInsideBase(t *testing.T) {
	tr := newTemp(t)
	source := tr.fixture("fixtures")
	noErr(t, os.Symlink(source, filepath.Join(tr.base, "alias")))
	invalid(t, tr.base, util.M{"backend": "container", "image": testImage,
		"mounts": []any{util.M{"source": "alias", "target": "/fixture", "access": "read"}}}, "symlink")
	noErr(t, os.Symlink(filepath.Join(source, "input.txt"), filepath.Join(source, "alias.txt")))
	invalid(t, tr.base, util.M{"backend": "container", "image": testImage,
		"mounts": []any{util.M{"source": "fixtures", "target": "/fixture", "access": "read"}}}, "symlink")
}

func TestSpecialFilesHardlinksAndConfigurationTreesAreRejected(t *testing.T) {
	tr := newTemp(t)
	source := tr.fixture("fixtures")
	mount := util.M{"backend": "container", "image": testImage,
		"mounts": []any{util.M{"source": "fixtures", "target": "/fixture", "access": "read"}}}
	hardlink := filepath.Join(source, "hardlink")
	noErr(t, os.Link(filepath.Join(source, "input.txt"), hardlink))
	invalid(t, tr.base, mount, "hardlink")
	noErr(t, os.Remove(hardlink))
	fifo := filepath.Join(source, "fifo")
	noErr(t, syscall.Mkfifo(fifo, 0o600))
	invalid(t, tr.base, mount, "special")
	noErr(t, os.Remove(fifo))
	for _, name := range []string{".aws", ".git", ".env", "report.html", "credentials.json"} {
		file := filepath.Join(source, name)
		writeFile(t, file, "synthetic only")
		invalid(t, tr.base, mount, "credential/configuration/report")
		noErr(t, os.Remove(file))
	}
}

func TestSnapshotLimitsAndUnknownToolFields(t *testing.T) {
	tr := newTemp(t)
	source := tr.fixture("fixtures")
	old := maxBytes
	maxBytes = 1
	invalid(t, tr.base, util.M{"backend": "container", "image": testImage,
		"mounts": []any{util.M{"source": source, "target": "/fixture", "access": "read"}}}, "exceeds")
	maxBytes = old
	invalid(t, tr.base, util.M{"backend": "local", "tools": []any{util.M{"name": "x", "argv": []any{"x"}, "required": "yes"}}}, "required")
	invalid(t, tr.base, util.M{"backend": "local", "tools": []any{util.M{"name": "x", "argv": []any{}}}}, "argv")
	invalid(t, tr.base, util.M{"backend": "local", "tools": []any{util.M{"name": "x", "argv": []any{"x"}},
		util.M{"name": "x", "argv": []any{"x"}}}}, "name")
}

func TestCellCannotOverrideEnvironmentOrUseLegacyRunner(t *testing.T) {
	tr := newTemp(t)
	conf := containerProfile(t, tr.base, nil)
	for _, cell := range []util.M{
		{"id": "c", "runner": util.M{"type": "wrapper"}},
		{"id": "c", "env": util.M{"HOME": "/elsewhere"}},
		{"id": "c", "env": map[string]string{"DOCKER_HOST": "tcp://example.invalid"}},
		{"id": "c", "unset_env": []any{"XDG_*"}},
	} {
		wantErr(t, ValidateForCell(conf, cell), KindValue, "cells.c")
	}
	noErr(t, ValidateForCell(conf, util.M{"id": "c", "env": util.M{"EXPLICIT_TOKEN": "synthetic"}}))
}

func TestCellEnvAndUnsetConflictsFailBeforeLaunch(t *testing.T) {
	// The environment part of test_environment_env_conflicts: exact and prefix
	// conflicts are named without values.
	tr := newTemp(t)
	for _, backend := range []string{"local", "container"} {
		var conf util.M
		if backend == "local" {
			out, err := NormalizeProfiles(util.M{"worker": util.M{"backend": "local"}}, tr.base)
			noErr(t, err)
			conf = out["worker"].(util.M)
		} else {
			conf = containerProfile(t, tr.base, nil)
		}
		for _, unset := range []string{"OPENAI_API_KEY", "OPENAI_*"} {
			err := ValidateForCell(conf, util.M{"id": "worker",
				"env":       map[string]string{"OPENAI_API_KEY": "synthetic-replacement"},
				"unset_env": []string{unset}})
			e := wantErr(t, err, KindValue, "both supplied and unset")
			if e.Msg != "cells.worker.env: variables are both supplied and unset: OPENAI_API_KEY; "+
				"remove the overlapping env declarations or narrow auth.unset before running" {
				t.Fatal(e.Msg)
			}
		}
		noErr(t, ValidateForCell(conf, util.M{"id": "worker", "env": map[string]string{"OPENAI_API_KEY": "x"},
			"unset_env": []string{"OTHER_PROVIDER_*"}}))
	}
}

func TestFileSwappedToSymlinkDuringSnapshotCannotEscape(t *testing.T) {
	tr := newTemp(t)
	source := tr.fixture("fixtures")
	outside := filepath.Join(tr.root, "outside.txt")
	writeFile(t, outside, "synthetic excluded content")
	swapped := false
	old := beforeOpen
	beforeOpen = func(name string) {
		if name == "input.txt" && !swapped {
			swapped = true
			os.Remove(filepath.Join(source, "input.txt"))
			os.Symlink(outside, filepath.Join(source, "input.txt"))
		}
	}
	defer func() { beforeOpen = old }()
	_, err := treeManifest(source, "test.fixture", treeOpts{fixtures: true, destination: filepath.Join(tr.root, "snapshot")})
	wantErr(t, err, KindValue, "safely inspect/copy")
	if lexists(filepath.Join(tr.root, "snapshot", "input.txt")) {
		t.Fatal("snapshot copied the swapped file")
	}
}

func TestPythonFormattingHelpers(t *testing.T) {
	for in, want := range map[float64]string{1730000000.1234567: "1730000000.1234567", 1e16: "1e+16", 1.5e-5: "1.5e-05",
		0.01: "0.01", 123.0: "123.0", -0.5: "-0.5", 1e22: "1e+22", 0.0001: "0.0001"} {
		if got := pyFloatRepr(in); got != want {
			t.Fatalf("pyFloatRepr(%v) = %s, want %s", in, got, want)
		}
	}
	if got := pyJSONString("é\x01\U0001F600\xff"); got != `"\u00e9\u0001\ud83d\ude00\udcff"` {
		t.Fatal(got)
	}
	if pyRepr("it's") != `"it's"` || pyRepr("x") != "'x'" {
		t.Fatal(pyRepr("it's"))
	}
	if util.ShellJoin([]string{"a b", "", "c'd", "--x=1"}) != `'a b' '' 'c'"'"'d' --x=1` {
		t.Fatal(util.ShellJoin([]string{"a b", "", "c'd", "--x=1"}))
	}
}

func TestChildEnvRejectsBadUnsetAndIsolatedDrops(t *testing.T) {
	tr := newTemp(t)
	r := localRunner(t, tr.base, nil)
	_, err := r.ChildEnvChecked(nil, []string{"*"})
	wantErr(t, err, KindValue, "trailing-star")
	_, err = r.ChildEnvChecked(map[string]string{"HOME": "/x"}, []string{"HOME"})
	wantErr(t, err, KindValue, "cannot unset the isolated HOME path")
	if env := r.ChildEnv(map[string]string{"LD_PRELOAD": "x"}, nil); len(env) != 0 {
		t.Fatal("ChildEnv must not return a partial environment on error")
	}
	var _ plugin.ManagedRunner = r
}
