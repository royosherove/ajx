// Package console ports lib/ajx/console.py: the AJX Console, a local web page
// (and agent CLI view) for reviewing, starting and watching one trial.
//
// The console runs as the user who started it and adds no execution path of
// its own: every action runs an existing `ajx` command (validate, doctor, run,
// report, export) or edits the trial files shown on the page. Its state is the
// trial's own files plus a small journal in <trial-dir>/.ajx-console/, shared
// with the Python console, so either CLI can read the other's state.
package console

import (
	"fmt"
	"time"
)

// Error is a console problem (Python ConsoleError); the CLI exits 2 on it.
// Status is the HTTP status the server answers with (400, or 409 for a conflict).
type Error struct {
	Msg    string
	Status int
}

func (e *Error) Error() string { return e.Msg }

// Errorf builds a console Error (HTTP 400).
func Errorf(format string, a ...any) error {
	return &Error{Msg: fmt.Sprintf(format, a...), Status: 400}
}

// Conflict builds a console Error for a state conflict (Python Conflict, HTTP 409).
func Conflict(format string, a ...any) error {
	return &Error{Msg: fmt.Sprintf(format, a...), Status: 409}
}

// SnapshotOpts are Snapshot's optional arguments.
type SnapshotOpts struct {
	// FeedLimit is how many live worker events each run carries: 0 means the
	// default (14), a negative value none.
	FeedLimit int
	// Skip lists pids the caller reaps itself (the server's own children).
	Skip []int
}

// StartOpts are Start's arguments (Python keyword arguments).
type StartOpts struct {
	Actor, Via, Plan, Quote string
	AcceptWarnings          bool
}

// AgainOpts are RunAgain's arguments; Mode is "same-folder" or "fresh-folder".
type AgainOpts struct {
	Actor, Via, Mode, Plan, Quote string
	AcceptWarnings                bool
}

// ServeOpts configure Serve.
type ServeOpts struct {
	Port        int
	OpenBrowser bool
	Idle        time.Duration // 0 = 30 minutes
	Announce    func(url string)
}

// OpenOpts configure OpenConsole.
type OpenOpts struct {
	Port        int
	OpenBrowser bool
	Timeout     time.Duration // 0 = 15 seconds
}
