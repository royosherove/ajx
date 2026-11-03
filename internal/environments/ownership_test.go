package environments

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

type inode struct {
	dev int64
	ino uint64
}

type chownRecord struct {
	key      inode
	uid, gid int
}

// ownershipFixture models only root identity and fchown; traversal is real.
type ownershipFixture struct {
	tr      *tempRoot
	owners  map[inode][2]int
	changes []chownRecord
	uid     int
	gid     int
}

func newOwnershipFixture(t *testing.T) *ownershipFixture {
	f := &ownershipFixture{tr: newTemp(t), owners: map[inode][2]int{}}
	oldUID, oldEUID, oldGID, oldFstat, oldChown := getuid, geteuid, getgid, fstat, fchown
	getuid = func() int { return f.uid }
	geteuid = func() int { return 0 }
	getgid = func() int { return f.gid }
	fstat = func(file *os.File) (statInfo, error) {
		info, err := realFstat(file)
		if err != nil {
			return info, err
		}
		owner := f.owners[inode{info.dev, info.ino}]
		info.uid, info.gid = uint32(owner[0]), uint32(owner[1])
		return info, nil
	}
	fchown = func(file *os.File, uid, gid int) error {
		info, err := realFstat(file)
		if err != nil {
			return err
		}
		key := inode{info.dev, info.ino}
		f.changes = append(f.changes, chownRecord{key, uid, gid})
		f.owners[key] = [2]int{uid, gid}
		return nil
	}
	t.Cleanup(func() { getuid, geteuid, getgid, fstat, fchown = oldUID, oldEUID, oldGID, oldFstat, oldChown })
	return f
}

func lidentity(t *testing.T, path string) inode {
	t.Helper()
	fi, err := os.Lstat(path)
	noErr(t, err)
	st, _ := infoStat(fi)
	return inode{st.dev, st.ino}
}

func (f *ownershipFixture) owner(t *testing.T, path string) [2]int {
	return f.owners[lidentity(t, path)]
}

func (f *ownershipFixture) prepared(t *testing.T, name string, opts util.M) (*EnvironmentRunner, *plugin.Ctx, util.M, *fakeDocker) {
	t.Helper()
	engine := newFakeDocker()
	r := engine.runner(t, containerProfile(t, f.tr.base, opts))
	ctx := newCtx(t, f.tr.root, name, true)
	ownAll(ctx)
	handle := persist(t, r, ctx)
	// Inventory is unrelated to ownership; the real preparation/lifecycle code runs.
	r.SetReport(func(util.M, bool) (util.M, error) { return util.M{"status": "prepared"}, nil })
	_, err := r.Prepare(ctx)
	noErr(t, err)
	f.changes = nil
	return r, ctx, handle, engine
}

func workerIDs(handle util.M) [2]int {
	var uid, gid int
	fmt.Sscanf(util.S(handle["worker_user"]), "%d:%d", &uid, &gid)
	return [2]int{uid, gid}
}

func TestPreparationAndRepairCoverOnlyTheFourOwnedRoots(t *testing.T) {
	f := newOwnershipFixture(t)
	r, ctx, handle, engine := f.prepared(t, "attempt", nil)
	worker := workerIDs(handle)
	for _, key := range Roots {
		if f.owner(t, ctxRoot(ctx, key)) != worker {
			t.Fatal(key)
		}
	}
	untouched := []string{ctx.RunDir, util.S(handle["docker_config_dir"]), r.markerPath(ctx, handle)}
	var created []string
	for _, key := range Roots {
		name := "host-setup"
		if key == "workspace" {
			name = ".git"
		}
		dir := filepath.Join(ctxRoot(ctx, key), name)
		noErr(t, os.Mkdir(dir, 0o700))
		output := filepath.Join(dir, "root-created")
		writeFile(t, output, "synthetic host setup\n")
		created = append(created, dir, output)
	}
	for _, p := range created {
		if f.owner(t, p) != [2]int{0, 0} {
			t.Fatal(p)
		}
	}
	original := util.Compact(handle)
	calls := len(engine.calls)
	out, err := r.RestoreWorkerOwnership(ctx)
	noErr(t, err)
	if out != nil {
		t.Fatal(out)
	}
	for _, p := range created {
		if f.owner(t, p) != worker {
			t.Fatal(p)
		}
	}
	for _, p := range untouched {
		if f.owner(t, p) != [2]int{0, 0} {
			t.Fatal(p)
		}
	}
	changed := map[inode]bool{}
	for _, c := range f.changes {
		changed[c.key] = true
	}
	want := map[inode]bool{}
	for _, p := range created {
		want[lidentity(t, p)] = true
	}
	if fmt.Sprint(changed) != fmt.Sprint(want) {
		t.Fatalf("changed %v want %v", changed, want)
	}
	if util.Compact(handle) != original || len(engine.calls) != calls {
		t.Fatal("repair must not provision or replay an attempt")
	}
	f.changes = nil
	_, err = r.RestoreWorkerOwnership(ctx)
	noErr(t, err)
	if len(f.changes) != 0 {
		t.Fatal("already-correct ownership should be left alone")
	}
}

