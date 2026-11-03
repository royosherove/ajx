package environments

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/royosherove/ajx/internal/util"
)

// Python exception class names carried by Error.Kind.
const (
	KindEnvironment       = "EnvironmentError"
	KindValue             = "ValueError"
	KindInspectionChanged = "_InspectionChanged" // a ValueError subclass
)

// Error is a Python exception raised by environments.py. Kind is the Python
// class name: "EnvironmentError" (an environment could not be established or
// safely operated, a RuntimeError), "ValueError" (configuration/validation:
// "field: message"), or "_InspectionChanged" (a ValueError: a starting
// directory changed while being inspected).
type Error struct {
	Kind  string
	Msg   string
	Notes []string // Python exception notes (exc.add_note)
}

func (e *Error) Error() string { return e.Msg }

// TypeName is type(exc).__name__ in Python.
func (e *Error) TypeName() string {
	if e.Kind == "" {
		return KindEnvironment
	}
	return e.Kind
}

// IsValue reports whether Python would raise a ValueError (configuration
// errors spec.load reports as SpecError), rather than an EnvironmentError.
func (e *Error) IsValue() bool { return e.Kind == KindValue || e.Kind == KindInspectionChanged }

// AddNote appends a Python-style exception note.
func (e *Error) AddNote(note string) { e.Notes = append(e.Notes, note) }

// fail is Python _fail: raise ValueError(f"{field}: {message}").
func fail(field, message string) *Error {
	return &Error{Kind: KindValue, Msg: field + ": " + message}
}

// envError is Python raise EnvironmentError(message).
func envError(message string) *Error {
	return &Error{Kind: KindEnvironment, Msg: message}
}

func envErrorf(format string, args ...any) *Error {
	return envError(fmt.Sprintf(format, args...))
}

func changed(field, entry string, attributes []string) *Error {
	return &Error{Kind: KindInspectionChanged, Msg: fmt.Sprintf(
		"%s: starting directory changed while being inspected: %s (%s)", field, entry, strings.Join(attributes, ", "))}
}

// asError returns err as *Error when it is one.
func asError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

func isKind(err error, kinds ...string) bool {
	e, ok := asError(err)
	if !ok {
		return false
	}
	for _, k := range kinds {
		if e.TypeName() == k {
			return true
		}
	}
	return false
}

// ErrorType is type(exc).__name__ for an error raised by this package:
// Error kinds, Python OSError subclasses for operating-system errors,
// "KeyboardInterrupt" for interruption, otherwise the Go type.
func ErrorType(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := asError(err); ok {
		return e.TypeName()
	}
	if util.Interrupted(err) {
		return "KeyboardInterrupt"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "TimeoutExpired"
	}
	if isOSError(err) {
		return osErrorName(err)
	}
	return fmt.Sprintf("%T", err)
}

// isOSError reports whether Python would see err as an OSError.
func isOSError(err error) bool {
	var errno syscall.Errno
	var pe *fs.PathError
	var le *os.LinkError
	var se *os.SyscallError
	var ee *exec.Error
	return errors.As(err, &errno) || errors.As(err, &pe) || errors.As(err, &le) || errors.As(err, &se) ||
		errors.As(err, &ee) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrExist) ||
		errors.Is(err, fs.ErrPermission)
}

// osErrorName maps an operating-system error to the Python OSError subclass name.
func osErrorName(err error) string {
	var ee *exec.Error
	if errors.As(err, &ee) && errors.Is(ee.Err, exec.ErrNotFound) {
		return "FileNotFoundError"
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "FileNotFoundError"
		case errors.Is(err, fs.ErrExist):
			return "FileExistsError"
		case errors.Is(err, fs.ErrPermission):
			return "PermissionError"
		}
		return "OSError"
	}
	switch errno {
	case syscall.EAGAIN, syscall.EALREADY, syscall.EINPROGRESS:
		return "BlockingIOError"
	case syscall.ECHILD:
		return "ChildProcessError"
	case syscall.EPIPE, syscall.ESHUTDOWN:
		return "BrokenPipeError"
	case syscall.ECONNABORTED:
		return "ConnectionAbortedError"
	case syscall.ECONNREFUSED:
		return "ConnectionRefusedError"
	case syscall.ECONNRESET:
		return "ConnectionResetError"
	case syscall.EEXIST:
		return "FileExistsError"
	case syscall.ENOENT:
		return "FileNotFoundError"
	case syscall.EISDIR:
		return "IsADirectoryError"
	case syscall.ENOTDIR:
		return "NotADirectoryError"
	case syscall.EINTR:
		return "InterruptedError"
	case syscall.EACCES, syscall.EPERM:
		return "PermissionError"
	case syscall.ESRCH:
		return "ProcessLookupError"
	case syscall.ETIMEDOUT:
		return "TimeoutError"
	}
	return "OSError"
}

// osErrorText approximates Python str(OSError): "[Errno N] Message: 'path'".
func osErrorText(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	msg := errno.Error()
	if msg != "" {
		msg = strings.ToUpper(msg[:1]) + msg[1:]
	}
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Path != "" {
		return fmt.Sprintf("[Errno %d] %s: %s", int(errno), msg, pyRepr(pe.Path))
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return fmt.Sprintf("[Errno %d] %s: %s -> %s", int(errno), msg, pyRepr(le.Old), pyRepr(le.New))
	}
	return fmt.Sprintf("[Errno %d] %s", int(errno), msg)
}

// pyStrErr is str(exc) for an error caught by `except (EnvironmentError, ValueError, OSError)`.
func pyStrErr(err error) string {
	if e, ok := asError(err); ok {
		return e.Msg
	}
	if isOSError(err) {
		return osErrorText(err)
	}
	return err.Error()
}

// cleanupCatches reports whether Python's `except (EnvironmentError,
// ValueError, OSError)` would catch err (interruption is never caught).
func cleanupCatches(err error) bool {
	if err == nil {
		return false
	}
	if util.Interrupted(err) {
		return false
	}
	if _, ok := asError(err); ok {
		return true
	}
	return isOSError(err)
}
