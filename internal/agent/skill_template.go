package agent

// skill_template.go — template bundles carried inside a skill's value
// (skill-template Phases 1–2). A skill stays a prose reference doc; it may
// additionally end with a line-oriented template section holding a JSON
// manifest (params, files, verify commands) and verbatim file bodies:
//
//	<prose>
//
//	=== wakil-template v1 ===
//	{"params":[...],"files":[{"path":"deploy.sh","mode":"0755"}],"verify":["bash -n deploy.sh"]}
//	=== file: deploy.sh ===
//	<verbatim body>
//	=== end: deploy.sh ===
//
// Design notes:
//   - No schema change: the section lives in the existing skill value, so it
//     inherits history, confirm gating, secret screening, and the 256 KiB cap.
//   - Line-oriented on purpose: update_skill's line diff stays readable and
//     FTS still indexes the bodies.
//   - Placeholders are @@NAME@@, substituted in one pass with no logic. Go
//     text/template or {{VAR}} would corrupt real-world bodies that already
//     contain docker --format '{{.ID}}' style strings.
//   - The end marker carries the file path, so a body may contain other
//     marker-looking lines; only its own end marker terminates it.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	tmplSectionMarker = "=== wakil-template v1 ==="
	tmplFilePrefix    = "=== file: "
	tmplEndPrefix     = "=== end: "
	tmplMarkerSuffix  = " ==="

	// tmplDefaultMode is applied when a manifest file entry has no mode.
	tmplDefaultMode = "0644"

	// Bounds (Mashūra plan review): cap work and output, not just storage.
	tmplMaxFiles        = 50
	tmplMaxParamBytes   = 4096
	tmplMaxRenderedSize = 1 << 20

	// tmplReservedDir is never a valid template output: it holds the
	// provenance file and the workflow plan.
	tmplReservedDir = ".wakil"

	// tmplKindSnippet marks a manifest entry that is never written to disk:
	// a tested block returned to the agent as reference material.
	tmplKindSnippet = "snippet"

	// tmplMaxSnippetBytes bounds one snippet body (storage and the apply
	// result both stay small).
	tmplMaxSnippetBytes = 16 * 1024
)

var (
	// tmplNameRE: lowercase slug used to select a named template.
	tmplNameRE      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	tmplParamNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	tmplTokenRE     = regexp.MustCompile(`@@([A-Z][A-Z0-9_]*)@@`)
	// tmplModeRE: octal permission bits; the owner must be able to read the
	// file (0000-style unreadable modes are rejected).
	tmplModeRE = regexp.MustCompile(`^0[4-7][0-7]{2}$`)
)