func TestUsesPlannedIdentityAfterCoordinatorIdentityChangesAndRecovery(t *testing.T) {
	f := newOwnershipFixture(t)
	f.uid, f.gid = 23001, 24002
	r, ctx, handle, engine := f.prepared(t, "attempt", nil)
	f.uid, f.gid = 0, 0
	output := filepath.Join(ctx.ConfigDir, "staged-auth.json")
	writeFile(t, output, `{"token": "synthetic-only"}`)
	recovered := engine.runner(t, r.Profile())
	_, err := recovered.RestoreWorkerOwnership(ctx)
	noErr(t, err)
	if handle["worker_user"] != "23001:24002" || f.owner(t, output) != [2]int{23001, 24002} {
		t.Fatal(handle["worker_user"])
	}
	for _, c := range f.changes {
		if c.uid != 23001 || c.gid != 24002 {
			t.Fatal(c)
		}
	}
}

func TestExtraSourcesAndStatePathsDoNotExpandTheOwnershipScope(t *testing.T) {
	f := newOwnershipFixture(t)
	read := f.tr.fixture("read-source")
	write := f.tr.fixture("write-source")
	r, ctx, handle, _ := f.prepared(t, "attempt", util.M{"mounts": []any{
		util.M{"source": read, "target": "/input", "access": "read"},
		util.M{"source": write, "target": "/output", "access": "write"}}})
	extra := filepath.Join(f.tr.root, "unrelated")
	noErr(t, os.Mkdir(extra, 0o755))
	sentinel := filepath.Join(extra, "sentinel")
	writeFile(t, sentinel, "synthetic unrelated data")
	ctx.State["owned_paths"] = append(util.List(ctx.State["owned_paths"]), extra)
	ctx.State["paths"].(util.M)["unrelated"] = extra
	ctx.State["archived"] = util.M{"workspace": extra}
	snapshot := util.S(util.Maps(handle["mounts"])[1]["host_source"])
	output := filepath.Join(snapshot, "host-output")
	writeFile(t, output, "new output in the owned snapshot")
	_, err := r.RestoreWorkerOwnership(ctx)
	noErr(t, err)
	worker := workerIDs(handle)
	if f.owner(t, output) != worker || f.owner(t, filepath.Join(snapshot, "input.txt")) != worker {
		t.Fatal("snapshot not repaired")
	}
	for _, p := range []string{extra, sentinel, read, filepath.Join(read, "input.txt"), write, filepath.Join(write, "input.txt")} {
		if f.owner(t, p) != [2]int{0, 0} {
			t.Fatal(p)
		}
	}
	if readFile(t, filepath.Join(write, "input.txt")) != "synthetic starting data\n" {
		t.Fatal("write source changed")
	}
}

