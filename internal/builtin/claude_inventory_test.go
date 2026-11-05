package builtin

import (
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/util"
)

func TestClaudeSkillAndCommandInventoriesAreDistinct(t *testing.T) {
	init := ccInit()
	init["skills"] = []any{"review", "ajx"}
	init["slash_commands"] = []any{"help", "review", "ajx", "compact"}
	tel := ccNormalize(t, []any{init}, nil)
	hr := util.MapOr(tel["harness_reported"])
	eq(t, hr["skills_visible"], 2, "skill inventory")
	eq(t, hr["slash_commands_visible"], 4, "command inventory")
	eq(t, hr["ajx_skills_visible"], []any{"ajx"}, "AJX skills")
	eq(t, hr["ajx_slash_commands_visible"], []any{"ajx"}, "AJX commands")
	for _, note := range strs(tel["limitations"]) {
		if strings.Contains(note, "user config") || strings.Contains(note, "is evaluation-aware") {
			t.Fatalf("inventory does not establish ownership or use: %s", note)
		}
	}
}

func TestClaudeAbsentSkillInventoryIsUnknown(t *testing.T) {
	init := ccInit()
	delete(init, "skills")
	init["slash_commands"] = []any{"ajx"}
	hr := util.MapOr(ccNormalize(t, []any{init}, nil)["harness_reported"])
	if hr["skills_visible"] != nil || hr["ajx_skills_visible"] != nil {
		t.Fatalf("missing inventory must remain unknown: %v", hr)
	}
	eq(t, hr["slash_commands_visible"], 1, "known command inventory")
	init["skills"] = []any{}
	hr = util.MapOr(ccNormalize(t, []any{init}, nil)["harness_reported"])
	eq(t, hr["skills_visible"], 0, "explicit empty skill inventory")
}

func TestClaudeMissingToolErrorFlagStaysUnknown(t *testing.T) {
	result := ccToolResult("t1", "2026-01-01T00:00:02Z", "ok")
	delete(util.Maps(util.Get(result, "message", "content"))[0], "is_error")
	rows := []any{ccAssistant("m1", ccToolUse("t1", "echo ok"), "2026-01-01T00:00:01Z", 1, ""), result}
	tel := ccNormalize(t, []any{ccInit()}, rows)
	events := util.Maps(tel["events"])
	if len(events) != 1 || events[0]["is_error"] != nil {
		t.Fatalf("missing error flag became a success: %v", events)
	}
}
