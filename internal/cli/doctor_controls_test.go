package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/util"
)

func TestDoctorExposesPreparedControlsWithoutRuntimeClaims(t *testing.T) {
	offline(t)
	path := headlessDoctorFixture(t, `['/bin/echo','synthetic-identity']`, "#!/bin/sh\necho synthetic-v1\n", "", "")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "[trial]\n", "[trial]\nshell_startup='skip'\n", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, diagnostic := run(t, "doctor", path, "--json")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, diagnostic)
	}
	cell := util.Maps(decode(t, out)["cells"])[0]
	config, shell := util.MapOr(cell["configuration"]), util.MapOr(cell["shell_startup"])
	if config["requested_mode"] != "clean" || config["auth_allows_isolation"] != true {
		t.Fatalf("missing requested configuration facts: %v", config)
	}
	if shell["evidence_scope"] != "doctor_probe" || shell["startup_files_read"] != nil ||
		util.Compact(shell["configured_env_names"]) != `["ZDOTDIR"]` {
		t.Fatalf("doctor lost prepared controls or invented runtime reads: %v", shell)
	}
	code, text, diagnostic := run(t, "doctor", path)
	if code != 0 || !strings.Contains(text, "configuration:") || !strings.Contains(text, "shell startup:") {
		t.Fatalf("human output must explain the controls: %d %s %s", code, text, diagnostic)
	}
}
