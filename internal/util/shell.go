package util

import "strings"

// ShellQuote quotes one argument for a POSIX shell, matching shlex.quote.
// It leaves only the ASCII shell-safe character set unquoted.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("@%+=:,./-_", c)) {
			return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
		}
	}
	return s
}

// ShellJoin quotes an argument vector for a POSIX shell, matching shlex.join.
// Prefer passing argv directly when a shell is not required.
func ShellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, arg := range argv {
		out[i] = ShellQuote(arg)
	}
	return strings.Join(out, " ")
}
