package ci

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/royosherove/ajx/internal/runner"
)

type notFreshError struct{ out string }

func (e *notFreshError) Error() string {
	return fmt.Sprintf("%s already holds attempts; pass a fresh --output for a new evaluation, or --allow-reuse to resume them", e.out)
}

// A run directory remains an attempt even when its state file has been lost.
// Read failures must not be mistaken for an empty output folder.
func hasAttempts(out string) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(out, "runs"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return len(entries) > 0, err
}

// claimOutput holds the runner's lock until Session.Run returns, including
// freshness checks, provisional artifacts, the runner, and final artifacts.
func (s *Session) claimOutput(out string) error {
	s.mu.Lock()
	s.paths.out = out
	s.mu.Unlock()
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	lock, err := runner.AcquireMatrixLock(out)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.lock = lock
	s.paths.out = out
	s.mu.Unlock()
	attempts, err := hasAttempts(out)
	if err != nil {
		return err
	}
	if attempts && !s.o.AllowReuse {
		return &notFreshError{out}
	}
	s.mu.Lock()
	if !s.done {
		// Reuse does not authorize replacing a prior result with an input
		// validation refusal. Publish only once preflight accepts this run.
		s.suppressWrites = attempts
	}
	s.mu.Unlock()
	return nil
}

func (s *Session) refuseOutput(err error) int {
	s.mu.Lock()
	s.suppressWrites = true
	s.mu.Unlock()
	code, why := ExitInvalid, "invalid_config"
	var locked *runner.LockedError
	var used *notFreshError
	switch {
	case errors.As(err, &locked):
		code, why = ExitLocked, "locked"
	case errors.As(err, &used):
		why = "output_not_fresh"
	}
	return s.finish(Inconclusive, code, reason(why, SevInconclusive, err.Error(), nil))
}

func (s *Session) checkArtifactPaths() error {
	seen := map[string]string{}
	for _, item := range []struct{ name, path string }{
		{"result", s.paths.result}, {"junit", s.paths.junit},
		{"doctor", filepath.Join(s.paths.out, "ci-doctor.txt")},
	} {
		if item.path == "" {
			continue
		}
		path := filepath.Clean(item.path)
		// Resolve an existing parent so aliases cannot overwrite another artifact.
		if parent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
			path = filepath.Join(parent, filepath.Base(path))
		}
		if previous, ok := seen[path]; ok {
			return fmt.Errorf("%s and %s artifact paths must differ: %s", previous, item.name, path)
		}
		seen[path] = item.name
	}
	return nil
}
