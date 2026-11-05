package util

import (
	"bytes"
	"os/exec"
	"testing"
)

var shellQuoteCases = []struct{ arg, quoted string }{
	{"", "''"},
	{"abc_012-DEF", "abc_012-DEF"},
	{"@%+=:,./", "@%+=:,./"},
	{"two words", "'two words'"},
	{"c'd", `'c'"'"'d'`},
	{`a"b`, `'a"b'`},
	{`a\b`, `'a\b'`},
	{"a\nb\tc", "'a\nb\tc'"},
	{"café", "'café'"},
	{"工具", "'工具'"},
	{"~", "'~'"},
	{"$AJX_SHELL_TEST", "'$AJX_SHELL_TEST'"},
	{"$(printf expanded)", "'$(printf expanded)'"},
	{"`printf expanded`", "'`printf expanded`'"},
	{"*?[x]; | & > ( )", "'*?[x]; | & > ( )'"},
}

func TestShellQuotePreservesPOSIXSpelling(t *testing.T) {
	for _, tc := range shellQuoteCases {
		if got := ShellQuote(tc.arg); got != tc.quoted {
			t.Errorf("ShellQuote(%q) = %q, want %q", tc.arg, got, tc.quoted)
		}
	}
	if got := ShellJoin(nil); got != "" {
		t.Errorf("empty argv became %q", got)
	}
}

func TestShellJoinRoundTripsArgumentsWithoutExpansion(t *testing.T) {
	args := make([]string, len(shellQuoteCases))
	var want bytes.Buffer
	for i, tc := range shellQuoteCases {
		args[i] = tc.arg
		want.WriteString(tc.arg)
		want.WriteByte(0)
	}
	cmd := exec.Command("/bin/sh", "-c", `set -- `+ShellJoin(args)+`; printf '%s\000' "$@"`)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "AJX_SHELL_TEST=expanded"}
	got, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("shell changed arguments:\n got %q\nwant %q", got, want.Bytes())
	}
}
