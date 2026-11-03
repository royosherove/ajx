package runner

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/util"
)

func TestCleanupOnly(t *testing.T) {
	cases := map[string]bool{"": false, "teardown": true, "teardown,render": true, "release,render,archive": true,
		"teardown,execute": false, "narrate": false}
	for in, want := range cases {
		var stages []string
		if in != "" {
			stages = strings.Split(in, ",")
		}
		if got := CleanupOnly(stages); got != want {
			t.Errorf("CleanupOnly(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLockBlocksSecondRunnerAndToleratesStaleLock(t *testing.T) {
	tmp := t.TempDir()
	lock, err := AcquireLock(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(tmp); err == nil {
		t.Fatal("second lock acquired")
	} else if _, ok := err.(*LockedError); !ok || !strings.Contains(err.Error(), "another ajx (pid ") {
		t.Fatalf("want LockedError, got %T %v", err, err)
	}
	ReleaseLock(lock)
	host, _ := os.Hostname()
	if err := util.WriteJSON(filepath.Join(tmp, ".lock"), M{"pid": 1<<22 + 12345, "host": host, "at": "x"}); err != nil {
		t.Fatal(err)
	}
	lock, err = AcquireLock(tmp) // dead pid: stale lock is replaced
	if err != nil {
		t.Fatal(err)
	}
	info := util.ReadMap(lock)
	if util.Int(info["pid"], 0) != os.Getpid() || info["host"] != host {
		t.Fatalf("lock content %v", info)
	}
	ReleaseLock(lock)
	if exists(lock) {
		t.Fatal("lock not released")
	}
	if err := util.WriteJSON(filepath.Join(tmp, ".lock"), M{"pid": 7, "host": "elsewhere.example", "at": "x"}); err != nil {
		t.Fatal(err)
	}
	_, err = AcquireLock(tmp)
	if err == nil || !strings.Contains(err.Error(), "lock held by elsewhere.example (pid 7); remove ") {
		t.Fatalf("other host: %v", err)
	}
}

func TestCellsContractAndChanges(t *testing.T) {
	sp := M{
		"cells":    []any{M{"id": "a", "harness": "fake", "model": "m1", "env": M{"B": "1", "A": "2"}, "args": []any{"--x"}}},
		"reporter": M{"harness": "codex", "model": nil, "effort": nil, "auth": "inherit", "extra": 1},
		"trial":    M{"timeout_seconds": int64(600), "narrate_timeout_seconds": int64(900), "max_budget_usd": nil, "cache": "isolated", "shell_startup": "inherit", "id": "t"},
	}
	c := CellsContract(sp)
	cell := util.MapOr(util.Get(c, "cells", "a"))
	if got := util.Strings(cell["env"]); strings.Join(got, ",") != "A,B" {
		t.Fatalf("env names %v", got)
	}
	if _, ok := cell["shell_startup"]; !ok {
		t.Fatal("missing shell_startup key")
	}
	if _, ok := util.MapOr(c["reporter"])["extra"]; ok {
		t.Fatal("reporter contract has extra keys")
	}
	// Round trip through JSON: numbers become float64 / int64 and must still compare equal.
	data, _ := util.MarshalIndent(c)
	back, err := DecodeJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if ch := ContractChanges(back.(M), c); len(ch) != 0 {
		t.Fatalf("unexpected changes %v", ch)
	}
	plain, _ := util.DecodeJSON(data) // float64 numbers
	if ch := ContractChanges(plain.(M), c); len(ch) != 0 {
		t.Fatalf("unexpected changes after float decode %v", ch)
	}
	sp2 := util.DeepCopy(sp).(M)
	util.Maps(sp2["cells"])[0]["model"] = "m2"
	util.MapOr(sp2["trial"])["timeout_seconds"] = int64(30)
	util.Maps(sp2["cells"])[0]["env"] = M{"A": "x"}
	sp2["cells"] = append(sp2["cells"].([]any), M{"id": "new", "harness": "fake"}) // added cells are fine
	got := ContractChanges(c, CellsContract(sp2))
	want := []string{"cell a model: 'm1' -> 'm2'", "cell a env: ['A', 'B'] -> ['A']", "limits timeout_seconds: 600 -> 30"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("changes\n got %q\nwant %q", got, want)
	}
}

func TestPythonFormatting(t *testing.T) {
	if got := PyRepr(M{"a": []any{int64(1), 2.5, nil, true, "it's"}}); got != `{'a': [1, 2.5, None, True, "it's"]}` {
		t.Fatal(got)
	}
	if got := pyTuple([]string{"prepare"}); got != "('prepare',)" {
		t.Fatal(got)
	}
	if got := pyList([]string{"rendr"}); got != "['rendr']" {
		t.Fatal(got)
	}
	// json.dumps(sort_keys=True) default: ", " / ": " separators and ASCII escapes.
	if got := PyDumps(M{"b": "é😀", "a": int64(1), "c": 1.0, "d": nil}); got != `{"a": 1, "b": "\u00e9\ud83d\ude00", "c": 1.0, "d": null}` {
		t.Fatal(got)
	}
	if got := CanonicalJSON(M{"n": 3600.0}, true); got != `{"n": 3600}` {
		t.Fatal(got)
	}
	v, err := DecodeJSON([]byte(`{"i": 3, "f": 3.0, "e": 1e3}`))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(M)
	if _, ok := m["i"].(int64); !ok {
		t.Fatalf("int literal decoded as %T", m["i"])
	}
	if _, ok := m["f"].(float64); !ok {
		t.Fatalf("float literal decoded as %T", m["f"])
	}
	if shellQuote("python3") != "python3" || shellQuote("my sb") != "'my sb'" || shellQuote("") != "''" ||
		shellQuote("a'b") != `'a'"'"'b'` {
		t.Fatal("shellQuote")
	}
}

func TestProfileContractComparesNumbersByValue(t *testing.T) {
	toml := M{"schema_version": int64(1), "environments": M{"c": M{"backend": "container", "lifetime_seconds": int64(3600)}},
		"agent_configurations": M{}}
	data, _ := util.MarshalIndent(toml)
	back, _ := util.DecodeJSON(data)
	if ProfileContract(toml) != ProfileContract(back.(M)) {
		t.Fatalf("contracts differ:\n%s\n%s", ProfileContract(toml), ProfileContract(back.(M)))
	}
	changed := util.DeepCopy(toml).(M)
	util.MapOr(util.Get(changed, "environments", "c"))["lifetime_seconds"] = int64(60)
	if ProfileContract(toml) == ProfileContract(changed) {
		t.Fatal("changed profile compares equal")
	}
}

func TestPurgeRemovesTokenDotfilesAndKeepsOtherFiles(t *testing.T) {
	for _, name := range []string{".npmrc", ".netrc", ".pypirc", ".git-credentials"} {
		if !util.Contains(CredentialFiles, name) {
			t.Fatalf("%s not a credential file", name)
		}
	}
	tmp := resolvePath(t.TempDir())
	home := filepath.Join(tmp, "home")
	nested := filepath.Join(home, "project", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".npmrc"), "//registry.example.test/:_authToken=synthetic-registry-token\n")
	write(filepath.Join(nested, ".npmrc"), "registry=https://registry.example.test/\n")
	write(filepath.Join(home, ".netrc"), "machine example.test login user password synthetic\n")
	write(filepath.Join(nested, ".pypirc"), "[pypi]\npassword = synthetic\n")
	write(filepath.Join(home, ".git-credentials"), "https://user:synthetic@example.com\n")
	outside := filepath.Join(tmp, "outside-target.txt")
	write(outside, "link target stays")
	if err := os.Symlink(outside, filepath.Join(nested, ".netrc")); err != nil {
		t.Fatal(err)
	}
	// A symlinked directory is never followed, even when named like a credential.
	outsideDir := filepath.Join(tmp, "outside-dir")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(outsideDir, "auth.json"), "outside secret stays")
	if err := os.Symlink(outsideDir, filepath.Join(home, "linked")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(home, "notes.txt"), "keep")
	write(filepath.Join(home, "project", "package.json"), "{}")
	write(filepath.Join(home, "project", "npmrc"), "not a dotfile")
	write(filepath.Join(home, "project", ".npmrc.bak"), "different name")
	dir, err := openDirectoryNofollow(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := purgeCredentialTree(dir.root); err != nil {
		t.Fatal(err)
	}
	dir.Close()
	var remaining []string
	_ = filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(home, p)
			remaining = append(remaining, rel)
		}
		return nil
	})
	sort.Strings(remaining)
	want := "linked|notes.txt|project/.npmrc.bak|project/npmrc|project/package.json"
	if strings.Join(remaining, "|") != want {
		t.Fatalf("remaining %v", remaining)
	}
	if data, _ := os.ReadFile(outside); string(data) != "link target stays" {
		t.Fatal("symlink target changed")
	}
	if data, _ := os.ReadFile(filepath.Join(outsideDir, "auth.json")); string(data) != "outside secret stays" {
		t.Fatal("followed a symlinked directory")
	}
}

func TestOpenDirectoryNofollowRefusesSymlinkedRoots(t *testing.T) {
	tmp := resolvePath(t.TempDir())
	real := filepath.Join(tmp, "real")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(tmp, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := openDirectoryNofollow(filepath.Join(tmp, "link")); err == nil || pyTypeName(err) != "OSError" {
		t.Fatalf("symlinked root: %v (%s)", err, pyTypeName(err))
	}
	if _, err := openDirectoryNofollow(filepath.Join(tmp, "link", "sub")); err == nil {
		t.Fatal("symlinked ancestor accepted")
	}
	if _, err := openDirectoryNofollow("relative/path"); err == nil || !strings.Contains(err.Error(), "absolute owned paths") {
		t.Fatalf("relative: %v", err)
	}
	if _, err := openDirectoryNofollow(filepath.Join(tmp, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing: %v", err)
	}
}
