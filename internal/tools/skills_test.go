package tools

import (
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

func names(ts []proxy.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Function.Name)
	}
	return out
}

func hasTool(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestSkillTools_InAllTiers(t *testing.T) {
	cwd := "/work"
	tiers := map[string][]string{
		"DefaultTools":   names(DefaultTools(cwd)),
		"DiscoveryTools": names(DiscoveryTools(cwd)),
		"EditTools":      names(EditTools(cwd)),
	}
	want := []string{"list_skills", "load_skill", "skill_search", "skill_history",
		"save_skill", "update_skill", "forget_skill"}
	for tier, toolNames := range tiers {
		for _, w := range want {
			if !hasTool(toolNames, w) {
				t.Errorf("%s missing skill tool %q", tier, w)
			}
		}
	}
}

func TestSkillTools_GatedWriteTools(t *testing.T) {
	for _, w := range []string{"save_skill", "update_skill", "forget_skill"} {
		if !GatedTool(w) {
			t.Errorf("GatedTool(%q) = false, want true (write tools need confirmation)", w)
		}
		if !IsSkillWriteTool(w) {
			t.Errorf("IsSkillWriteTool(%q) = false, want true (main-agent-only)", w)
		}
	}
}

func TestSkillTools_ReadToolsUngated(t *testing.T) {
	for _, r := range []string{"list_skills", "load_skill", "skill_search", "skill_history"} {
		if GatedTool(r) {
			t.Errorf("GatedTool(%q) = true, want false (read tools ungated)", r)
		}
		if IsSkillWriteTool(r) {
			t.Errorf("IsSkillWriteTool(%q) = true, want false (read tools available to subagents)", r)
		}
	}
}

func TestIsSkillTool(t *testing.T) {
	for _, st := range SkillTools() {
		if !IsSkillTool(st.Function.Name) {
			t.Errorf("IsSkillTool(%q) = false for a SkillTools-defined tool", st.Function.Name)
		}
	}
	if IsSkillTool("memory_get") || IsSkillTool("read_file") {
		t.Error("IsSkillTool returned true for a non-skill tool")
	}
}
