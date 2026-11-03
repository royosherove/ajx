package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/royosherove/ajx/internal/util"
)

// MatrixLock lets a caller protect freshness checks, execution and its own
// result writes with one lock. Acquire it before inspecting or writing output,
// pass it to RunMatrixLocked, and defer Close until all result writes finish.
// Only AcquireMatrixLock returns a valid handle; it cannot be reused after Close.
type MatrixLock struct {
	mu     sync.Mutex
	output string
	path   string
	token  string
	closed bool
	file   *os.File
}

func AcquireMatrixLock(output string) (*MatrixLock, error) {
	output = resolvePath(output)
	if err := os.MkdirAll(output, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(output, ".lock")
	// Keep an OS lock on the same inode until Close. Two coordinators must
	// never both remove a stale lock and accidentally unlink the new owner.
	for attempt := 0; attempt < 5; attempt++ {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			return nil, &LockedError{"another ajx is running this matrix; stop it or wait"}
		}
		pinned, err := f.Stat()
		current, pathErr := os.Stat(path)
		if err != nil || pathErr != nil || !os.SameFile(pinned, current) {
			f.Close()
			continue
		}
		record := util.ReadMap(path)
		if pinned.Size() > 0 {
			if util.S(record["host"]) != hostname() {
				f.Close()
				return nil, &LockedError{fmt.Sprintf("lock held by %s; inspect %s before removing a stale lock", util.S(record["host"]), path)}
			}
			if pid := lockPid(record["pid"]); pid != 0 && PidAlive(pid) {
				f.Close()
				return nil, &LockedError{fmt.Sprintf("another ajx (pid %d) is running this matrix; stop it or wait", pid)}
			}
		}
		l := &MatrixLock{output: output, path: path, token: randHex(), file: f}
		data, err := util.MarshalIndent(M{"pid": int64(os.Getpid()), "host": hostname(), "at": util.Now(), "owner_token": l.token})
		if err == nil {
			err = f.Truncate(0)
		}
		if err == nil {
			_, err = f.Write(data)
		}
		if err == nil {
			err = f.Sync()
		}
		if err != nil {
			f.Close()
			return nil, err
		}
		return l, nil
	}
	return nil, &LockedError{"matrix lock changed while acquiring ownership; retry"}
}

func (l *MatrixLock) validate(output string) error {
	if l.closed || l.file == nil || resolvePath(output) != l.output {
		return fmt.Errorf("matrix lock is closed or belongs to another output directory")
	}
	pinned, err := l.file.Stat()
	current, pathErr := os.Stat(l.path)
	if err != nil || pathErr != nil || !os.SameFile(pinned, current) {
		return &LockedError{"matrix lock was replaced; refusing to execute or remove another owner's lock"}
	}
	record := util.ReadMap(l.path)
	if record["owner_token"] != l.token || lockPid(record["pid"]) != os.Getpid() {
		return &LockedError{"matrix lock ownership changed; refusing to execute or remove another owner's lock"}
	}
	return nil
}

// Close releases only this handle's lock. It refuses to unlink a replacement.
func (l *MatrixLock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	defer func() {
		l.closed = true
		if l.file != nil {
			l.file.Close()
		}
	}()
	if err := l.validate(l.output); err != nil {
		return err
	}
	if err := os.Remove(l.path); err != nil {
		return err
	}
	return nil
}

// ValidateMatrixLocked checks the same saved input contracts and run-state
// integrity as RunMatrixLocked, without writing files, launching processes,
// upgrading fingerprints, or performing recovery. Call it before publishing
// provisional artifacts. Keep the lock until all execution/result writes end.
// sp must be a loaded, normalized spec (as for RunMatrix).
func ValidateMatrixLocked(sp M, o MatrixOpts, l *MatrixLock) error {
	if l == nil {
		return fmt.Errorf("ValidateMatrixLocked requires an acquired matrix lock")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.validate(util.S(util.Get(sp, "trial", "output_dir"))); err != nil {
		return err
	}
	_, err := inspectMatrix(sp, o)
	return err
}

// RunMatrixLocked executes using a caller-owned lock without reacquiring it.
// The handle must still cover the spec's exact output directory. It repeats
// the read-only validation so a caller cannot bypass checks by omitting
// ValidateMatrixLocked.
func RunMatrixLocked(ctx context.Context, sp M, o MatrixOpts, l *MatrixLock) (M, error) {
	if l == nil {
		return nil, fmt.Errorf("RunMatrixLocked requires an acquired matrix lock")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	output := util.S(util.Get(sp, "trial", "output_dir"))
	if err := l.validate(filepath.Clean(output)); err != nil {
		return nil, err
	}
	return runMatrixLocked(ctx, sp, o)
}