func TestSymlinksHardlinksAndSpecialFilesAreLeftUntouched(t *testing.T) {
	f := newOwnershipFixture(t)
	r, ctx, _, _ := f.prepared(t, "attempt", nil)
	outside := f.tr.fixture("outside")
	target := filepath.Join(outside, "input.txt")
	ordinary := filepath.Join(ctx.Workspace, "ordinary")
	writeFile(t, ordinary, "owned file")
	links := []string{filepath.Join(ctx.Workspace, "external-file"), filepath.Join(ctx.Workspace, "external-dir"),
		filepath.Join(ctx.Workspace, "internal-file"), filepath.Join(ctx.Workspace, "dangling")}
	noErr(t, os.Symlink(target, links[0]))
	noErr(t, os.Symlink(outside, links[1]))
	noErr(t, os.Symlink("ordinary", links[2]))
	noErr(t, os.Symlink("missing", links[3]))
	shared := filepath.Join(ctx.Workspace, "hardlinked")
	noErr(t, os.Link(target, shared))
	fifo := filepath.Join(ctx.Workspace, "fifo")
	noErr(t, syscall.Mkfifo(fifo, 0o600))
	_, err := r.RestoreWorkerOwnership(ctx)
	noErr(t, err)
	if f.owner(t, ordinary) == [2]int{0, 0} {
		t.Fatal("ordinary file not repaired")
	}
	for _, p := range append([]string{outside, target, shared, fifo}, links...) {
		if f.owner(t, p) != [2]int{0, 0} {
			t.Fatal(p)
		}
	}
	for _, l := range links {
		if !isSymlink(l) {
			t.Fatal(l)
		}
	}
}

func TestChangedRootIdentitiesAreRejectedBeforeAnyChown(t *testing.T) {
	f := newOwnershipFixture(t)
	for _, key := range Roots {
		r, ctx, _, _ := f.prepared(t, "replaced-"+key, nil)
		path := ctxRoot(ctx, key)
		noErr(t, os.Rename(path, filepath.Join(filepath.Dir(path), key+"-original")))
		noErr(t, os.Mkdir(path, 0o700))
		writeFile(t, filepath.Join(path, "sentinel"), "replacement directory")
		_, err := r.RestoreWorkerOwnership(ctx)
		wantErr(t, err, KindEnvironment, "identity changed")
		if len(f.changes) != 0 {
			t.Fatal(key)
		}
	}
}

func TestSymlinkedRootOrParentIsRejected(t *testing.T) {
	f := newOwnershipFixture(t)
	r, ctx, _, _ := f.prepared(t, "attempt", nil)
	outside := f.tr.fixture("outside")
	noErr(t, os.Rename(ctx.Workspace, filepath.Join(filepath.Dir(ctx.Workspace), "original")))
	noErr(t, os.Symlink(outside, ctx.Workspace))
	_, err := r.RestoreWorkerOwnership(ctx)
	wantErr(t, err, KindValue, "symlink")
	r, ctx, _, _ = f.prepared(t, "parent-link", nil)
	parent := filepath.Dir(ctx.Workspace)
	noErr(t, os.Rename(parent, parent+"-original"))
	noErr(t, os.Symlink(outside, parent))
	_, err = r.RestoreWorkerOwnership(ctx)
	wantErr(t, err, KindValue, "symlink")
	if len(f.changes) != 0 || f.owner(t, filepath.Join(outside, "input.txt")) != [2]int{0, 0} {
		t.Fatal(f.changes)
	}
}

func TestMissingRootsAndRetargetedContextsCannotBeRepaired(t *testing.T) {
	f := newOwnershipFixture(t)
	r, ctx, _, _ := f.prepared(t, "attempt", nil)
	noErr(t, os.Remove(ctx.Workspace))
	_, err := r.RestoreWorkerOwnership(ctx)
	wantErr(t, err, KindEnvironment, "Cannot safely restore")
	if lexists(ctx.Workspace) || len(f.changes) != 0 {
		t.Fatal("missing root recreated or repaired")
	}
	r, ctx, _, _ = f.prepared(t, "retargeted", nil)
	ctx.Workspace = f.tr.fixture("unrelated")
	_, err = r.RestoreWorkerOwnership(ctx)
	wantErr(t, err, KindEnvironment, "paths changed")
	if len(f.changes) != 0 {
		t.Fatal(f.changes)
	}
}