// templateParam declares one placeholder. Required ⇔ no default: a param with
// a default is optional; a required param has none. Pattern is a full match
// (implicitly anchored). Secret params are deliberately unsupported: secrets
// belong in runtime references (.env files, docker secrets) that the template
// points at, never in values that pass through tool arguments and the
// transcript.
type templateParam struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Default     string `json:"default,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Pattern     string `json:"pattern,omitempty"`
}

// patternRE compiles the param's full-match pattern.
func (p templateParam) patternRE() (*regexp.Regexp, error) {
	return regexp.Compile(`^(?:` + p.Pattern + `)$`)
}

// templateDef declares one NAMED template of a skill. A skill without any
// templateDef carries the legacy single anonymous template (every file is
// always applied). Requires is a HINT only: apply notes a missing dependency
// but never adds or refuses anything, and a satisfied hint does not prove the
// combination is valid. Verify is scoped to this template (the manifest-level
// Verify stays global).
type templateDef struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Requires    []string `json:"requires,omitempty"`
	Verify      []string `json:"verify,omitempty"`
}

// templateFile is one entry of the bundle: a file written to disk, or a
// snippet (Kind == "snippet") returned to the agent and never written. Body
// is carried in the section, not in the JSON manifest. For a snippet, Path is
// only a unique id and Target names the workspace file it belongs in. Paths
// and ids share one namespace.
type templateFile struct {
	Path     string `json:"path"`
	Mode     string `json:"mode,omitempty"`
	NoEOL    bool   `json:"no_eol,omitempty"` // body has no trailing newline
	Template string `json:"template,omitempty"`
	Kind     string `json:"kind,omitempty"` // "" (file) or "snippet"
	Target   string `json:"target,omitempty"`
	Body     string `json:"-"`
}

// isSnippet reports whether the entry is a snippet.
func (f templateFile) isSnippet() bool { return f.Kind == tmplKindSnippet }

// skillTemplate is a parsed template section.
type skillTemplate struct {
	Params    []templateParam `json:"params,omitempty"`
	Templates []templateDef   `json:"templates,omitempty"`
	Files     []templateFile  `json:"files"`
	Verify    []string        `json:"verify,omitempty"`
}

// renderedFile is one template file after placeholder substitution.
type renderedFile struct {
	Path     string // workspace-relative to the target dir, slash-separated
	Mode     string
	Content  string
	SrcPath  string // the template's own path (source identity for check)
	Template string // owning named template ("" for legacy)
}

// splitSkillTemplate separates a skill value into prose and the template
// section (marker line included). found is false when the value carries no
// template section. The FIRST line equal to the marker starts the section:
// prose must never contain the marker line (enforced on write).
func splitSkillTemplate(value string) (prose, section string, found bool) {
	// Line-by-line so LF and CRLF marker lines are recognized alike
	// (parseTemplateSection tolerates \r; the split must agree, or a CRLF
	// section would silently be treated as prose).
	off := 0
	for off <= len(value) {
		end := strings.IndexByte(value[off:], '\n')
		line := value[off:]
		if end >= 0 {
			line = value[off : off+end]
		}
		if strings.TrimSuffix(line, "\r") == tmplSectionMarker {
			return value[:off], value[off:], true
		}
		if end < 0 {
			break
		}
		off += end + 1
	}
	return value, "", false
}

// skillProse returns the value without its template section (the whole value
// when there is none), with trailing whitespace trimmed.
func skillProse(value string) string {
	prose, _, found := splitSkillTemplate(value)
	if !found {
		return value
	}
	return strings.TrimRight(prose, " \t\r\n")
}

// parseSkillTemplateValue parses the template section of a skill value.
// Returns (nil, nil) when the value has no template section.
func parseSkillTemplateValue(value string) (*skillTemplate, error) {
	_, section, found := splitSkillTemplate(value)
	if !found {
		return nil, nil
	}
	return parseTemplateSection(section)
}

// parseTemplateSection parses a section starting with the marker line.
func parseTemplateSection(section string) (*skillTemplate, error) {
	lines := strings.Split(section, "\n")
	if len(lines) == 0 || strings.TrimSuffix(lines[0], "\r") != tmplSectionMarker {
		return nil, fmt.Errorf("template section must start with %q", tmplSectionMarker)
	}
	i := 1
	var manifest []string
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], tmplFilePrefix) {
			break
		}
		manifest = append(manifest, lines[i])
	}
	var t skillTemplate
	dec := json.NewDecoder(strings.NewReader(strings.Join(manifest, "\n")))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("template manifest: %w", err)
	}
	// dec.More() only reports array/object continuation; a second Decode
	// must hit io.EOF for the manifest to be exactly one JSON object.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("template manifest: unexpected content after the JSON object")
	}

	bodies := map[string][]string{}
	var order []string
	for i < len(lines) {
		line := strings.TrimSuffix(lines[i], "\r")
		if strings.TrimSpace(line) == "" {
			i++
			continue
		}
		if !strings.HasPrefix(line, tmplFilePrefix) || !strings.HasSuffix(line, tmplMarkerSuffix) {
			return nil, fmt.Errorf("template section line %d: expected %q, got %q", i+1, tmplFilePrefix+"<path>"+tmplMarkerSuffix, truncateRunes(line, 80))
		}
		p := strings.TrimSuffix(strings.TrimPrefix(line, tmplFilePrefix), tmplMarkerSuffix)
		end := tmplEndPrefix + p + tmplMarkerSuffix
		if _, dup := bodies[p]; dup {
			return nil, fmt.Errorf("template file %q has more than one body", p)
		}
		j := i + 1
		for j < len(lines) && strings.TrimSuffix(lines[j], "\r") != end {
			j++
		}
		if j >= len(lines) {
			return nil, fmt.Errorf("template file %q: missing end marker %q", p, end)
		}
		bodies[p] = lines[i+1 : j]
		order = append(order, p)
		i = j + 1
	}

	if len(t.Files) != len(bodies) {
		return nil, fmt.Errorf("template manifest lists %d file(s) but the section has %d body(ies)", len(t.Files), len(bodies))
	}
	for k := range t.Files {
		f := &t.Files[k]
		body, ok := bodies[f.Path]
		if !ok {
			return nil, fmt.Errorf("template manifest file %q has no body (bodies: %s)", f.Path, strings.Join(order, ", "))
		}
		// Zero lines between the markers is the empty file. Otherwise the
		// line break before the end marker is synthetic unless the body had a
		// trailing newline (no_eol=false).
		// The encoding is canonical (what serializeTemplateSection writes):
		// an empty file is zero lines + no_eol, and a no_eol body must not end
		// in a newline. Rejecting the other spellings keeps "apply exactly"
		// unambiguous for hand-edited sections.
		if len(body) == 0 {
			if !f.NoEOL {
				return nil, fmt.Errorf("template file %q: empty body requires \"no_eol\":true (a single empty line is a one-newline file)", f.Path)
			}
			continue
		}
		f.Body = strings.Join(body, "\n")
		if !f.NoEOL {
			f.Body += "\n"
		} else if strings.HasSuffix(f.Body, "\n") {
			return nil, fmt.Errorf("template file %q: \"no_eol\":true but the body ends with an empty line", f.Path)
		}
	}
	if err := t.validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// validate checks the manifest invariants shared by parse and capture.
func (t *skillTemplate) validate() error {
	if len(t.Files) == 0 {
		return fmt.Errorf("template must contain at least one file")
	}
	if len(t.Files) > tmplMaxFiles {
		return fmt.Errorf("template has %d files (max %d)", len(t.Files), tmplMaxFiles)
	}
	declared := map[string]bool{}
	for _, p := range t.Params {
		if !tmplParamNameRE.MatchString(p.Name) {
			return fmt.Errorf("template param %q: name must match %s", p.Name, tmplParamNameRE)
		}
		if declared[p.Name] {
			return fmt.Errorf("template param %q declared twice", p.Name)
		}
		declared[p.Name] = true
		if p.Required && p.Default != "" {
			return fmt.Errorf("template param %q: required and default are mutually exclusive (a default makes it optional)", p.Name)
		}
		if p.Pattern != "" {
			re, err := p.patternRE()
			if err != nil {
				return fmt.Errorf("template param %q: invalid pattern: %w", p.Name, err)
			}
			if p.Default != "" && !re.MatchString(p.Default) {
				return fmt.Errorf("template param %q: default %q does not match its pattern", p.Name, p.Default)
			}
		}
		if err := validateParamValue(p.Default); err != nil {
			return fmt.Errorf("template param %q default: %w", p.Name, err)
		}
	}
	tdefs, err := t.validateTemplateDefs()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for k := range t.Files {
		f := &t.Files[k]
		if err := validateTemplatePath(f.Path); err != nil {
			return err
		}
		if seen[f.Path] {
			return fmt.Errorf("template file %q listed twice", f.Path)
		}
		seen[f.Path] = true
		switch {
		case len(tdefs) == 0 && f.Template != "":
			return fmt.Errorf("template file %q names template %q but the manifest declares no templates", f.Path, f.Template)
		case len(tdefs) > 0 && f.Template == "":
			return fmt.Errorf("template file %q must name one of the declared templates (%s)", f.Path, strings.Join(t.templateNames(), ", "))
		case len(tdefs) > 0 && !tdefs[f.Template]:
			return fmt.Errorf("template file %q names undeclared template %q", f.Path, f.Template)
		}
		switch f.Kind {
		case "":
			if f.Target != "" {
				return fmt.Errorf("template file %q: target is only valid for snippets", f.Path)
			}
			if f.Mode == "" {
				f.Mode = tmplDefaultMode
			}
			if !tmplModeRE.MatchString(f.Mode) {
				return fmt.Errorf("template file %q: mode %q must be octal like 0644 or 0755 (owner-readable)", f.Path, f.Mode)
			}
		case tmplKindSnippet:
			if len(tdefs) == 0 {
				return fmt.Errorf("template snippet %q: snippets need named templates (declare \"templates\" and give the snippet a \"template\")", f.Path)
			}
			if err := checkSnippetDelimiters(f.Path, f.Body); err != nil {
				return err
			}
			if f.Mode != "" {
				return fmt.Errorf("template snippet %q: mode is not applicable (snippets are never written)", f.Path)
			}
			if f.Target == "" {
				return fmt.Errorf("template snippet %q: target (the workspace file it belongs in) is required", f.Path)
			}
			if err := validateTemplatePath(f.Target); err != nil {
				return fmt.Errorf("template snippet %q target: %w", f.Path, err)
			}
			if len(f.Body) > tmplMaxSnippetBytes {
				return fmt.Errorf("template snippet %q is %d bytes (max %d)", f.Path, len(f.Body), tmplMaxSnippetBytes)
			}
		default:
			return fmt.Errorf("template file %q: unknown kind %q (use \"snippet\" or omit)", f.Path, f.Kind)
		}
		if err := validateTemplateBody(f.Path, f.Body); err != nil {
			return err
		}
		for _, src := range []string{f.Path, f.Target, f.Body} {
			for _, m := range tmplTokenRE.FindAllStringSubmatch(src, -1) {
				if !declared[m[1]] {
					return fmt.Errorf("template file %q uses undeclared placeholder @@%s@@", f.Path, m[1])
				}
			}
		}
	}
	return nil
}

// snippetDelimPrefix starts every delimiter line apply_skill_template puts
// around a returned snippet. A snippet body must never contain a line that
// starts like one, or its text could close the "untrusted data" frame early.
const snippetDelimPrefix = "<<<"

// checkSnippetDelimiters rejects snippet text with a line that starts like
// the result frame's delimiters.
func checkSnippetDelimiters(id, body string) error {
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimLeft(l, " \t"), snippetDelimPrefix) {
			return fmt.Errorf("template snippet %q has a line starting with %q, which is reserved for result delimiters", id, snippetDelimPrefix)
		}
	}
	return nil
}

// templateNames lists declared template names in manifest order.
func (t *skillTemplate) templateNames() []string {
	names := make([]string, len(t.Templates))
	for i, d := range t.Templates {
		names[i] = d.Name
	}
	return names
}

// templateDef returns the declaration of a named template.
func (t *skillTemplate) templateDefByName(name string) (templateDef, bool) {
	for _, d := range t.Templates {
		if d.Name == name {
			return d, true
		}
	}
	return templateDef{}, false
}

// validateTemplateDefs checks the named-template declarations and returns the
// set of declared names (empty for a legacy single-template manifest).
func (t *skillTemplate) validateTemplateDefs() (map[string]bool, error) {
	names := map[string]bool{}
	for _, d := range t.Templates {
		if !tmplNameRE.MatchString(d.Name) {
			return nil, fmt.Errorf("template name %q must match %s", d.Name, tmplNameRE)
		}
		if names[d.Name] {
			return nil, fmt.Errorf("template %q declared twice", d.Name)
		}
		names[d.Name] = true
	}
	for _, d := range t.Templates {
		for _, r := range d.Requires {
			if r == d.Name {
				return nil, fmt.Errorf("template %q requires itself", d.Name)
			}
			if !names[r] {
				return nil, fmt.Errorf("template %q requires undeclared template %q", d.Name, r)
			}
		}
		for _, v := range d.Verify {
			if strings.ContainsAny(v, "\r\n\x00") {
				return nil, fmt.Errorf("template %q: verify commands must be single-line", d.Name)
			}
		}
	}
	// requires is a hint, but a cycle is still a mistake worth rejecting.
	state := map[string]int{} // 0 new, 1 visiting, 2 done
	var visit func(string) error
	visit = func(n string) error {
		switch state[n] {
		case 1:
			return fmt.Errorf("templates form a requires cycle through %q", n)
		case 2:
			return nil
		}
		state[n] = 1
		if d, ok := t.templateDefByName(n); ok {
			for _, r := range d.Requires {
				if err := visit(r); err != nil {
					return err
				}
			}
		}
		state[n] = 2
		return nil
	}
	for _, d := range t.Templates {
		if err := visit(d.Name); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// validateTemplatePath requires a clean, relative, slash-separated path that
// cannot escape the target directory or break the section markers.
func validateTemplatePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("template file path is required")
	case strings.ContainsRune(p, '\\') || strings.IndexFunc(p, isControlRune) >= 0:
		return fmt.Errorf("template file path %q contains a backslash or control character", p)
	case strings.Contains(p, "==="):
		return fmt.Errorf("template file path %q must not contain \"===\"", p)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("template file path %q must be relative", p)
	case path.Clean(p) != p || p == ".":
		return fmt.Errorf("template file path %q must be clean (no ./, //, or trailing /)", p)
	}
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		if seg == ".." {
			return fmt.Errorf("template file path %q must not contain \"..\"", p)
		}
	}
	if segs[0] == tmplReservedDir {
		return fmt.Errorf("template file path %q: %s/ is reserved (provenance and workflow files)", p, tmplReservedDir)
	}
	return nil
}

// isControlRune reports C0/C1 control characters and DEL (tab included —
// paths are shown in confirm previews and must not carry layout tricks).
func isControlRune(r rune) bool {
	return r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F)
}

// validateTemplateBody requires text content that survives storage in a
// TEXT/FTS column and the line-oriented section format.
func validateTemplateBody(p, body string) error {
	if !utf8.ValidString(body) {
		return fmt.Errorf("template file %q is not valid UTF-8 (binary files are not supported)", p)
	}
	if strings.ContainsRune(body, 0) {
		return fmt.Errorf("template file %q contains NUL bytes (binary files are not supported)", p)
	}
	end := tmplEndPrefix + p + tmplMarkerSuffix
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSuffix(l, "\r") == end {
			return fmt.Errorf("template file %q contains its own end marker line", p)
		}
	}
	return nil
}

// validateParamValue rejects values that could break single-line placeholder
// semantics or the provenance file.
func validateParamValue(v string) error {
	if strings.ContainsAny(v, "\r\n\x00") {
		return fmt.Errorf("value must be a single line without NUL")
	}
	if len(v) > tmplMaxParamBytes {
		return fmt.Errorf("value exceeds %d bytes", tmplMaxParamBytes)
	}
	return nil
}

// selectEntries returns the manifest entries chosen by the named templates,
// in manifest order. A legacy manifest (no declared templates) has one
// anonymous template: names must be empty and every entry is selected. With
// declared templates a non-empty, duplicate-free, known selection is required.
func (t *skillTemplate) selectEntries(names []string) ([]templateFile, error) {
	if len(t.Templates) == 0 {
		if len(names) > 0 {
			return nil, fmt.Errorf("this skill declares no named templates, but templates %s were requested", strings.Join(names, ", "))
		}
		return append([]templateFile(nil), t.Files...), nil
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("this skill has named templates — choose from: %s", strings.Join(t.templateNames(), ", "))
	}
	chosen := map[string]bool{}
	for _, n := range names {
		if _, ok := t.templateDefByName(n); !ok {
			return nil, fmt.Errorf("unknown template %q — available: %s", n, strings.Join(t.templateNames(), ", "))
		}
		if chosen[n] {
			return nil, fmt.Errorf("template %q selected twice", n)
		}
		chosen[n] = true
	}
	var out []templateFile
	for _, f := range t.Files {
		if chosen[f.Template] {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the selected template(s) %s contain no files or snippets", strings.Join(names, ", "))
	}
	return out, nil
}

// missingRequires lists "A requires B" hints for selected templates whose
// declared dependencies were not selected. Hint only — nothing is refused.
func (t *skillTemplate) missingRequires(names []string) []string {
	chosen := map[string]bool{}
	for _, n := range names {
		chosen[n] = true
	}
	var out []string
	for _, n := range names {
		d, _ := t.templateDefByName(n)
		for _, r := range d.Requires {
			if !chosen[r] {
				out = append(out, fmt.Sprintf("%s requires %s (not selected)", n, r))
			}
		}
	}
	return out
}

// selectedVerify returns the global verify commands plus those scoped to the
// selected templates.
func (t *skillTemplate) selectedVerify(names []string) []string {
	out := append([]string(nil), t.Verify...)
	for _, n := range names {
		if d, ok := t.templateDefByName(n); ok {
			out = append(out, d.Verify...)
		}
	}
	return out
}

// referencedParams returns the declared param names used by the entries'
// paths, targets and bodies.
func referencedParams(entries []templateFile) map[string]bool {
	ref := map[string]bool{}
	for _, f := range entries {
		for _, src := range []string{f.Path, f.Target, f.Body} {
			for _, m := range tmplTokenRE.FindAllStringSubmatch(src, -1) {
				ref[m[1]] = true
			}
		}
	}
	return ref
}

// resolveTemplateParams merges supplied values with defaults and validates
// them for the whole template (legacy behavior: every declared param is in
// scope). It reports every missing required param at once.
func resolveTemplateParams(t *skillTemplate, supplied map[string]string) (map[string]string, error) {
	values, _, err := resolveParamsScoped(t, nil, supplied, false)
	return values, err
}

// resolveParamsScoped resolves params for a selection. scope limits which
// declared params are resolved (nil: all); supplied params outside the scope
// but declared are ignored, undeclared ones are an error. With allowUnfilled a
// required param without a value is not an error: it is reported in unfilled
// and its @@NAME@@ tokens stay in the rendered bodies.
func resolveParamsScoped(t *skillTemplate, scope map[string]bool, supplied map[string]string, allowUnfilled bool) (values map[string]string, unfilled []string, err error) {
	declared := map[string]templateParam{}
	for _, p := range t.Params {
		declared[p.Name] = p
	}
	var unknown []string
	for k := range supplied {
		if _, ok := declared[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, nil, fmt.Errorf("unknown template param(s): %s", strings.Join(unknown, ", "))
	}
	values = map[string]string{}
	var missing []string
	for _, p := range t.Params {
		if scope != nil && !scope[p.Name] {
			continue
		}
		v, ok := supplied[p.Name]
		if !ok {
			v = p.Default
		}
		if v == "" && p.Required {
			if allowUnfilled {
				unfilled = append(unfilled, p.Name)
			} else {
				missing = append(missing, p.Name)
			}
			continue
		}
		if err := validateParamValue(v); err != nil {
			return nil, nil, fmt.Errorf("template param %s: %w", p.Name, err)
		}
		if p.Pattern != "" && v != "" {
			re, err := p.patternRE()
			if err != nil {
				return nil, nil, fmt.Errorf("template param %s: invalid pattern: %w", p.Name, err)
			}
			if !re.MatchString(v) {
				return nil, nil, fmt.Errorf("template param %s: value %q does not fully match pattern %s", p.Name, v, p.Pattern)
			}
		}
		values[p.Name] = v
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("missing required template param(s): %s", strings.Join(missing, ", "))
	}
	return values, unfilled, nil
}

// substitute replaces every @@NAME@@ token in s in a single pass. Tokens for
// names absent from values are left untouched (unfilled params stay as
// tokens; validate guarantees every token is declared).
func substitute(s string, values map[string]string) string {
	return tmplTokenRE.ReplaceAllStringFunc(s, func(tok string) string {
		if v, ok := values[tok[2:len(tok)-2]]; ok {
			return v
		}
		return tok
	})
}

// renderedSnippet is one snippet after placeholder substitution. Snippets are
// returned to the agent and never written.
type renderedSnippet struct {
	ID       string
	Template string
	Target   string
	Content  string
}

// renderTemplate renders every file of a legacy/whole template with resolved
// values (snippets are skipped). Rendered paths are re-validated (a
// placeholder value must not introduce traversal).
func renderTemplate(t *skillTemplate, values map[string]string) ([]renderedFile, error) {
	files, _, err := renderSelection(t.Files, values)
	return files, err
}

// renderSelection renders the chosen entries: files are validated and
// collision-checked; snippets are only substituted. A path or snippet target
// that still contains an @@NAME@@ token (unfilled param) is refused — an
// unresolved name must never become a literal file name.
func renderSelection(entries []templateFile, values map[string]string) ([]renderedFile, []renderedSnippet, error) {
	out := make([]renderedFile, 0, len(entries))
	var snips []renderedSnippet
	seen := map[string]bool{}
	total := 0
	for _, f := range entries {
		content := substitute(f.Body, values)
		total += len(content)
		if total > tmplMaxRenderedSize {
			return nil, nil, fmt.Errorf("rendered template exceeds %d bytes", tmplMaxRenderedSize)
		}
		if f.isSnippet() {
			if len(content) > tmplMaxSnippetBytes {
				return nil, nil, fmt.Errorf("snippet %q renders to %d bytes (max %d)", f.Path, len(content), tmplMaxSnippetBytes)
			}
			if err := checkSnippetDelimiters(f.Path, content); err != nil {
				return nil, nil, fmt.Errorf("rendered %w", err)
			}
			target := substitute(f.Target, values)
			if m := tmplTokenRE.FindString(target); m != "" {
				return nil, nil, fmt.Errorf("snippet %q: target %q still contains placeholder %s — supply that param", f.Path, target, m)
			}
			if err := validateTemplatePath(target); err != nil {
				return nil, nil, fmt.Errorf("rendered target for snippet %q: %w", f.Path, err)
			}
			snips = append(snips, renderedSnippet{ID: f.Path, Template: f.Template, Target: target, Content: content})
			continue
		}
		p := substitute(f.Path, values)
		if m := tmplTokenRE.FindString(p); m != "" {
			return nil, nil, fmt.Errorf("file path %q still contains placeholder %s — supply that param (unfilled placeholders are only allowed in file contents)", p, m)
		}
		if err := validateTemplatePath(p); err != nil {
			return nil, nil, fmt.Errorf("rendered path for %q: %w", f.Path, err)
		}
		if seen[p] {
			return nil, nil, fmt.Errorf("two template files render to the same path %q", p)
		}
		seen[p] = true
		out = append(out, renderedFile{Path: p, Mode: f.Mode, Content: content, SrcPath: f.Path, Template: f.Template})
	}
	// A rendered path that is a prefix directory of another (a, a/b) can
	// never both be files.
	for p := range seen {
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if seen[dir] {
				return nil, nil, fmt.Errorf("template file %q is also the parent directory of %q", dir, p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, snips, nil
}

// serializeTemplateSection renders a template back to its section text.
func serializeTemplateSection(t *skillTemplate) (string, error) {
	files := make([]templateFile, len(t.Files))
	for i, f := range t.Files {
		f.NoEOL = !strings.HasSuffix(f.Body, "\n")
		if f.Mode == "" && !f.isSnippet() {
			f.Mode = tmplDefaultMode
		}
		files[i] = f
	}
	manifest := skillTemplate{Params: t.Params, Templates: t.Templates, Files: files, Verify: t.Verify}
	if err := manifest.validate(); err != nil {
		return "", err
	}
	var mb bytes.Buffer
	enc := json.NewEncoder(&mb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(manifest); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(tmplSectionMarker + "\n")
	b.WriteString(mb.String()) // Encode appends "\n"
	for _, f := range files {
		b.WriteString(tmplFilePrefix + f.Path + tmplMarkerSuffix + "\n")
		b.WriteString(f.Body)
		if f.NoEOL && f.Body != "" {
			b.WriteString("\n")
		}
		b.WriteString(tmplEndPrefix + f.Path + tmplMarkerSuffix + "\n")
	}
	return b.String(), nil
}

// joinSkillTemplate builds a full skill value from prose and a template.
func joinSkillTemplate(prose string, t *skillTemplate) (string, error) {
	section, err := serializeTemplateSection(t)
	if err != nil {
		return "", err
	}
	prose = strings.TrimRight(prose, " \t\r\n")
	if prose == "" {
		return section, nil
	}
	return prose + "\n\n" + section, nil
}

// validateSkillTemplateValue enforces template invariants on a value about to
// be saved: prose must not contain the marker in an unparseable way, and a
// present section must parse.
func validateSkillTemplateValue(value string) error {
	if _, err := parseSkillTemplateValue(value); err != nil {
		return fmt.Errorf("invalid template section: %w", err)
	}
	return nil
}

// summarizeSkillTemplate renders the compact template description shown by
// load_skill instead of the file bodies. Named templates are grouped with
// their files, snippets, requires hints and the params they reference.
func summarizeSkillTemplate(t *skillTemplate) string {
	var b strings.Builder
	b.WriteString("## Template (apply with apply_skill_template")
	if len(t.Templates) > 0 {
		b.WriteString(" + templates: [...]")
	}
	b.WriteString(" — bodies omitted; load_skill include_files=true shows them)\n")
	if len(t.Params) > 0 {
		b.WriteString("Params:\n")
		for _, p := range t.Params {
			var flags []string
			if p.Required {
				flags = append(flags, "required")
			}
			if p.Default != "" {
				flags = append(flags, fmt.Sprintf("default %q", p.Default))
			}
			if p.Pattern != "" {
				flags = append(flags, "pattern "+p.Pattern)
			}
			fmt.Fprintf(&b, "  - %s", p.Name)
			if len(flags) > 0 {
				fmt.Fprintf(&b, " (%s)", strings.Join(flags, ", "))
			}
			if p.Description != "" {
				b.WriteString(" — " + sanitizeDiffLine(p.Description))
			}
			b.WriteString("\n")
		}
	}
	writeEntry := func(indent string, f templateFile) {
		if f.isSnippet() {
			fmt.Fprintf(&b, "%s- snippet %s → add to %s (%d bytes, %d lines; returned, never written)\n", indent, f.Path, f.Target, len(f.Body), strings.Count(f.Body, "\n"))
			return
		}
		fmt.Fprintf(&b, "%s- %s (%s, %d bytes, %d lines)\n", indent, f.Path, f.Mode, len(f.Body), strings.Count(f.Body, "\n"))
	}
	if len(t.Templates) == 0 {
		b.WriteString("Files:\n")
		for _, f := range t.Files {
			writeEntry("  ", f)
		}
	} else {
		b.WriteString("Templates (choose any combination; requires is a hint only):\n")
		for _, d := range t.Templates {
			fmt.Fprintf(&b, "  [%s]", d.Name)
			if d.Description != "" {
				b.WriteString(" " + sanitizeDiffLine(d.Description))
			}
			if len(d.Requires) > 0 {
				b.WriteString(" — requires: " + strings.Join(d.Requires, ", "))
			}
			b.WriteString("\n")
			var mine []templateFile
			for _, f := range t.Files {
				if f.Template == d.Name {
					mine = append(mine, f)
				}
			}
			for _, f := range mine {
				writeEntry("      ", f)
			}
			var names []string
			for p := range referencedParams(mine) {
				names = append(names, p)
			}
			sort.Strings(names)
			if len(names) > 0 {
				fmt.Fprintf(&b, "      params used: %s\n", strings.Join(names, ", "))
			}
			for _, v := range d.Verify {
				b.WriteString("      verify: " + sanitizeDiffLine(v) + "\n")
			}
		}
	}
	if len(t.Verify) > 0 {
		b.WriteString("Verify after applying (run separately; each needs approval):\n")
		for _, v := range t.Verify {
			b.WriteString("  - " + sanitizeDiffLine(v) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// sha256Hex returns the hex sha256 of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// truncateRunes shortens s to at most n runes with an ellipsis.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
