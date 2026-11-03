package ci

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/royosherove/ajx/internal/util"
)

// writeArtifact publishes a complete, synced file. A unique temporary filename
// avoids colliding with another invocation's temporary data. When the output
// folder is unknown (early invalid input), exclusive publication preserves any
// existing standalone result supplied by --result or --junit.
func writeArtifact(path string, data []byte, replace bool) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("cannot write %s: %w", path, err)
		}
	}()
	parent := filepath.Dir(path)
	if err = os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(parent, ".ajx-ci-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0o644); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if replace {
		err = os.Rename(f.Name(), path)
	} else {
		err = os.Link(f.Name(), path)
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Session) artifactErrorLocked(err error, code int) {
	s.o.Say("ci: " + err.Error())
	s.result["decision"] = Inconclusive
	// Keep a more specific usage or cancellation exit when reporting it fails.
	switch util.Int(s.result["exit_code"], code) {
	case ExitInvalid, ExitLocked, ExitInterrupted:
	default:
		s.result["exit_code"] = code
	}
	s.result["reasons"] = append(util.List(s.result["reasons"]),
		reason("artifact_write_error", SevInconclusive, err.Error(), nil))
	s.result["finished_at"] = util.Now()
}

// writeLocked writes JUnit first and JSON last. The JSON decision is the commit
// record: it cannot say passed after a known JUnit write failure. A JSON write
// failure also updates any writable JUnit file to report the infrastructure
// error. The caller holds s.mu.
func (s *Session) writeLocked(failureCode int) error {
	if s.suppressWrites {
		return nil
	}
	var errs []error
	art := util.MapOr(s.result["artifacts"])
	s.result["artifacts"] = art
	junitWritten := false
	if s.paths.junit != "" {
		err := writeArtifact(s.paths.junit, []byte(JUnit(s.result)), s.lock != nil || s.written.junit != "")
		if err != nil {
			s.written.junit = ""
			art["junit"] = nil
			s.artifactErrorLocked(err, failureCode)
			errs = append(errs, err)
		} else {
			junitWritten = true
			s.written.junit = s.paths.junit
			art["junit"] = s.paths.junit
		}
	}
	if s.paths.result != "" {
		art["result"] = s.paths.result
		// Standard encoding rejects non-finite values instead of emitting NaN.
		data, err := json.MarshalIndent(s.result, "", "  ")
		if err == nil {
			err = writeArtifact(s.paths.result, append(data, '\n'), s.lock != nil || s.written.result != "")
		}
		if err != nil {
			s.written.result = ""
			art["result"] = nil
			s.artifactErrorLocked(err, failureCode)
			errs = append(errs, err)
			if junitWritten {
				if e := writeArtifact(s.paths.junit, []byte(JUnit(s.result)), true); e != nil {
					s.written.junit = ""
					art["junit"] = nil
					errs = append(errs, e)
					s.artifactErrorLocked(e, failureCode)
				}
			}
		} else {
			s.written.result = s.paths.result
		}
	}
	return errors.Join(errs...)
}