func TestOwnedPathMetadataCannotWithdrawAnOriginalRoot(t *testing.T) {
	f := newOwnershipFixture(t)
	r, ctx, _, _ := f.prepared(t, "attempt", nil)
	var owned []any
	for _, p := range util.List(ctx.State["owned_paths"]) {
		if p != ctx.HomeDir {
			owned = append(owned, p)
		}
	}
	ctx.State["owned_paths"] = owned
	_, err := r.RestoreWorkerOwnership(ctx)
	wantErr(t, err, KindEnvironment, "no longer recorded as owned")
	if len(f.changes) != 0 {
		t.Fatal(f.changes)
	}
}

func TestUnconfirmedReleasedAndInvalidIdentityAttemptsAreRejected(t *testing.T) {
	f := newOwnershipFixture(t)
	r, ctx, handle, _ := f.prepared(t, "attempt", nil)
	noErr(t, os.Remove(r.markerPath(ctx, handle)))
	_, err := r.RestoreWorkerOwnership(ctx)
	wantErr(t, err, KindEnvironment, "not confirmed")
	r, ctx, _, _ = f.prepared(t, "released", nil)
	r.Release(ctx)
	_, err = r.RestoreWorkerOwnership(ctx)
	wantErr(t, err, KindEnvironment, "released")
	r, ctx, handle, _ = f.prepared(t, "invalid-user", nil)
	for _, user := range []string{"0:1000", "1000:0", "-1:1000", "1000:-1", "root", "1000:1000:1000"} {
		handle["worker_user"] = user
		_, err := r.RestoreWorkerOwnership(ctx)
		wantErr(t, err, KindEnvironment, "worker identity")
	}
	if len(f.changes) != 0 {
		t.Fatal(f.changes)
	}
}

func TestLocalAndEffectivelyNonrootCoordinatorsNoopWithoutAPlan(t *testing.T) {
	f := newOwnershipFixture(t)
	local := mustNew(t, util.M{"backend": "local", "id": "local"})
	container := newFakeDocker().runner(t, containerProfile(t, f.tr.base, nil))
	for _, c := range []struct {
		r    *EnvironmentRunner
		euid int
	}{{local, 0}, {local, 1234}, {container, 1234}} {
		geteuid = func() int { return c.euid }
		out, err := c.r.RestoreWorkerOwnership(&plugin.Ctx{})
		if out != nil || err != nil {
			t.Fatal(out, err)
		}
	}
	if len(f.changes) != 0 {
		t.Fatal(f.changes)
	}
}

func openFDs(t *testing.T) int {
	dir := "/dev/fd"
	if runtime.GOOS == "linux" {
		dir = "/proc/self/fd"
	}
	d, err := os.Open(dir)
	noErr(t, err)
	defer d.Close()
	names, err := d.Readdirnames(-1)
	noErr(t, err)
	return len(names)
}

func TestSymlinkSwapBetweenStatAndOpenCannotEscapeOrLeakDescriptors(t *testing.T) {
	f := newOwnershipFixture(t)
	for _, directory := range []bool{false, true} {
		r, ctx, _, _ := f.prepared(t, fmt.Sprint("swap-", directory), nil)
		outside := f.tr.fixture(fmt.Sprint("outside-", directory))
		target := filepath.Join(outside, "input.txt")
		if directory {
			target = outside
		}
		victim := filepath.Join(ctx.Workspace, "victim")
		if directory {
			noErr(t, os.Mkdir(victim, 0o755))
			writeFile(t, filepath.Join(victim, "owned"), "owned")
		} else {
			writeFile(t, victim, "owned")
		}
		swapped := false
		old := beforeOpen
		beforeOpen = func(name string) {
			if name == "victim" && !swapped {
				swapped = true
				os.Rename(victim, filepath.Join(ctx.Workspace, "original-victim"))
				os.Symlink(target, victim)
			}
		}
		before := openFDs(t)
		_, err := r.RestoreWorkerOwnership(ctx)
		beforeOpen = old
		wantErr(t, err, KindEnvironment, "Cannot safely restore")
		if !swapped || f.owner(t, outside) != [2]int{0, 0} || f.owner(t, filepath.Join(outside, "input.txt")) != [2]int{0, 0} {
			t.Fatal("swap escaped")
		}
		if after := openFDs(t); after > before {
			t.Fatalf("leaked %d descriptors", after-before)
		}
	}
}

