package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/royosherove/ajx/internal/util"
)

func TestMatrixLockCoversCallerWritesAndRunWithoutReacquiring(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, root, "", ""))
	out := util.S(util.Get(sp, "trial", "output_dir"))
	l, err := AcquireMatrixLock(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	record := util.ReadMap(filepath.Join(out, ".lock"))
	if record["owner_token"] == nil || record["host"] != hostname() || util.Int(record["pid"], 0) != os.Getpid() {
		t.Fatalf("lock schema %v", record)
	}
	assertLocked := func() {
		t.Helper()
		other, err := AcquireMatrixLock(out)
		if other != nil {
			other.Close()
			t.Fatal("second holder acquired matrix")
		}
		var locked *LockedError
		if !errors.As(err, &locked) {
			t.Fatalf("want LockedError, got %v", err)
		}
	}
	assertLocked()
	if err := os.WriteFile(filepath.Join(out, "caller-before.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RunMatrixLocked(context.Background(), sp, MatrixOpts{
		Stages: []string{"prepare"}, Log: func(string) {},
	}, l); err != nil {
		t.Fatal(err)
	}
	assertLocked()
	if err := os.WriteFile(filepath.Join(out, "caller-after.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
	if _, err := RunMatrixLocked(context.Background(), sp, MatrixOpts{}, l); err == nil {
		t.Fatal("closed handle was accepted")
	}
}

func TestMatrixLockRejectsWrongOutputAndReplacement(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, root, "", ""))
	out := util.S(util.Get(sp, "trial", "output_dir"))
	l, err := AcquireMatrixLock(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	wrong := util.DeepCopy(sp).(M)
	util.Map(wrong["trial"])["output_dir"] = filepath.Join(root, "other")
	if _, err := RunMatrixLocked(context.Background(), wrong, MatrixOpts{}, l); err == nil {
		t.Fatal("handle accepted for wrong output")
	}
	if exists(filepath.Join(root, "other")) {
		t.Fatal("wrong-output refusal created files")
	}
	path := filepath.Join(out, ".lock")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Simulate an operator replacing the file: this handle must not remove it.
	replacement := []byte(`{"pid":7,"host":"other.example","owner_token":"replacement"}`)
	if err := os.WriteFile(path, replacement, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RunMatrixLocked(context.Background(), sp, MatrixOpts{}, l); err == nil {
		t.Fatal("replaced lock accepted")
	}
	if err := l.Close(); err == nil {
		t.Fatal("replacement removal not refused")
	}
	if readFile(t, path) != string(replacement) {
		t.Fatal("another owner's lock was removed")
	}
}

func TestConcurrentStaleMatrixLockAcquisitionHasOneWinner(t *testing.T) {
	out := resolvePath(t.TempDir())
	if err := util.WriteJSON(filepath.Join(out, ".lock"), M{"pid": 1 << 29, "host": hostname(), "at": "past"}); err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	winners := make(chan *MatrixLock, n)
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l, err := AcquireMatrixLock(out)
			if err == nil {
				winners <- l
			} else {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(winners)
	count := 0
	for l := range winners {
		count++
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	}
	if count != 1 {
		t.Fatalf("%d winners; expected exactly one", count)
	}
	if count+len(errs) != n {
		t.Fatal("missing contender result")
	}
}

func snapshotMatrixTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		key, _ := filepath.Rel(root, path)
		record := fmt.Sprintf("%v %v", info.Mode(), info.ModTime())
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			record += " " + string(data)
		}
		snapshot[key] = record
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestValidateMatrixLockedFreshOutputDoesNotWrite(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, root, "", ""))
	out := util.S(util.Get(sp, "trial", "output_dir"))
	l, err := AcquireMatrixLock(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	before := snapshotMatrixTree(t, root)
	if err := ValidateMatrixLocked(sp, MatrixOpts{}, l); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMatrixLocked(sp, MatrixOpts{Stages: []string{"unknown"}}, l); err == nil {
		t.Fatal("invalid stage accepted")
	}
	if err := ValidateMatrixLocked(sp, MatrixOpts{}, nil); err == nil {
		t.Fatal("missing lock accepted")
	}
	wrong := util.DeepCopy(sp).(M)
	util.Map(wrong["trial"])["output_dir"] = filepath.Join(root, "wrong")
	if err := ValidateMatrixLocked(wrong, MatrixOpts{}, l); err == nil {
		t.Fatal("wrong output accepted")
	}
	if after := snapshotMatrixTree(t, root); !reflect.DeepEqual(after, before) {
		t.Fatal("validation changed the filesystem")
	}
	if exists(filepath.Join(out, "runs")) || exists(filepath.Join(out, "matrix-plan.json")) {
		t.Fatal("validation created attempt/provenance artifacts")
	}
}

func TestValidateMatrixLockedSharesRefusalsWithoutTouchingPriorArtifacts(t *testing.T) {
	root := resolvePath(t.TempDir())
	sp := loadSpec(t, writeTrial(t, root, `[[preflight]]
run = "true"
[[setup]]
run = "true"
[[verify]]
name = "made"
type = "file_exists"
path = "made.txt"
[[teardown]]
run = "true"
`, ""))
	if _, err := RunMatrix(context.Background(), sp, MatrixOpts{Stages: []string{"prepare"}, Log: func(string) {}}); err != nil {
		t.Fatal(err)
	}
	out := util.S(util.Get(sp, "trial", "output_dir"))
	l, err := AcquireMatrixLock(out)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := os.WriteFile(filepath.Join(out, "ci-result.json"), []byte(`{"original":"result"}`), 0600); err != nil {
		t.Fatal(err)
	}
	options := MatrixOpts{Log: func(string) {}}
	check := func(candidate M, wantError bool) {
		t.Helper()
		before := snapshotMatrixTree(t, root)
		err := ValidateMatrixLocked(candidate, options, l)
		if (err != nil) != wantError {
			t.Fatalf("validation error %v; want error=%v", err, wantError)
		}
		if wantError {
			_, runErr := RunMatrixLocked(context.Background(), candidate, options, l)
			if runErr == nil || runErr.Error() != err.Error() {
				t.Fatalf("validation/execution disagree: %v / %v", err, runErr)
			}
		}
		if after := snapshotMatrixTree(t, root); !reflect.DeepEqual(after, before) {
			t.Fatal("validation/refusal rewrote original artifacts or created directories")
		}
	}
	check(sp, false)
	for _, section := range []string{"verify", "setup", "preflight", "teardown"} {
		next := util.DeepCopy(sp).(M)
		command := util.Maps(next[section])[0]
		if section == "verify" {
			command["path"] = "missing"
		} else {
			command["run"] = "false"
		}
		check(next, true)
	}
	next := util.DeepCopy(sp).(M)
	util.Maps(next["cells"])[0]["model"] = "changed-model"
	check(next, true)
	next = util.DeepCopy(sp).(M)
	util.Map(next["task"])["prompt_sha256"] = "changed-prompt"
	check(next, true)

	statePath := filepath.Join(out, "runs", "f-r1", "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	check(sp, true)
	for _, invalid := range []string{"null", "[]", "{broken", "{}", `{"stages":null}`,
		`{"stages":{"execute":"running"}}`, `{"stages":{"execute":{"status":"unknown"}}}`,
		`{"stages":{},"execute":{"pid":123}}`} {
		if err := os.WriteFile(statePath, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		check(sp, true)
	}
	if err := util.WriteJSON(statePath, M{"stages": M{}, "environment_contract_sha256": "wrong"}); err != nil {
		t.Fatal(err)
	}
	check(sp, true)
}
