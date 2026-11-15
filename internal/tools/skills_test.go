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

// TestSkillTools_GatedWriteTools records that the skill write tools are in the
// gated set (see TestGatedToolListMatchesHandlers for the handler sync).
func TestSkillTools_GatedWriteTools(t *testing.T) {
	for _, w := range []string{"save_skill", "update_skill", "forget_skill"} {
		if !gatedTool(w) {
			t.Errorf("gatedTool(%q) = false, want true (write tools need confirmation)", w)
		}
	}
}

// TestSkillTools_ReadToolsUngated records that the skill read tools are not gated.
func TestSkillTools_ReadToolsUngated(t *testing.T) {
	for _, r := range []string{"list_skills", "load_skill", "skill_search", "skill_history"} {
		if gatedTool(r) {
			t.Errorf("gatedTool(%q) = true, want false (read tools ungated)", r)
		}
	}
}

func TestSkillTemplateTools_Classification(t *testing.T) {
	for _, n := range []string{"apply_skill_template", "capture_skill_template", "check_skill_template"} {
		if !hasTool(names(SkillTools()), n) {
			t.Errorf("SkillTools missing %q", n)
		}
	}
	for _, n := range []string{"apply_skill_template", "capture_skill_template"} {
		if !gatedTool(n) {
			t.Errorf("%q must be gated", n)
		}
	}
	if gatedTool("check_skill_template") {
		t.Error("check_skill_template is read-only and ungated")
	}
}
