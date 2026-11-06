package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/treeol/wakil/internal/memory"
)

// Legacy-store migration (0.5.0, C3 / D3). The legacy shared store is only
// read (memory.OpenReadOnly). An entry is copied into a project's store only
// when attribution is unambiguous: every anchor of the entry lies under exactly
// one known project directory. Entries with no anchors, anchors outside every
// known project, or anchors under two or more projects stay legacy. Nothing is
// deleted, and the legacy file is never written.
//
// Idempotency: the copied entries' legacy IDs are recorded in a sidecar file in
// the project's store directory (legacy-migrated.json), not in the legacy DB.

// MigrationPlan is the dry-run result for one legacy store.
type MigrationPlan struct {
	LegacyDB     string
	Total        int
	Unattributed int            // no attribution: stays legacy
	Ambiguous    int            // subset of Unattributed: evidence under 2+ projects
	SharedWriter int            // subset of Unattributed: writer recorded the shared store path
	AlreadyTotal int            // attributable but already copied (sidecar)
	PerProject   map[string]int // project dir -> entries to copy
	Already      map[string]int // project dir -> entries already migrated
	Moves        []MigrationMove
}

// MigrationMove is one legacy entry and the project it belongs to.
type MigrationMove struct {
	Project string
	Entry   *memory.Entry
}

// migrationMarkerFile is the sidecar that lists legacy entry IDs already copied.
const migrationMarkerFile = "legacy-migrated.json"

// attributeEntry returns the single project directory every anchor of e lies
// under, "" with ambiguous=true when anchors span several projects, and "" with
// ambiguous=false when there is no usable attribution.
func attributeEntry(e *memory.Entry, projects []string) (project string, ambiguous bool) {
	if len(e.Anchors) == 0 {
		return "", false
	}
	var owner string
	for _, a := range e.Anchors {
		// Only absolute anchors carry a project. A relative anchor is relative
		// to whichever workspace wrote it, so it cannot name one project; it
		// stays unattributed rather than being guessed (fail-safe).
		if !filepath.IsAbs(a.Path) {
			return "", false
		}
		full := filepath.Clean(a.Path)
		var hit string
		for _, p := range projects {
			rel, err := filepath.Rel(p, full)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			if hit != "" && hit != p {
				return "", true // one anchor under two projects (nested): ambiguous
			}
			hit = p
		}
		if hit == "" {
			return "", false // an anchor outside every known project: unattributed
		}
		if owner != "" && owner != hit {
			return "", true // anchors under two different projects
		}
		owner = hit
	}
	return owner, false
}

// sessionWorkspaces maps each saved session's chat ID to its recorded workspace
// (the project the chat ran in). An entry's SessionID is the writer's chat ID,
// so this is the primary attribution evidence: anchors are workspace-relative
// and cannot name a project on their own.
func sessionWorkspaces() map[string]string {
	out := map[string]string{}
	all, err := ListSessions()
	if err != nil {
		return out
	}
	for _, s := range all {
		if s.ChatID != "" && s.Workspace != "" {
			out[s.ChatID] = s.Workspace
		}
	}
	return out
}

// attributeByWriter returns the project of the session that wrote e, when that
// session's workspace is exactly one known project ("" otherwise).
//
// A recorded workspace equal to the legacy shared store's own path (legacyWS,
// the old WorkspacePath()) is not evidence: every 0.4.x session recorded it,
// so it names the shared store, not a project. Such entries fall back to
// anchors.
func attributeByWriter(e *memory.Entry, sessWS map[string]string, projects []string, legacyWS string) string {
	ws, ok := sessWS[e.SessionID]
	if !ok {
		return ""
	}
	if legacyWS != "" && sameWorkspace(ws, legacyWS) {
		return ""
	}
	for _, p := range projects {
		if sameWorkspace(ws, p) {
			return p
		}
	}
	return ""
}

// attributeFor is the full rule: writer session first, then absolute anchors.
func attributeFor(e *memory.Entry, sessWS map[string]string, projects []string, legacyWS string) (string, bool) {
	if p := attributeByWriter(e, sessWS, projects, legacyWS); p != "" {
		return p, false
	}
	return attributeEntry(e, projects)
}

