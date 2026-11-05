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
	eq(t, hr["skills_inventory"], []any{
		M{"name": "review", "source": nil, "source_status": "unknown"},
		M{"name": "ajx", "source": nil, "source_status": "unknown"},
	}, "named skills with unknown sources")
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
	if hr["skills_visible"] != nil || hr["ajx_skills_visible"] != nil || hr["skills_inventory"] != nil {
		t.Fatalf("missing inventory must remain unknown: %v", hr)
	}
	eq(t, hr["slash_commands_visible"], 1, "known command inventory")
	init["skills"] = []any{}
	hr = util.MapOr(ccNormalize(t, []any{init}, nil)["harness_reported"])
	eq(t, hr["skills_visible"], 0, "explicit empty skill inventory")
	eq(t, hr["skills_inventory"], []any{}, "explicit empty inventory entries")
}

func TestClaudeSkillSourcesRequireExplicitPerEntryEvidence(t *testing.T) {
	init := ccInit()
	init["skills"] = []any{
		M{"name": "review", "source": "plugin:example"},
		M{"name": "ajx", "description": "source not supplied"},
		"plain",
		M{"name": "other", "source": M{"unknown": "shape"}},
		M{"source": "ajx"},
	}
	init["plugins"] = []any{M{"name": "plain", "path": "/synthetic/plugins/plain"}}
	hr := util.MapOr(ccNormalize(t, []any{init}, nil)["harness_reported"])
	eq(t, hr["skills_inventory"], []any{
		M{"name": "review", "source": "plugin:example", "source_status": "reported"},
		M{"name": "ajx", "source": nil, "source_status": "unknown"},
		M{"name": "plain", "source": nil, "source_status": "unknown"},
		M{"name": "other", "source": nil, "source_status": "unknown"},
		M{"name": nil, "source": nil, "source_status": "unknown"},
	}, "explicit names and source labels only")
	eq(t, hr["ajx_skills_visible"], []any{"ajx"}, "source text is not a skill name")
	eq(t, hr["skills_visible"], 5, "preserve reported entry count")
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
