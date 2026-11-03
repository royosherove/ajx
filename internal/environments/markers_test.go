package environments

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

type markerFixture struct {
	root    string
	ctx     *plugin.Ctx
	profile util.M
	runner  *EnvironmentRunner
	handle  util.M
	marker  string
}

func newMarkerFixture(t *testing.T) *markerFixture {
	tr := newTemp(t)
	f := &markerFixture{root: tr.root}
	f.ctx = newCtx(t, tr.root, "attempt", true)
	out, err := NormalizeProfiles(util.M{"local": util.M{"backend": "local"}}, tr.root)
	noErr(t, err)
	f.profile = out["local"].(util.M)
	f.runner = mustNew(t, f.profile)
	f.handle = persist(t, f.runner, f.ctx)
	f.marker = f.runner.markerPath(f.ctx, f.handle)
	return f
}

func (f *markerFixture) recovered(t *testing.T) *EnvironmentRunner { return mustNew(t, f.profile) }

func (f *markerFixture) leftovers(t *testing.T) []string {
	matches, err := filepath.Glob(f.marker + ".*.tmp")
	noErr(t, err)
	return matches
}

func (f *markerFixture) status(t *testing.T) any {
	m, err := f.runner.readMarker(f.ctx, f.handle)
	noErr(t, err)
	return m["status"]
}

var errSyntheticInterrupt = errors.New("synthetic interruption during lifecycle write")

func interruptMarkerWrites(t *testing.T) {
	old := writeMarkerData
	writeMarkerData = func(w io.Writer, data []byte) error {
		io.WriteString(w, `{"owner":`)
		return errSyntheticInterrupt
	}
	t.Cleanup(func() { writeMarkerData = old })
}

func withSuffix(path, suffix string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + suffix
}

func TestLegacyStaleTempDoesNotBlockPreparationOrRelease(t *testing.T) {
	f := newMarkerFixture(t)
	stale := withSuffix(f.marker, ".tmp")
	writeFile(t, stale, `{"owner":`)
	_, err := f.runner.Prepare(f.ctx)
	noErr(t, err)
	if f.status(t) != "prepared" {
		t.Fatal(f.status(t))
	}
	if released, _ := f.recovered(t).Release(f.ctx); released["confirmed"] != true {
		t.Fatal(released)
	}
	if f.status(t) != "released" || readFile(t, stale) != `{"owner":` {
		t.Fatal("stale temp changed")
	}
}

func TestStaleTempSymlinkIsNotFollowedOrDeleted(t *testing.T) {
	f := newMarkerFixture(t)
	outside := filepath.Join(f.root, "untouched")
	writeFile(t, outside, "synthetic outside content")
	stale := withSuffix(f.marker, ".tmp")
	noErr(t, os.Symlink(outside, stale))
	_, err := f.runner.Prepare(f.ctx)
	noErr(t, err)
	if released, _ := f.recovered(t).Release(f.ctx); released["confirmed"] != true {
		t.Fatal(released)
	}
	if !isSymlink(stale) || readFile(t, outside) != "synthetic outside content" {
		t.Fatal("stale symlink followed")
	}
}

func TestHandledInterruptionPreservesMarkerAndAllowsReleaseRetry(t *testing.T) {
	f := newMarkerFixture(t)
	noErr(t, f.runner.writeMarker(f.ctx, f.handle, "preparing", true))
	original := readFile(t, f.marker)
	for _, status := range []string{"prepared", "released"} {
		func() {
			old := writeMarkerData
			defer func() { writeMarkerData = old }()
			interruptMarkerWrites(t)
			err := f.runner.writeMarker(f.ctx, f.handle, status, false)
			if !errors.Is(err, errSyntheticInterrupt) {
				t.Fatal(err)
			}
		}()
		if readFile(t, f.marker) != original || len(f.leftovers(t)) != 0 {
			t.Fatal(status)
		}
	}
	if released, _ := f.recovered(t).Release(f.ctx); released["confirmed"] != true {
		t.Fatal(released)
	}
	_, err := f.recovered(t).Prepare(f.ctx)
	wantErr(t, err, KindEnvironment, "not confirmed or has been released")
}

