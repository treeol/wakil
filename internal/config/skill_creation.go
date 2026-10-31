package config

import "strings"

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
	// Mode is the default mode: off | on | auto | auto-mashura. Empty = "on".
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

// ModeOrDefault returns the configured default mode ("on" when unset).
func (c SkillCreationConfig) ModeOrDefault() string {
	if c.Mode == "" {
		return SkillCreationOn
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
