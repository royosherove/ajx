package evidence

import (
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

func TestAJXProductUseDoesNotHideOuterTrialAccess(t *testing.T) {
	sp := M{"trial": M{"product": "AJX", "output_dir": "/outer/results", "workspace_root": "/work"},
		"path": "/outer/trial.toml"}
	tel := AssignIDs(M{"events": []any{
		tool("a", "Bash", M{"command": "ajx --help"}, "", ""),
		tool("b", "Read", M{"file_path": "/outer/trial.toml"}, "", ""),
		tool("c", "Read", M{"file_path": "/opt/.claude/skills/ajx/SKILL.md"}, "", ""),
	}})
	flags := IsolationScan(tel, sp, M{"paths": M{"workspace": "/work/0123456789"}})
	if len(flags) != 2 {
		t.Fatalf("expected only concrete outside-material flags, got %v", flags)
	}
	sp["trial"].(M)["product"] = "another-cli"
	if got := IsolationScan(tel, sp, M{}); len(got) != 3 {
		t.Fatalf("non-AJX trials retain mention warning: %v", got)
	}
}

func TestDigestSuppliesCompleteInputBeyondPreview(t *testing.T) {
	command := "jq '" + strings.Repeat(".items | ", 40) + "sort_by(.total)' orders.json > summary.json"
	ev := tool("a", "Bash", M{"command": command}, "", "")
	tel := AssignIDs(plugin.EmptyTelemetry(M{"events": []any{ev}}))
	digest := BuildDigest(M{"trial": M{}}, M{}, M{}, tel, M{}, M{}, DigestOpts{})
	if !strings.Contains(digest, command) || !strings.Contains(digest, "input_raw (complete)") {
		t.Fatal("reporter must receive the full input, including the output redirect")
	}
}

func TestFailureMetricsDiscloseTheirScope(t *testing.T) {
	ok := tool("a", "Bash", M{"command": "broken-command; true"}, "", "")
	plugin.FinishTool(ok, "", false, "error: first command failed")
	unknown := tool("b", "Bash", M{"command": "still running"}, "", "")
	tel := AssignIDs(plugin.EmptyTelemetry(M{"events": []any{ok, unknown}, "tool_status": "complete"}))
	summary := TaskSummary(M{}, tel)
	jsonEq(t, "harness failures", summary["failed_tool_calls"], 0)
	jsonEq(t, "unreported outcomes", summary["tool_results_without_error_status"], 1)
	jsonEq(t, "coverage", summary["tool_error_status"], "partial")
	if !strings.Contains(util.Str(summary["failure_metric_basis"]), "is_error") {
		t.Fatal("failure basis must explain the signal")
	}
}

func TestDigestInputBudgetDoesNotTruncateStoredEvidence(t *testing.T) {
	original := strings.Repeat("x", ToolInputMax+1)
	ev := tool("a", "Bash", M{"command": original}, "", "")
	tel := AssignIDs(M{"events": []any{ev}})
	text := strings.Join(ToolInputEvidence(util.List(tel["events"])), "\n")
	if !strings.Contains(text, "input omitted from this digest") || strings.Contains(text, "input_raw (complete)") {
		t.Fatalf("oversized input must be labeled, not presented as complete: %s", text)
	}
	if util.Get(ev, "input_raw", "command") != original {
		t.Fatal("stored evidence was truncated")
	}
}

func TestAJXProductSuppressesOnlyBareProductMentions(t *testing.T) {
	for _, command := range []string{"ajx --help", "curl https://ajx.dev/agents/", "/opt/bin/ajx version"} {
		if unexpectedAJXMention(command, "ajx") {
			t.Fatalf("ordinary product access flagged: %s", command)
		}
	}
	for _, command := range []string{
		"cat ajx-review.md", "cat ajx_context.json", "curl https://ajx.dev.invalid",
		"cat /opt/shared/ajx/SKILL.md", "cat /opt/shared/ajx/prompts/narrate.md",
		`type C:\shared\ajx\SKILL.md`,
	} {
		if !unexpectedAJXMention(command, "ajx") {
			t.Fatalf("distinct AJX material was exempted: %s", command)
		}
	}
}

func TestCopiedAJXInstructionsOutsideWorkspaceRemainFlagged(t *testing.T) {
	sp := M{"trial": M{"product": "AJX", "output_dir": "/outer/results", "workspace_root": "/work"},
		"path": "/outer/trial.toml"}
	tel := AssignIDs(M{"events": []any{
		tool("a", "Read", M{"file_path": "/opt/shared/ajx/SKILL.md"}, "", ""),
		tool("b", "Read", M{"file_path": "/opt/shared/ajx/prompts/narrate.md"}, "", ""),
	}})
	flags := IsolationScan(tel, sp, M{"paths": M{"workspace": "/work/0123456789"}})
	if len(flags) != 2 {
		t.Fatalf("copied AJX instructions outside the workspace were exempted: %v", flags)
	}
}

func TestOwnAJXPathsDoNotHideSeparateExternalPaths(t *testing.T) {
	sp := M{"trial": M{"product": "AJX", "output_dir": "/outer/results", "workspace_root": "/work"},
		"path": "/outer/trial.toml"}
	state := M{"paths": M{"workspace": "/work/0123456789"}}
	for _, tc := range []struct {
		input   M
		flagged bool
	}{
		{M{"file_path": "/work/0123456789/ajx/SKILL.md"}, false},
		{M{"command": "cat /work/0123456789/ajx/SKILL.md"}, false},
		{M{"command": "cat /work/0123456789/ajx/SKILL.md /opt/shared/ajx/SKILL.md"}, true},
		{M{"command": "cat /work/0123456789/ajx/SKILL.md;cat /opt/shared/ajx/SKILL.md"}, true},
		{M{"file_path": "/work/0123456789-other/ajx/SKILL.md"}, true},
		{M{"file_path": "/work/0123456789/../shared/ajx/SKILL.md"}, true},
	} {
		tel := AssignIDs(M{"events": []any{tool("a", "Read", tc.input, "", "")}})
		if flags := IsolationScan(tel, sp, state); (len(flags) > 0) != tc.flagged {
			t.Errorf("input %v: got flags %v, want flagged=%v", tc.input, flags, tc.flagged)
		}
	}
}