func TestInterruptedInitialWriteDoesNotPublishAPartialMarker(t *testing.T) {
	f := newMarkerFixture(t)
	func() {
		old := writeMarkerData
		defer func() { writeMarkerData = old }()
		interruptMarkerWrites(t)
		if err := f.runner.writeMarker(f.ctx, f.handle, "preparing", true); !errors.Is(err, errSyntheticInterrupt) {
			t.Fatal(err)
		}
	}()
	if lexists(f.marker) || len(f.leftovers(t)) != 0 {
		t.Fatal("partial marker published")
	}
	if released, _ := f.recovered(t).Release(f.ctx); released["confirmed"] != true {
		t.Fatal(released)
	}
	_, err := f.recovered(t).Prepare(f.ctx)
	wantErr(t, err, KindEnvironment, "preparation was not confirmed")
}

func TestInitialPublicationDoesNotReplaceAnExistingMarker(t *testing.T) {
	f := newMarkerFixture(t)
	noErr(t, f.runner.writeMarker(f.ctx, f.handle, "preparing", true))
	original := readFile(t, f.marker)
	err := f.runner.writeMarker(f.ctx, f.handle, "released", true)
	wantErr(t, err, KindEnvironment, "already attempted")
	st, err := pathStat(f.marker)
	noErr(t, err)
	if readFile(t, f.marker) != original || st.mode&0o777 != 0o600 || st.nlink != 1 || len(f.leftovers(t)) != 0 {
		t.Fatalf("marker changed: mode %o nlink %d", st.mode&0o777, st.nlink)
	}
}

// markerCrashEnv runs one synthetic crash in a child process (TestMain).
const markerCrashEnv = "AJX_ENV_TEST_MARKER_CRASH"

func markerCrashChild() {
	data, err := util.DecodeJSON([]byte(os.Getenv(markerCrashEnv)))
	if err != nil {
		os.Exit(2)
	}
	args := data.(util.M)
	r, err := New(util.MapOr(args["profile"]))
	if err != nil {
		os.Exit(3)
	}
	ctx := &plugin.Ctx{RunDir: util.S(args["run_dir"]), State: util.M{"environment": args["handle"]}}
	// Exit immediately before atomic publication: deferred cleanup cannot run.
	replaceFile = func(string, string) error { os.Exit(73); return nil }
	r.writeMarker(ctx, util.MapOr(args["handle"]), "released", false)
	os.Exit(4)
}

func TestMain(m *testing.M) {
	if os.Getenv(markerCrashEnv) != "" {
		markerCrashChild()
	}
	os.Exit(m.Run())
}

func TestAbruptExitLeavesAnOrphanThatCannotBlockCleanupRetry(t *testing.T) {
	f := newMarkerFixture(t)
	noErr(t, f.runner.writeMarker(f.ctx, f.handle, "preparing", true))
	original := readFile(t, f.marker)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), markerCrashEnv+"="+util.Compact(util.M{"profile": f.profile,
		"run_dir": f.ctx.RunDir, "handle": f.handle}))
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 73 {
		t.Fatalf("crash child: %v", err)
	}
	if readFile(t, f.marker) != original {
		t.Fatal("authoritative marker changed")
	}
	leftovers := f.leftovers(t)
	if len(leftovers) != 1 {
		t.Fatal(leftovers)
	}
	st, err := pathStat(leftovers[0])
	noErr(t, err)
	if st.mode&0o777 != 0o600 {
		t.Fatalf("orphan mode %o", st.mode&0o777)
	}
	if released, _ := f.recovered(t).Release(f.ctx); released["confirmed"] != true {
		t.Fatal(released)
	}
	if f.status(t) != "released" || !lexists(leftovers[0]) {
		t.Fatal("cleanup retry blocked or removed the orphan")
	}
}

func TestMarkerSymlinkAndForeignOwnershipAreRejected(t *testing.T) {
	f := newMarkerFixture(t)
	other := filepath.Join(f.root, "other.json")
	writeFile(t, other, `{"owner": "x", "profile_sha256": "y", "status": "prepared"}`)
	noErr(t, os.Symlink(other, f.marker))
	_, err := f.runner.readMarker(f.ctx, f.handle)
	wantErr(t, err, KindEnvironment, "must not be a symlink")
	noErr(t, os.Remove(f.marker))
	writeFile(t, f.marker, `{"owner": "x", "profile_sha256": "y", "status": "prepared"}`)
	_, err = f.runner.readMarker(f.ctx, f.handle)
	wantErr(t, err, KindEnvironment, "ownership differs")
}
