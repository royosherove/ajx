package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain keeps the suite offline: identity checks would otherwise run the
// caller's real `aws` (an inherit profile with CLAUDE_CODE_USE_BEDROCK in the
// caller's env does). Shadow it for every test, as the Python suite does.
func TestMain(m *testing.M) {
	guard, err := os.MkdirTemp("", "ajx-test-guard-")
	if err != nil {
		panic(err)
	}
	script := "#!/bin/sh\necho \"$@\" >> \"$(dirname \"$0\")/aws.calls\"\necho \"ajx tests: real aws blocked\" >&2\nexit 97\n"
	if err := os.WriteFile(filepath.Join(guard, "aws"), []byte(script), 0o755); err != nil {
		panic(err)
	}
	os.Setenv("PATH", guard+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, v := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		os.Unsetenv(v)
	}
	code := m.Run()
	os.RemoveAll(guard)
	os.Exit(code)
}