// PlanLegacyMigration classifies every active legacy entry against the known
// project directories. It opens the legacy store read-only and writes nothing.
// alreadyMigrated maps project dir -> legacy IDs already copied (from sidecars).
//
// legacyWS is the legacy shared store's workspace path (see attributeByWriter).
func PlanLegacyMigration(ctx context.Context, legacyDB, legacyWS string, projects []string, alreadyMigrated map[string]map[int64]bool) (MigrationPlan, error) {
	plan := MigrationPlan{LegacyDB: legacyDB, PerProject: map[string]int{}, Already: map[string]int{}}
	ls, err := memory.OpenReadOnly(legacyDB, "")
	if err != nil {
		return plan, err
	}
	defer ls.Close()
	entries, err := ls.List(ctx, "", "", "active")
	if err != nil {
		return plan, fmt.Errorf("legacy list: %w", err)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i] < projects[j] })
	sessWS := sessionWorkspaces()
	plan.Total = len(entries)
	for _, e := range entries {
		proj, ambiguous := attributeFor(e, sessWS, projects, legacyWS)
		if proj == "" {
			plan.Unattributed++
			if ambiguous {
				plan.Ambiguous++
			}
			if ws, ok := sessWS[e.SessionID]; ok && legacyWS != "" && sameWorkspace(ws, legacyWS) {
				plan.SharedWriter++
			}
			continue
		}
		if alreadyMigrated[proj][e.ID] {
			plan.Already[proj]++
			plan.AlreadyTotal++
			continue
		}
		plan.PerProject[proj]++
		plan.Moves = append(plan.Moves, MigrationMove{Project: proj, Entry: e})
	}
	return plan, nil
}

// LoadMigrationMarkers reads the sidecar of one project store: the set of
// legacy IDs already copied. A missing sidecar is an empty set.
func LoadMigrationMarkers(project string) (map[int64]bool, error) {
	path := filepath.Join(filepath.Dir(MemoryDBPath(project)), migrationMarkerFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[int64]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []int64
	if err := json.Unmarshal(b, &ids); err != nil {
		return nil, fmt.Errorf("migration sidecar %s: %w", path, err)
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// ApplyLegacyMigration copies the planned entries into their project stores
// with PutActive and records their legacy IDs in each project's sidecar. It
// never deletes and never writes the legacy store. A failure stops the run;
// entries already copied stay copied and recorded.
func ApplyLegacyMigration(ctx context.Context, plan MigrationPlan) (copied int, err error) {
	byProject := map[string][]MigrationMove{}
	for _, m := range plan.Moves {
		byProject[m.Project] = append(byProject[m.Project], m)
	}
	projects := make([]string, 0, len(byProject))
	for p := range byProject {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	for _, proj := range projects {
		db := MemoryDBPath(proj)
		if db == "" {
			return copied, fmt.Errorf("no store path for %s", proj)
		}
		ps, oerr := memory.Open(db, proj)
		if oerr != nil {
			return copied, oerr
		}
		marked, merr := LoadMigrationMarkers(proj)
		if merr != nil {
			ps.Close()
			return copied, merr
		}
		for _, m := range byProject[proj] {
			e := m.Entry
			paths := make([]string, 0, len(e.Anchors))
			for _, a := range e.Anchors {
				paths = append(paths, a.Path)
			}
			if _, perr := ps.PutActive(ctx, e.Key, e.Value, e.Kind, e.Tier, e.Writer, e.SessionID, e.Tainted, e.ExpiresAt, paths, e.Note); perr != nil {
				ps.Close()
				return copied, fmt.Errorf("copy %s: %w", e.Key, perr)
			}
			marked[e.ID] = true
			copied++
		}
		ps.Close()
		if werr := writeMigrationMarkers(proj, marked); werr != nil {
			return copied, werr
		}
	}
	return copied, nil
}

func writeMigrationMarkers(project string, marked map[int64]bool) error {
	ids := make([]int64, 0, len(marked))
	for id := range marked {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	b, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(MemoryDBPath(project)), migrationMarkerFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
