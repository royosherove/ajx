package ajx

import (
	"strings"
	"testing"
)

func TestInstallerUsesCanonicalLogo(t *testing.T) {
	const begin = "    # BEGIN GENERATED LOGO (skills/ajx/assets/logo.txt)\n"
	const end = "    # END GENERATED LOGO\n"
	if strings.Count(Installer, begin) != 1 || strings.Count(Installer, end) != 1 {
		t.Fatal("installer must contain exactly one generated logo block")
	}
	_, block, _ := strings.Cut(Installer, begin)
	block, _, _ = strings.Cut(block, end)
	want := "    cat <<'AJX_LOGO'\n" + Logo + "AJX_LOGO\n"
	if block != want {
		t.Fatal("installer logo differs from canonical asset; run go generate ./")
	}
}
