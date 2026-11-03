package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"github.com/royosherove/ajx/internal/spec"
	"github.com/royosherove/ajx/internal/util"
)

// StageRefusalError means an explicit request would replay an existing task.
// It is distinct from an idempotent resume, which may have nothing left to do.
type StageRefusalError struct {
	RunID  string
	Stages []string
}

func (e *StageRefusalError) Error() string {
	return fmt.Sprintf("%s: refusing to redo %s: a task runs once per repetition; use a new repetition or output directory",
		e.RunID, pyList(e.Stages))
}

func (e *StageRefusalError) TypeName() string { return "StageRefusalError" }

// ProcessRecoveryError leaves an attempt intact when its worker cannot be
// confirmed stopped. Retrying must not verify or archive that workspace.
type ProcessRecoveryError struct {
	RunID  string
	Reason string
}

func (e *ProcessRecoveryError) Error() string {
	return fmt.Sprintf("%s: worker cleanup is not confirmed: %s; preserving the original workspace and evidence",
		e.RunID, e.Reason)
}

func (e *ProcessRecoveryError) TypeName() string { return "ProcessRecoveryError" }

// pyError is an error carrying the Python exception class name it ports.
type pyError struct {
	kind string
	msg  string
}

func (e *pyError) Error() string    { return e.msg }
func (e *pyError) TypeName() string { return e.kind }

// runtimeErr is Python's RuntimeError(msg).
func runtimeErr(format string, a ...any) error {
	return &pyError{"RuntimeError", fmt.Sprintf(format, a...)}
}

// valueErr is Python's ValueError(msg).
func valueErr(format string, a ...any) error {
	return &pyError{"ValueError", fmt.Sprintf(format, a...)}
}

// interruptError is Python's KeyboardInterrupt; it wraps util.ErrInterrupted.
type interruptError struct {
	signal string // "" for SIGINT (KeyboardInterrupt()), else "signal N"
	notes  []string
}

func (e *interruptError) Error() string {
	if e.signal == "" {
		return "interrupted"
	}
	return "interrupted (" + e.signal + ")"
}
func (e *interruptError) Unwrap() error     { return util.ErrInterrupted }
func (e *interruptError) TypeName() string  { return "KeyboardInterrupt" }
func (e *interruptError) PyMessage() string { return e.signal }

// pyTypeName is type(exc).__name__ for an error.
func pyTypeName(err error) string {
	var tn interface{ TypeName() string }
	if errors.As(err, &tn) {
		return tn.TypeName()
	}
	var se *spec.Error
	if errors.As(err, &se) {
		return "SpecError"
	}
	if util.Interrupted(err) {
		return "KeyboardInterrupt"
	}
	var le *LockedError
	if errors.As(err, &le) {
		return "LockedError"
	}
	var pe *fs.PathError
	var le2 *os.LinkError
	if errors.As(err, &pe) || errors.As(err, &le2) {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "FileNotFoundError"
		case errors.Is(err, fs.ErrExist):
			return "FileExistsError"
		case errors.Is(err, fs.ErrPermission):
			return "PermissionError"
		case errors.Is(err, syscall.ENOTDIR):
			return "NotADirectoryError"
		case errors.Is(err, syscall.EISDIR):
			return "IsADirectoryError"
		}
		return "OSError"
	}
	return "RuntimeError"
}

// pyMessage is str(exc).
func pyMessage(err error) string {
	var pm interface{ PyMessage() string }
	if errors.As(err, &pm) {
		return pm.PyMessage()
	}
	if util.Interrupted(err) {
		return ""
	}
	return err.Error()
}

// pyErrString is f"{type(exc).__name__}: {exc}".
func pyErrString(err error) string { return pyTypeName(err) + ": " + pyMessage(err) }
