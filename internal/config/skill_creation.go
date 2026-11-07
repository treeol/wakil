package config

import (
	"fmt"
	"path"
	"strings"
)

// Skill-creation modes (/skill-creation).
const (
	SkillCreationOff         = "off"
	SkillCreationOn          = "on"
	SkillCreationAuto        = "auto"
	SkillCreationAutoMashura = "auto-mashura"
)

// Defaults applied when the corresponding SkillCreationConfig field is zero.
const (
	DefaultSkillCreationMaxAutoPerDay   = 3
	DefaultSkillCreationMaxDrafts       = 10
	DefaultSkillCreationDraftTTLDays    = 30
	DefaultSkillCreationReviewTimeout   = 180
	DefaultSkillCreationMaxReviewBytes  = 120 * 1024
	DefaultSkillCreationMaxCaptureFiles = 12
)

// defaultSkillCreationAllowedFiles is the file-type allowlist for captured
// template files: base-name globs matched with path.Match.
var defaultSkillCreationAllowedFiles = []string{
	"*.yml", "*.yaml", "*.sh", "*.sql", "*.conf", "*.md", "Dockerfile", "Dockerfile.*",
}

// SkillCreationConfig holds the knobs for automatic skill creation. Every
// limit is configurable; zero values select the documented defaults.
type SkillCreationConfig struct {
	// Mode is the default mode: off | on | auto | auto-mashura. Empty = "off": the user opts in with `/skill-creation on` or this key.
	// A per-repo saved setting and a per-session override take precedence.
	Mode string `json:"mode,omitempty"`
	// MaxAutoPerDay caps automatically created skills/drafts per UTC day.
	MaxAutoPerDay int `json:"max_auto_per_day,omitempty"`
	// MaxDrafts caps pending drafts in the global skill store.
	MaxDrafts int `json:"max_drafts,omitempty"`
	// DraftTTLDays expires pending drafts after this many days.
	DraftTTLDays int `json:"draft_ttl_days,omitempty"`
	// ReviewTimeoutSeconds bounds one whole auto-mashura pipeline run.
	ReviewTimeoutSeconds int `json:"review_timeout_seconds,omitempty"`
	// MaxReviewBytes caps the draft size sent to Mashūra.
	MaxReviewBytes int `json:"max_review_bytes,omitempty"`
	// MaxCaptureFiles caps how many files one automatic capture may include.
	MaxCaptureFiles int `json:"max_capture_files,omitempty"`
	// Panel names the Mashūra panel used for skill reviews. Empty = default panel.
	Panel string `json:"panel,omitempty"`
	// AllowedFiles overrides the base-name glob allowlist for captured files.
	AllowedFiles []string `json:"allowed_files,omitempty"`
}

// Validate rejects values that cannot work: negative limits (0 = default) and
// allowed_files globs that path.Match cannot parse.
func (c SkillCreationConfig) Validate() error {
	for name, v := range map[string]int{
		"max_auto_per_day": c.MaxAutoPerDay, "max_drafts": c.MaxDrafts, "draft_ttl_days": c.DraftTTLDays,
		"review_timeout_seconds": c.ReviewTimeoutSeconds, "max_review_bytes": c.MaxReviewBytes,
		"max_capture_files": c.MaxCaptureFiles,
	} {
		if v < 0 {
			return fmt.Errorf("skill_creation.%s must be >= 0 (0 = default), got %d", name, v)
		}
	}
	if c.DraftTTLDays > 3650 {
		return fmt.Errorf("skill_creation.draft_ttl_days must be <= 3650, got %d", c.DraftTTLDays)
	}
	if c.ReviewTimeoutSeconds > 86400 {
		return fmt.Errorf("skill_creation.review_timeout_seconds must be <= 86400, got %d", c.ReviewTimeoutSeconds)
	}
	for _, g := range c.AllowedFiles {
		if _, err := path.Match(g, "x"); err != nil {
			return fmt.Errorf("skill_creation.allowed_files: invalid glob %q: %w", g, err)
		}
	}
	return nil
}

// ValidSkillCreationMode reports whether m is empty or a known mode.
func ValidSkillCreationMode(m string) bool {
	switch m {
	case "", SkillCreationOff, SkillCreationOn, SkillCreationAuto, SkillCreationAutoMashura:
		return true
	}
	return false
}

// NormalizeSkillCreationMode lowercases/trims m; "" stays "".
func NormalizeSkillCreationMode(m string) string { return strings.ToLower(strings.TrimSpace(m)) }

func posOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// ModeOrDefault returns the configured default mode ("off" when unset).
// 0.4.4 (D2): the default moved from "on" to "off"; an explicit setting still wins.
func (c SkillCreationConfig) ModeOrDefault() string {
	if c.Mode == "" {
		return SkillCreationOff
	}
	return c.Mode
}

func (c SkillCreationConfig) MaxAutoPerDayOrDefault() int {
	return posOr(c.MaxAutoPerDay, DefaultSkillCreationMaxAutoPerDay)
}
func (c SkillCreationConfig) MaxDraftsOrDefault() int {
	return posOr(c.MaxDrafts, DefaultSkillCreationMaxDrafts)
}
func (c SkillCreationConfig) DraftTTLDaysOrDefault() int {
	return posOr(c.DraftTTLDays, DefaultSkillCreationDraftTTLDays)
}
func (c SkillCreationConfig) ReviewTimeoutSecondsOrDefault() int {
	return posOr(c.ReviewTimeoutSeconds, DefaultSkillCreationReviewTimeout)
}
func (c SkillCreationConfig) MaxReviewBytesOrDefault() int {
	return posOr(c.MaxReviewBytes, DefaultSkillCreationMaxReviewBytes)
}
func (c SkillCreationConfig) MaxCaptureFilesOrDefault() int {
	return posOr(c.MaxCaptureFiles, DefaultSkillCreationMaxCaptureFiles)
}

// AllowedFilesOrDefault returns the allowlist globs (defaults when unset).
func (c SkillCreationConfig) AllowedFilesOrDefault() []string {
	if len(c.AllowedFiles) > 0 {
		return c.AllowedFiles
	}
	return append([]string(nil), defaultSkillCreationAllowedFiles...)
}
