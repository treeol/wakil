package agent

// skill_template_handlers.go — apply_skill_template, capture_skill_template,
// check_skill_template (skill-template Phases 1–3). Format and rendering live
// in skill_template.go; this file is the tool boundary: path confinement,
// preview + confirm, writes, and the workspace provenance file.
//
// Gating (Mashūra plan review):
//   - apply and capture are MAIN AGENT ONLY; the IsSubagent check runs before
//     any read or prompt (a tools-tier child's confirmer approves everything).
//   - Both are GatedTool AND carved out in SuspendAuto: apply writes skill
//     content (global, possibly tainted) and executable files into the
//     workspace; capture writes the global store. Neither is auto-approved.
//   - check is read-only and available to all tiers.
//   - Verify commands from a skill are surfaced, never executed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

const (
	// skillTemplateMainOnlyDenied: stable message for subagent calls.
	skillTemplateMainOnlyDenied = "ERROR: this skill template tool is main-agent only — subagents can inspect templates (load_skill, check_skill_template) but cannot apply or capture them."

	// provenancePath is the workspace-relative provenance file.
	provenancePath = tmplReservedDir + "/skill-templates.json"

	provenanceMaxBytes   = 1 << 20
	captureMaxFileBytes  = 64 * 1024
	captureMinValueBytes = 3
	applyDiffMaxBytes    = 4000
)

// ─── Provenance ─────────────────────────────────────────────────────────────

// provenanceRecord describes one file written by apply_skill_template.
type provenanceRecord struct {
	Path         string            `json:"path"` // workspace-relative, slash-separated
	Skill        string            `json:"skill"`
	SkillEntryID int64             `json:"skill_entry_id"`
	Params       map[string]string `json:"params,omitempty"`
	SHA256       string            `json:"sha256"`
	Mode         string            `json:"mode,omitempty"` // empty: executor did not manage modes
	AppliedAt    string            `json:"applied_at"`
}

type provenanceFile struct {
	Version int                `json:"version"`
	Files   []provenanceRecord `json:"files"`
}

// readProvenance loads the workspace provenance file. exists is false when
// it is absent; a present but corrupt file is an error (never silently
// replaced). raw is the file content for change detection.
func (a *App) readProvenance(ctx context.Context) (pf *provenanceFile, raw string, exists bool, err error) {
	canonical, err := a.provenanceDest(ctx)
	if err != nil {
		return nil, "", false, err
	}
	if err := a.probeRegular(ctx, canonical, provenancePath); err != nil {
		return nil, "", false, err
	}
	content, truncated, err := a.Exec.ReadFileBounded(ctx, canonical, provenanceMaxBytes)
	if err != nil {
		if isFileNotFoundError(err) {
			return &provenanceFile{Version: 1}, "", false, nil
		}
		return nil, "", false, fmt.Errorf("read %s: %w", provenancePath, err)
	}
	if truncated {
		return nil, "", true, fmt.Errorf("%s exceeds %d bytes", provenancePath, provenanceMaxBytes)
	}
	var f provenanceFile
	if err := json.Unmarshal([]byte(content), &f); err != nil {
		return nil, content, true, fmt.Errorf("%s is not valid JSON (fix or remove it): %w", provenancePath, err)
	}
	if f.Version != 1 {
		return nil, content, true, fmt.Errorf("%s has unsupported version %d", provenancePath, f.Version)
	}
	return &f, content, true, nil
}

// provenanceDest resolves the provenance file location. It must be exactly
// <workspace>/.wakil/skill-templates.json after resolution: a symlinked
// provenance file (or .wakil dir) could alias a template output or be
// retargeted during the confirm, so either is refused.
func (a *App) provenanceDest(ctx context.Context) (string, error) {
	canonical, err := a.confineForCreate(ctx, provenancePath)
	if err != nil {
		return "", err
	}
	root, err := a.Exec.ConfinePath(ctx, a.Exec.WorkspaceRoot())
	if err != nil {
		return "", err
	}
	if want := filepath.Join(root, filepath.FromSlash(provenancePath)); canonical != want {
		return "", fmt.Errorf("%s resolves to %s (symlinked) — refusing to use it as provenance; replace it with a regular file", provenancePath, canonical)
	}
	return canonical, nil
}

// probeRegular fails when canonical exists but is not a regular file, so
// FIFOs and devices are never opened. Missing files and executors without
// mode support pass (the subsequent read handles them).
func (a *App) probeRegular(ctx context.Context, canonical, label string) error {
	m, supported, err := a.fileModeOf(ctx, canonical)
	switch {
	case !supported, isFileNotFoundError(err):
		return nil
	case err != nil:
		return fmt.Errorf("%s: cannot inspect: %w", label, err)
	case m&(fs.ModeDir|fs.ModeIrregular) != 0:
		return fmt.Errorf("%s exists and is not a regular file", label)
	}
	return nil
}