func TestRootSwapAfterValidationStillUsesOriginalOpenDirectory(t *testing.T) {
	f := newOwnershipFixture(t)
	r, ctx, _, _ := f.prepared(t, "attempt", nil)
	outside := f.tr.fixture("outside")
	output := filepath.Join(ctx.Workspace, "output")
	writeFile(t, output, "host output")
	original := filepath.Join(filepath.Dir(ctx.Workspace), "original-workspace")
	swapped := false
	old := chownOwnedTree
	chownOwnedTree = func(d *dirHandle, uid, gid int) error {
		if !swapped {
			swapped = true
			os.Rename(ctx.Workspace, original)
			os.Symlink(outside, ctx.Workspace)
		}
		return realChownOwnedTree(d, uid, gid)
	}
	defer func() { chownOwnedTree = old }()
	_, err := r.RestoreWorkerOwnership(ctx)
	noErr(t, err)
	if f.owner(t, filepath.Join(original, "output")) == [2]int{0, 0} || f.owner(t, filepath.Join(outside, "input.txt")) != [2]int{0, 0} {
		t.Fatal("repair followed the swapped root")
	}
}

func TestSameDeviceNestedDirectoryAndFileMountsAreNotChowned(t *testing.T) {
	f := newOwnershipFixture(t)
	r, ctx, _, _ := f.prepared(t, "attempt", nil)
	mounted := filepath.Join(ctx.Workspace, "mounted")
	noErr(t, os.Mkdir(mounted, 0o755))
	sentinel := filepath.Join(mounted, "sentinel")
	writeFile(t, sentinel, "unrelated mounted data")
	fileMount := filepath.Join(ctx.Workspace, "mounted-file")
	writeFile(t, fileMount, "unrelated bind-mounted file")
	ordinary := filepath.Join(ctx.Workspace, "owned")
	writeFile(t, ordinary, "host setup")
	points := map[inode]bool{lidentity(t, mounted): true, lidentity(t, fileMount): true}
	old := ownershipMount
	ownershipMount = func(file *os.File) (mountID, error) {
		value, err := realOwnershipMount(file)
		info, _ := realFstat(file)
		if points[inode{info.dev, info.ino}] {
			value.label = "synthetic-other-mount"
		}
		return value, err
	}
	defer func() { ownershipMount = old }()
	_, err := r.RestoreWorkerOwnership(ctx)
	noErr(t, err)
	if f.owner(t, ordinary) == [2]int{0, 0} {
		t.Fatal("ordinary not repaired")
	}
	for _, p := range []string{mounted, sentinel, fileMount} {
		if f.owner(t, p) != [2]int{0, 0} {
			t.Fatal(p)
		}
	}
}

func TestChownFailureIsReportedAndAllOpenDescriptorsAreClosed(t *testing.T) {
	f := newOwnershipFixture(t)
	r, ctx, _, _ := f.prepared(t, "attempt", nil)
	writeFile(t, filepath.Join(ctx.Workspace, "host-output"), "synthetic")
	fchown = func(*os.File, int, int) error {
		return &os.PathError{Op: "fchown", Path: "x", Err: syscall.EPERM}
	}
	before := openFDs(t)
	_, err := r.RestoreWorkerOwnership(ctx)
	e := wantErr(t, err, KindEnvironment, "Cannot safely restore.*PermissionError")
	if !strings.Contains(e.Msg, "attempt paths must remain accessible without symlinks") {
		t.Fatal(e.Msg)
	}
	if after := openFDs(t); after > before {
		t.Fatalf("leaked %d descriptors", after-before)
	}
}