// mergeProvenance replaces records for the given paths and keeps the rest.
func mergeProvenance(pf *provenanceFile, recs []provenanceRecord) *provenanceFile {
	byPath := map[string]provenanceRecord{}
	for _, r := range pf.Files {
		byPath[r.Path] = r
	}
	for _, r := range recs {
		byPath[r.Path] = r
	}
	out := &provenanceFile{Version: 1}
	for _, r := range byPath {
		out.Files = append(out.Files, r)
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out
}

// ─── Path helpers ───────────────────────────────────────────────────────────

// workspaceRel converts a canonical absolute path into a workspace-relative
// slash path (provenance keys must survive a moved workspace).
func (a *App) workspaceRel(canonical string) string {
	root := a.Exec.WorkspaceRoot()
	if rel, err := filepath.Rel(root, canonical); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(canonical)
}

// confineForCreate confines a path that may not exist yet, including missing
// parent directories. DockerExecutor's `readlink -f` fails when more than the
// last component is missing, so on failure walk up to the deepest ancestor
// that ConfinePath accepts — but only through components that do NOT exist
// (an existing component that fails confinement is a real rejection, e.g. a
// symlink out of the workspace) — and re-join the missing tail. Missing
// components cannot be symlinks, so the joined path stays where the
// ancestor resolved.
func (a *App) confineForCreate(ctx context.Context, p string) (string, error) {
	canonical, err := a.Exec.ConfinePath(ctx, p)
	if err == nil {
		return canonical, nil
	}
	origErr := err
	cur := path.Clean(filepath.ToSlash(p))
	// Never climb through "..": a ".." component is not a missing entry, and
	// re-joining it would escape the confined ancestor.
	for _, seg := range strings.Split(cur, "/") {
		if seg == ".." {
			return "", origErr
		}
	}
	rootCanon, rerr := a.Exec.ConfinePath(ctx, a.Exec.WorkspaceRoot())
	if rerr != nil {
		return "", origErr
	}
	var tail []string
	for {
		// "Missing" must be decided WITHOUT following symlinks: StatFile
		// follows them, so a dangling link would look absent and be climbed
		// past. A directory listing shows the link itself as an entry.
		if !a.entryAbsent(ctx, cur) {
			return "", origErr // exists (or unknown): the original rejection stands
		}
		parent := path.Dir(cur)
		if parent == cur {
			return "", origErr
		}
		tail = append([]string{path.Base(cur)}, tail...)
		cur = parent
		if c, cerr := a.Exec.ConfinePath(ctx, cur); cerr == nil {
			joined := filepath.Join(append([]string{c}, tail...)...)
			if !isWithin(joined, rootCanon) {
				return "", origErr // defense in depth: must stay in the workspace
			}
			return joined, nil
		}
	}
}

// entryAbsent reports whether p provably has no directory entry, using the
// executor's structured lstat-style probe (a dangling or escaping symlink is
// an entry). Executors without the probe, and any probe error, count as
// present — the caller then keeps the original confinement rejection.
func (a *App) entryAbsent(ctx context.Context, p string) bool {
	fm, ok := a.Exec.(exec.FileModer)
	if !ok {
		return false
	}
	// FileModer takes absolute paths (Docker probes run without a cd).
	if !path.IsAbs(p) {
		p = path.Join(filepath.ToSlash(a.Exec.WorkspaceRoot()), p)
	}
	exists, err := fm.EntryExists(ctx, p)
	return err == nil && !exists
}

// isWithin reports whether p is dir or nested inside it (both cleaned, abs).
func isWithin(p, dir string) bool {
	p, dir = filepath.Clean(p), filepath.Clean(dir)
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// fileModeOf returns the executor-reported mode (perm bits + type indicator,
// see exec.FileModer). supported=false when the executor cannot report modes;
// with supported=true every probe failure is returned as an error — an
// unknown mode must never be treated as "unchanged".
func (a *App) fileModeOf(ctx context.Context, canonical string) (m fs.FileMode, supported bool, err error) {
	fm, ok := a.Exec.(exec.FileModer)
	if !ok {
		return 0, false, nil
	}
	m, err = fm.FileMode(ctx, canonical)
	return m, true, err
}

// isReservedDest reports whether a canonical destination lies inside the
// workspace's reserved .wakil/ directory (provenance, workflow plan). Checked
// on the resolved path so neither target_dir nor an in-workspace symlink
// alias can route a template file there.
func (a *App) isReservedDest(ctx context.Context, canonical string) bool {
	reserved, err := a.confineForCreate(ctx, tmplReservedDir)
	if err != nil {
		return true // cannot prove it is outside — refuse
	}
	return isWithin(canonical, reserved)
}

func formatMode(m fs.FileMode) string { return fmt.Sprintf("%04o", uint32(m.Perm())) }

func parseMode(s string) fs.FileMode {
	v, _ := strconv.ParseUint(s, 8, 32) // validated by tmplModeRE upstream
	return fs.FileMode(v).Perm()
}

// ─── apply_skill_template ───────────────────────────────────────────────────

// applyTarget is the planned action for one rendered file.
type applyTarget struct {
	renderedFile
	canonical string
	exists    bool
	oldBody   string
	oldMode   string // "" when unknown
	action    string // "new", "overwrite", "mode", "unchanged"
}

// planApplyTargets resolves and inspects every rendered file. It is called
// twice: for the preview, and again after confirmation to detect changes.
func (a *App) planApplyTargets(ctx context.Context, targetDir string, files []renderedFile) ([]applyTarget, error) {
	dirCanon, err := a.confineForCreate(ctx, targetDir)
	if err != nil {
		return nil, fmt.Errorf("target_dir: %w", err)
	}
	out := make([]applyTarget, 0, len(files))
	byCanon := map[string]string{}
	for _, f := range files {
		canonical, err := a.confineForCreate(ctx, path.Join(targetDir, f.Path))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		// Canonical collisions: distinct template paths can resolve to the
		// same file (or file vs. parent) through an in-workspace symlink.
		if other, dup := byCanon[canonical]; dup {
			return nil, fmt.Errorf("%s and %s resolve to the same file %s", other, f.Path, canonical)
		}
		for c, other := range byCanon {
			if isWithin(canonical, c) || isWithin(c, canonical) {
				return nil, fmt.Errorf("%s and %s resolve to a file and its parent directory", other, f.Path)
			}
		}
		byCanon[canonical] = f.Path
		if !isWithin(canonical, dirCanon) || canonical == dirCanon {
			return nil, fmt.Errorf("%s resolves to %s, outside target_dir %s", f.Path, canonical, dirCanon)
		}
		if a.isReservedDest(ctx, canonical) {
			return nil, fmt.Errorf("%s resolves into %s/, which is reserved (provenance and workflow files)", f.Path, tmplReservedDir)
		}
		t := applyTarget{renderedFile: f, canonical: canonical}
		// Type/mode probe first, so FIFOs and devices are never opened.
		m, supported, merr := a.fileModeOf(ctx, canonical)
		switch {
		case !supported:
		case merr == nil && m&(fs.ModeDir|fs.ModeIrregular) != 0:
			return nil, fmt.Errorf("%s exists and is not a regular file", f.Path)
		case merr == nil:
			t.oldMode = formatMode(m)
		case !isFileNotFoundError(merr):
			return nil, fmt.Errorf("%s: cannot inspect existing target: %w", f.Path, merr)
		}
		body, truncated, err := a.Exec.ReadFileBounded(ctx, canonical, tmplMaxRenderedSize+1)
		switch {
		case err == nil && truncated:
			// Larger than any renderable file: certainly different.
			t.exists, t.oldBody = true, body
			t.oldBody += "\x00(truncated)"
		case err == nil:
			t.exists, t.oldBody = true, body
		case isFileNotFoundError(err):
		default:
			return nil, fmt.Errorf("%s: cannot inspect existing target: %w", f.Path, err)
		}
		if t.exists && supported && t.oldMode == "" {
			return nil, fmt.Errorf("%s: appeared while being inspected — retry", f.Path)
		}
		switch {
		case !t.exists:
			t.action = "new"
		case t.oldBody != f.Content:
			t.action = "overwrite"
		case !supported:
			// Policy for executors without FileModer: modes are not managed
			// at all — bytes alone decide, the result says modes were not
			// applied, and provenance records no mode.
			t.action = "unchanged"
		case t.oldMode != f.Mode:
			t.action = "mode"
		default:
			t.action = "unchanged"
		}
		out = append(out, t)
	}
	return out, nil
}

// handleApplySkillTemplate renders a template skill into the workspace.
func (a *App) handleApplySkillTemplate(ctx context.Context, tc proxy.ToolCall) string {
	if a.IsSubagent {
		return skillTemplateMainOnlyDenied
	}
	s := a.getSkillStore()
	if s == nil {
		return skillsUnavailable
	}
	var args struct {
		Key       string            `json:"key"`
		Params    map[string]string `json:"params"`
		TargetDir string            `json:"target_dir"`
		Overwrite bool              `json:"overwrite"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.TargetDir == "" {
		args.TargetDir = "."
	}
	if strings.IndexFunc(args.TargetDir, isControlRune) >= 0 {
		return "ERROR: target_dir contains control characters"
	}
	entry, err := s.getActiveSkill(ctx, args.Key)
	if errors.Is(err, memory.ErrNotFound) {
		return "ERROR: skill not found: " + args.Key
	} else if err != nil {
		return fmt.Sprintf("ERROR: load skill: %v", err)
	}
	tmpl, err := parseSkillTemplateValue(entry.Value)
	if err != nil {
		return fmt.Sprintf("ERROR: skill %q has an invalid template section: %v", args.Key, err)
	}
	if tmpl == nil {
		return fmt.Sprintf("ERROR: skill %q has no template files — load_skill it and follow the prose instead", args.Key)
	}
	values, err := resolveTemplateParams(tmpl, args.Params)
	if err != nil {
		return "ERROR: " + err.Error() + "\n\n" + summarizeSkillTemplate(tmpl)
	}
	files, err := renderTemplate(tmpl, values)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	targets, err := a.planApplyTargets(ctx, args.TargetDir, files)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	var conflicts []string
	writes := 0
	for _, t := range targets {
		if t.action == "overwrite" || t.action == "mode" {
			conflicts = append(conflicts, t.Path)
		}
		if t.action != "unchanged" {
			writes++
		}
	}
	if len(conflicts) > 0 && !args.Overwrite {
		return fmt.Sprintf("ERROR: %d existing file(s) differ from the template: %s — nothing written. Re-run with overwrite=true to replace them (the confirm shows a diff), or apply into another target_dir.", len(conflicts), strings.Join(conflicts, ", "))
	}
	if writes == 0 {
		return fmt.Sprintf("no-op: all %d file(s) of skill %q already match the template — nothing written", len(targets), args.Key)
	}
	pf, provRaw, _, err := a.readProvenance(ctx)
	if err != nil {
		return "ERROR: " + err.Error() + " — nothing written"
	}
	provCanon, err := a.provenanceDest(ctx)
	if err != nil {
		return "ERROR: " + err.Error() + " — nothing written"
	}
	_, canChmod := a.Exec.(exec.FileModer)
	now := time.Now().UTC().Format(time.RFC3339)
	// Preflight: the merged provenance (all files recorded) must stay within
	// the read limit, or a successful apply would make the file unreadable.
	allRecs := make([]provenanceRecord, len(targets))
	for i, t := range targets {
		allRecs[i] = provenanceRecordFor(a, t, args.Key, entry.ID, values, now, canChmod)
	}
	if data, err := json.MarshalIndent(mergeProvenance(pf, allRecs), "", "  "); err != nil || len(data)+1 > provenanceMaxBytes {
		return fmt.Sprintf("ERROR: %s would exceed %d bytes — prune old records first; nothing written", provenancePath, provenanceMaxBytes)
	}

	detail := a.applyPreview(entry, args.TargetDir, values, tmpl, targets)
	if !a.Confirm("apply_skill_template", fmt.Sprintf("Apply template skill %q (%d file write(s))?", args.Key, writes), detail, false) {
		return "[declined by user]"
	}

	// Change detection: the workspace and the skill must still match what the
	// user approved. This narrows (does not close) the check-to-write gap —
	// the same guarantee write_file gives.
	if cur, err := s.getActiveSkill(ctx, args.Key); err != nil || cur.ID != entry.ID {
		return "ERROR: skill changed while the confirm was open — nothing written; re-run apply_skill_template"
	}
	again, err := a.planApplyTargets(ctx, args.TargetDir, files)
	if err != nil {
		return "ERROR: " + err.Error() + " — nothing written"
	}
	for i := range again {
		// Destination identity too: a directory symlink retargeted during the
		// confirm must not redirect the approved writes.
		if again[i].canonical != targets[i].canonical || again[i].exists != targets[i].exists ||
			again[i].oldBody != targets[i].oldBody || again[i].oldMode != targets[i].oldMode {
			return fmt.Sprintf("ERROR: %s changed while the confirm was open — nothing written; re-run apply_skill_template", targets[i].Path)
		}
	}
	if c, err := a.provenanceDest(ctx); err != nil || c != provCanon {
		return "ERROR: " + provenancePath + " changed location while the confirm was open — nothing written; re-run apply_skill_template"
	}
	if _, raw, _, err := a.readProvenance(ctx); err != nil || raw != provRaw {
		return "ERROR: " + provenancePath + " changed while the confirm was open — nothing written; re-run apply_skill_template"
	}

	// Per-file outcome. A file whose content was written but whose chmod
	// failed is reported as such (it IS modified) and is not recorded in
	// provenance (it does not match the template).
	status := make([]string, len(targets))
	var recs []provenanceRecord
	var failure string
	fm, _ := a.Exec.(exec.FileModer)
	for i, t := range targets {
		if failure != "" {
			status[i] = "not attempted"
			continue
		}
		if t.action == "unchanged" {
			status[i] = "unchanged"
			recs = append(recs, allRecs[i])
			continue
		}
		a.captureFileOriginal(ctx, t.canonical)
		contentWritten := false
		if t.action != "mode" {
			if _, err := a.Exec.WriteFile(ctx, t.canonical, t.Content); err != nil {
				// WriteFile may truncate before failing: treat the file as
				// possibly modified (it is in the /rewind checkpoint).
				status[i] = "FAILED (write error — may be partially written)"
				a.recordFileChanged(t.canonical)
				failure = fmt.Sprintf("%s: %v", t.Path, err)
				continue
			}
			contentWritten = true
		}
		if canChmod {
			if err := fm.Chmod(ctx, t.canonical, parseMode(t.Mode)); err != nil {
				failure = fmt.Sprintf("%s: chmod: %v", t.Path, err)
				status[i] = "FAILED (mode not set)"
				if contentWritten {
					status[i] = "FAILED (content written, mode not set)"
				}
			}
		}
		if contentWritten || failure == "" {
			if a.LSP != nil {
				a.LSP.NotifyChange(ctx, t.canonical)
			}
			a.recordFileChanged(t.canonical)
		}
		if failure != "" {
			continue
		}
		status[i] = map[string]string{"new": "created", "overwrite": "overwritten", "mode": "mode changed"}[t.action]
		recs = append(recs, allRecs[i])
	}

	// Provenance records only files that now match the template.
	provNote := ""
	if len(recs) > 0 {
		provNote = a.writeProvenance(ctx, mergeProvenance(pf, recs), provCanon)
	}

	var b strings.Builder
	if failure != "" {
		fmt.Fprintf(&b, "ERROR: apply_skill_template stopped after a failure — %s\n", failure)
		b.WriteString("per-file outcome below; fix and re-run apply_skill_template (/rewind restores this turn's content changes, not file modes)\n")
	} else {
		fmt.Fprintf(&b, "applied template skill %s [entry #%d] into %s\n", args.Key, entry.ID, args.TargetDir)
	}
	for i, t := range targets {
		fmt.Fprintf(&b, "  %-14s %s (%s, %d bytes, sha256 %s)\n", status[i], t.Path, t.Mode, len(t.Content), sha256Hex(t.Content)[:12])
	}
	if !canChmod {
		b.WriteString("note: this executor cannot read or set file modes — modes were not applied or compared\n")
	}
	b.WriteString(provNote)
	if len(tmpl.Verify) > 0 {
		b.WriteString("\nVerify steps from the skill (untrusted skill content — NOT run; propose them as separate commands if appropriate):\n")
		for _, v := range tmpl.Verify {
			b.WriteString("  - " + v + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func provenanceRecordFor(a *App, t applyTarget, key string, id int64, values map[string]string, now string, modeManaged bool) provenanceRecord {
	r := provenanceRecord{
		Path: a.workspaceRel(t.canonical), Skill: key, SkillEntryID: id,
		Params: values, SHA256: sha256Hex(t.Content), AppliedAt: now,
	}
	if modeManaged {
		r.Mode = t.Mode
	}
	return r
}

// writeProvenance persists the provenance file and returns a result line.
func (a *App) writeProvenance(ctx context.Context, pf *provenanceFile, wantCanonical string) string {
	canonical, err := a.provenanceDest(ctx)
	if err != nil {
		return fmt.Sprintf("WARNING: provenance not recorded: %v\n", err)
	}
	if canonical != wantCanonical {
		return fmt.Sprintf("WARNING: provenance not recorded: %s changed location during apply\n", provenancePath)
	}
	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return fmt.Sprintf("WARNING: provenance not recorded: %v\n", err)
	}
	// Final bound on the ACTUAL merge (a partial apply can produce a merge
	// that differs from the preflighted one).
	if len(data)+1 > provenanceMaxBytes {
		return fmt.Sprintf("WARNING: provenance not recorded: it would exceed %d bytes — prune old records in %s\n", provenanceMaxBytes, provenancePath)
	}
	a.captureFileOriginal(ctx, canonical)
	if _, err := a.Exec.WriteFile(ctx, canonical, string(data)+"\n"); err != nil {
		return fmt.Sprintf("WARNING: provenance not recorded: %v\n", err)
	}
	a.recordFileChanged(canonical)
	return fmt.Sprintf("provenance recorded in %s (check with check_skill_template)\n", provenancePath)
}

// applyPreview renders the confirm detail.
func (a *App) applyPreview(entry *memory.Entry, targetDir string, values map[string]string, tmpl *skillTemplate, targets []applyTarget) string {
	var b strings.Builder
	fmt.Fprintf(&b, "skill: %s [entry #%d, %s]\n", entry.Key, entry.ID, renderSkillProvenance(entry))
	fmt.Fprintf(&b, "target_dir: %s (%s)\n", targetDir, a.Exec.Describe())
	if len(values) > 0 {
		b.WriteString("params:\n")
		names := make([]string, 0, len(values))
		for k := range values {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(&b, "  %s = %q\n", k, values[k])
		}
	}
	b.WriteString("files:\n")
	for _, t := range targets {
		extra := ""
		switch t.action {
		case "overwrite":
			extra = " — REPLACES existing content"
		case "mode":
			extra = fmt.Sprintf(" — mode %s → %s (content identical)", t.oldMode, t.Mode)
		}
		exe := ""
		if parseMode(t.Mode)&0o111 != 0 {
			exe = " executable"
		}
		fmt.Fprintf(&b, "  [%s] %s (%s%s, %d bytes, sha256 %s)%s\n", t.action, t.Path, t.Mode, exe, len(t.Content), sha256Hex(t.Content)[:12], extra)
	}
	fmt.Fprintf(&b, "  [record] %s\n", provenancePath)
	diffBudget := applyDiffMaxBytes
	for _, t := range targets {
		if t.action != "overwrite" || diffBudget <= 0 {
			continue
		}
		d := skillLineDiff(t.oldBody, t.Content)
		if len(d) > diffBudget {
			d = d[:diffBudget] + "\n… (diff truncated)"
		}
		diffBudget -= len(d)
		fmt.Fprintf(&b, "\n--- diff %s (existing → template) ---\n%s\n", t.Path, d)
	}
	if len(tmpl.Verify) > 0 {
		b.WriteString("\nverify steps (shown only — NOT run):\n")
		for _, v := range tmpl.Verify {
			b.WriteString("  - " + sanitizeDiffLine(v) + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ─── capture_skill_template ─────────────────────────────────────────────────

// captureParam is one param supplied to capture_skill_template.
type captureParam struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
	Default     string `json:"default,omitempty"`
	Pattern     string `json:"pattern,omitempty"`
}

// parameterize replaces literal values with @@NAME@@ tokens in one
// left-to-right scan over the ORIGINAL text: at each offset the longest
// matching value wins, and inserted tokens are never rescanned. counts
// reports replacements per param name.
func parameterize(text string, params []captureParam, counts map[string]int) string {
	ordered := append([]captureParam(nil), params...)
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i].Value) > len(ordered[j].Value) })
	var b strings.Builder
	for i := 0; i < len(text); {
		matched := false
		for _, p := range ordered {
			if strings.HasPrefix(text[i:], p.Value) {
				b.WriteString("@@" + p.Name + "@@")
				counts[p.Name]++
				i += len(p.Value)
				matched = true
				break
			}
		}
		if !matched {
			b.WriteByte(text[i])
			i++
		}
	}
	return b.String()
}

// handleCaptureSkillTemplate reads workspace files and stores them as the
// template section of a skill (create or replace-section update).
func (a *App) handleCaptureSkillTemplate(ctx context.Context, tc proxy.ToolCall) string {
	if a.IsSubagent {
		return skillTemplateMainOnlyDenied
	}
	s := a.getSkillStore()
	if s == nil {
		return skillsUnavailable
	}
	var args struct {
		Key         string            `json:"key"`
		Paths       []string          `json:"paths"`
		BaseDir     string            `json:"base_dir"`
		Params      []captureParam    `json:"params"`
		Modes       map[string]string `json:"modes"`
		Verify      []string          `json:"verify"`
		Description string            `json:"description"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if err := validateSkillKey(args.Key); err != nil {
		return "ERROR: " + err.Error()
	}
	if len(args.Paths) == 0 {
		return "ERROR: paths is required (files to capture, relative to base_dir)"
	}
	if args.BaseDir == "" {
		args.BaseDir = "."
	}
	if strings.IndexFunc(args.BaseDir, isControlRune) >= 0 {
		return "ERROR: base_dir contains control characters"
	}
	seenVal := map[string]string{}
	for _, p := range args.Params {
		if len(p.Value) < captureMinValueBytes {
			return fmt.Sprintf("ERROR: param %s: value must be at least %d bytes (short values match unrelated text)", p.Name, captureMinValueBytes)
		}
		if err := validateParamValue(p.Value); err != nil {
			return fmt.Sprintf("ERROR: param %s: %v", p.Name, err)
		}
		if other, dup := seenVal[p.Value]; dup {
			return fmt.Sprintf("ERROR: params %s and %s have the same value", other, p.Name)
		}
		seenVal[p.Value] = p.Name
		// The captured value must itself be valid, or the template could not
		// be re-applied with its own original setup.
		if p.Pattern != "" {
			re, err := templateParam{Pattern: p.Pattern}.patternRE()
			if err != nil {
				return fmt.Sprintf("ERROR: param %s: invalid pattern: %v", p.Name, err)
			}
			if !re.MatchString(p.Value) {
				return fmt.Sprintf("ERROR: param %s: captured value %q does not fully match its pattern %s", p.Name, p.Value, p.Pattern)
			}
		}
	}

	baseCanon, err := a.Exec.ConfinePath(ctx, args.BaseDir)
	if err != nil {
		return "ERROR: base_dir: " + err.Error()
	}
	counts := map[string]int{}
	tmpl := &skillTemplate{Verify: args.Verify}
	for _, p := range args.Params {
		tmpl.Params = append(tmpl.Params, templateParam{Name: p.Name, Description: p.Description, Default: p.Default, Required: p.Default == "", Pattern: p.Pattern})
	}
	for _, rel := range args.Paths {
		canonical, err := a.Exec.ConfinePath(ctx, path.Join(args.BaseDir, rel))
		if err != nil {
			return fmt.Sprintf("ERROR: %s: %v", rel, err)
		}
		if !isWithin(canonical, baseCanon) || canonical == baseCanon {
			return fmt.Sprintf("ERROR: %s resolves outside base_dir %s", rel, args.BaseDir)
		}
		m, modeKnown, merr := a.fileModeOf(ctx, canonical)
		if modeKnown && merr != nil {
			return fmt.Sprintf("ERROR: %s: %v", rel, merr)
		}
		if modeKnown && m&(fs.ModeDir|fs.ModeIrregular) != 0 {
			return fmt.Sprintf("ERROR: %s is not a regular file", rel)
		}
		body, truncated, err := a.Exec.ReadFileBounded(ctx, canonical, captureMaxFileBytes)
		if err != nil {
			return fmt.Sprintf("ERROR: read %s: %v", rel, err)
		}
		if truncated {
			return fmt.Sprintf("ERROR: %s exceeds %d bytes — too large for a template file", rel, captureMaxFileBytes)
		}
		if tmplTokenRE.MatchString(body) {
			return fmt.Sprintf("ERROR: %s already contains @@NAME@@ tokens — capture would make them ambiguous", rel)
		}
		relPath, _ := filepath.Rel(baseCanon, canonical)
		relPath = filepath.ToSlash(relPath)
		mode := args.Modes[rel]
		if mode == "" {
			if modeKnown {
				mode = formatMode(m)
			} else if strings.HasPrefix(body, "#!") {
				mode = "0755"
			} else {
				mode = tmplDefaultMode
			}
		}
		tmpl.Files = append(tmpl.Files, templateFile{
			Path: parameterize(relPath, args.Params, counts),
			Mode: mode,
			Body: parameterize(body, args.Params, counts),
		})
	}
	var unused []string
	for _, p := range args.Params {
		if counts[p.Name] == 0 {
			unused = append(unused, p.Name)
		}
	}
	if len(unused) > 0 {
		return fmt.Sprintf("ERROR: param value(s) for %s do not occur in the captured files", strings.Join(unused, ", "))
	}
	section, err := serializeTemplateSection(tmpl)
	if err != nil {
		return "ERROR: " + err.Error()
	}

	existing, err := s.getActiveSkill(ctx, args.Key)
	isUpdate := err == nil
	if err != nil && !errors.Is(err, memory.ErrNotFound) {
		return fmt.Sprintf("ERROR: load skill: %v", err)
	}
	var value string
	if isUpdate {
		// Keep the existing prose byte-for-byte. A skill that already has a
		// section keeps exactly the bytes before it; a plain skill gets the
		// section appended after a blank line.
		prose, _, hadSection := splitSkillTemplate(existing.Value)
		switch {
		case hadSection:
			value = prose + section
		case strings.HasSuffix(prose, "\n\n"):
			value = prose + section
		case strings.HasSuffix(prose, "\n"):
			value = prose + "\n" + section
		default:
			value = prose + "\n\n" + section
		}
	} else {
		if strings.TrimSpace(args.Description) == "" {
			return "ERROR: description is required when creating a new skill (it becomes the first line shown by list_skills)"
		}
		value = args.Description + "\n\nTemplate captured from " + args.BaseDir + ". Apply with apply_skill_template.\n\n" + section
	}
	if err := validateSkillValue(value); err != nil {
		return "ERROR: " + err.Error()
	}
	if _, err := parseSkillTemplateValue(value); err != nil {
		return "ERROR: captured template does not round-trip: " + err.Error()
	}
	if isUpdate && value == existing.Value {
		return fmt.Sprintf("no-op: skill %q already holds exactly this template", args.Key)
	}
	if msg := screenSkillSecrets(value); msg != "" {
		return "ERROR: " + msg
	}

	var d strings.Builder
	fmt.Fprintf(&d, "Size: %d bytes\nTainted: %s\n\nFiles:\n", len(value), taintLabel(a.computeTainted()))
	for _, f := range tmpl.Files {
		fmt.Fprintf(&d, "  %s (%s, %d bytes)\n", f.Path, f.Mode, len(f.Body))
	}
	if len(args.Params) > 0 {
		d.WriteString("Params (captured values replaced by @@NAME@@; values themselves are not stored):\n")
		for _, p := range args.Params {
			fmt.Fprintf(&d, "  %s ← %q: %d replacement(s)\n", p.Name, p.Value, counts[p.Name])
		}
	}
	headline := fmt.Sprintf("Save new template skill %q to the global store?", args.Key)
	if isUpdate {
		headline = fmt.Sprintf("Replace the template files of skill %q (prose kept; old version in history)?", args.Key)
		fmt.Fprintf(&d, "\nDiff (old → new):\n%s", skillLineDiff(existing.Value, value))
	} else {
		fmt.Fprintf(&d, "\nContent preview:\n%s", previewSkillValue(value))
	}
	if !a.Confirm("capture_skill_template", headline, d.String(), false) {
		return "[declined by user]"
	}
	if isUpdate {
		if cur, err := s.getActiveSkill(ctx, args.Key); err != nil || cur.ID != existing.ID {
			return "ERROR: skill changed while the confirm was open — nothing saved; re-run capture_skill_template"
		}
	}
	e, err := s.putActiveSkill(ctx, args.Key, value, a.AgentPrefix, a.chatID(), a.computeTainted(), isUpdate, "")
	if err != nil {
		return fmt.Sprintf("ERROR: save skill: %v", err)
	}
	verb := "saved"
	if isUpdate {
		verb = "updated"
	}
	return fmt.Sprintf("%s template skill: %s [id: %d] — %d file(s), %d param(s)\n%s", verb, e.Key, e.ID, len(tmpl.Files), len(tmpl.Params), renderSkillProvenance(e))
}

// ─── check_skill_template ───────────────────────────────────────────────────

// handleCheckSkillTemplate reports local modification and template drift for
// files recorded in the provenance file. Read-only.
func (a *App) handleCheckSkillTemplate(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Path string `json:"path"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	pf, _, exists, err := a.readProvenance(ctx)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	if !exists || len(pf.Files) == 0 {
		return "(no template provenance in this workspace — " + provenancePath + " does not exist or is empty)"
	}
	wantPath := ""
	if args.Path != "" {
		c, err := a.Exec.ConfinePath(ctx, args.Path)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		wantPath = a.workspaceRel(c)
	}
	s := a.getSkillStore()
	var b strings.Builder
	n := 0
	for _, r := range pf.Files {
		if (wantPath != "" && r.Path != wantPath) || (args.Key != "" && r.Skill != args.Key) {
			continue
		}
		n++
		fmt.Fprintf(&b, "%s ← %s #%d (applied %s)\n", r.Path, r.Skill, r.SkillEntryID, r.AppliedAt)
		b.WriteString("  file: " + a.checkRecordedFile(ctx, r) + "\n")
		b.WriteString("  template: " + checkRecordedSkill(ctx, s, r) + "\n")
	}
	if n == 0 {
		return "(no provenance records match that path/key)"
	}
	return strings.TrimRight(b.String(), "\n")
}

func (a *App) checkRecordedFile(ctx context.Context, r provenanceRecord) string {
	if err := validateTemplatePath(r.Path); err != nil {
		return "INVALID record (" + err.Error() + ")"
	}
	canonical, err := a.confineForCreate(ctx, r.Path)
	if err != nil {
		return "INVALID record (" + err.Error() + ")"
	}
	if err := a.probeRegular(ctx, canonical, r.Path); err != nil {
		return "unreadable (" + err.Error() + ")"
	}
	body, truncated, err := a.Exec.ReadFileBounded(ctx, canonical, tmplMaxRenderedSize)
	switch {
	case isFileNotFoundError(err):
		return "missing"
	case err != nil:
		return "unreadable (" + err.Error() + ")"
	case truncated || sha256Hex(body) != r.SHA256:
		return "locally modified since apply"
	}
	if r.Mode != "" {
		m, supported, err := a.fileModeOf(ctx, canonical)
		switch {
		case !supported:
			return "content unchanged (mode not checkable with this executor)"
		case err != nil:
			return "content unchanged, mode unknown (" + err.Error() + ")"
		case formatMode(m) != r.Mode:
			return fmt.Sprintf("content unchanged, mode changed (%s → %s)", r.Mode, formatMode(m))
		}
	}
	return "unchanged since apply"
}

func checkRecordedSkill(ctx context.Context, s *skillsProfile, r provenanceRecord) string {
	if s == nil {
		return "unknown (skill store unavailable)"
	}
	if err := validateSkillKey(r.Skill); err != nil {
		return "INVALID record (" + err.Error() + ")"
	}
	cur, err := s.getActiveSkill(ctx, r.Skill)
	switch {
	case errors.Is(err, memory.ErrNotFound):
		return "skill no longer exists (forgotten)"
	case err != nil:
		return "unknown (" + err.Error() + ")"
	case cur.ID == r.SkillEntryID:
		return "current (no newer version)"
	}
	return fmt.Sprintf("newer skill version exists (#%d → #%d) — re-apply with the recorded params to update (overwrite=true shows the diff)", r.SkillEntryID, cur.ID)
}
