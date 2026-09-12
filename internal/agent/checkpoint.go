package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
)

// FileSnapshot captures the pre-mutation state of a single file for checkpoint
// restore. Exists=true means the file existed before the mutation; content holds
// its bytes. Exists=false means the file did not exist (so rewind should delete it).
// TooLarge=true means the file exceeded the per-file size limit and its content
// was not captured — rewind cannot restore it and must warn.
type FileSnapshot struct {
	Exists   bool
	Content  []byte
	TooLarge bool
	// Unknown is true when the file's state could not be determined (read
	// error other than "not found", stat error, etc.). Rewind skips these
	// with a warning — never deletes them.
	Unknown bool
}

// Checkpoint captures workspace state at the start of a user turn. Files is
// populated copy-on-first-write as mutating tools run during the turn: each
// file's snapshot reflects its state BEFORE the first mutation in this turn.
// HadShellCmd is set when a non-read-only shell command or background process
// was executed during this turn — rewind warns that shell side effects are not
// reliably reverted.
type Checkpoint struct {
	TurnIndex   int                    // 1-based user turn index (for display)
	ConvLen     int                    // len(a.Conv) at checkpoint creation
	Files       map[string]FileSnapshot // canonical path → pre-mutation state
	HadShellCmd bool                   // turn included non-read-only shell/background
	CreatedAt   time.Time
	Generation  int64                  // monotonically increasing ID for capture identity checks
}

const (
	// checkpointMaxKeep bounds the number of checkpoints retained. Older
	// checkpoints are evicted (FIFO) — the earliest rewindable point may not
	// be the session start.
	checkpointMaxKeep = 20
	// checkpointMaxFileSize bounds the size of a single file snapshot (1 MB).
	// Files exceeding this are marked TooLarge and not captured.
	checkpointMaxFileSize = 1 << 20
	// checkpointMaxTotalBytes bounds the total memory across all checkpoints
	// (50 MB). When exceeded, the oldest checkpoint is evicted.
	checkpointMaxTotalBytes = 50 << 20
)

// checkpointState holds the in-memory checkpoint stack, embedded in App.
// Checkpoints are NOT persisted — they are cleared on session rotation,
// compaction, and resume. They exist only for the current session's undo.
type checkpointState struct {
	cpMu           sync.Mutex
	checkpoints    []Checkpoint
	cpTurnCount    int  // total user turns this session (for display)
	cpActive       bool // true when a checkpoint is active for the current turn
	cpTotalBytes   int  // approximate total bytes across all checkpoints
	cpRewinding    bool // true while rewind file I/O is in progress — blocks new turns and captures
	cpGenCounter   int64 // monotonically increasing generation ID for checkpoint identity
}

// startCheckpoint begins a new checkpoint for the current turn. Called from
// SendOutcome after the user message is appended to Conv but before the tool
// loop runs. The checkpoint's ConvLen captures the conversation length at this
// point, so rewind can truncate Conv back to just before the assistant's
// response.
//
// Safe to call when no checkpoint system is active (subagents, tests) — it's
// a no-op for IsSubagent Apps. Returns false if a rewind is in progress
// (the caller should warn the user that the turn will not be checkpointed).
func (a *App) startCheckpoint() bool {
	if a.IsSubagent {
		return false
	}
	a.cpMu.Lock()
	defer a.cpMu.Unlock()

	// Refuse to start a new checkpoint while a rewind is in progress —
	// file restores and conversation truncation would race the new turn.
	if a.cpRewinding {
		return false
	}

	a.cpTurnCount++
	a.cpGenCounter++
	cp := Checkpoint{
		TurnIndex:  a.cpTurnCount,
		ConvLen:    len(a.Conv),
		Files:      make(map[string]FileSnapshot),
		CreatedAt:  time.Now(),
		Generation: a.cpGenCounter,
	}
	a.checkpoints = append(a.checkpoints, cp)
	a.cpActive = true

	// Evict oldest if over capacity.
	for len(a.checkpoints) > checkpointMaxKeep {
		a.evictOldestLocked()
	}
	return true
}

// evictOldestLocked removes the oldest checkpoint and updates the total byte
// counter. Caller must hold cpMu.
func (a *App) evictOldestLocked() {
	if len(a.checkpoints) == 0 {
		return
	}
	oldest := a.checkpoints[0]
	for _, snap := range oldest.Files {
		if snap.Content != nil {
			a.cpTotalBytes -= len(snap.Content)
		}
	}
	a.checkpoints = a.checkpoints[1:]
}

// captureForCheckpoint captures the pre-mutation state of a file into the
// current (active) checkpoint. Copy-on-first-write: only the first capture
// for a path within a turn is stored. Called from captureFileOriginal BEFORE
// the mutation occurs.
//
// If the file is too large, it's marked TooLarge (rewind will warn).
// If the file doesn't exist, it's marked Exists=false (rewind will delete it).
// Other read errors (permission, transport, cancellation) are marked Unknown
// (rewind skips with warning — never deletes). This prevents destructive
// deletion of files whose state could not be determined.
func (a *App) captureForCheckpoint(ctx context.Context, canonical string) {
	// Subagent path: relay to the parent's checkpoint via the callback.
	// This ensures subagent file mutations are captured in the parent's
	// /rewind history.
	if a.parentCaptureCallback != nil {
		a.parentCaptureCallback(ctx, canonical)
		return
	}

	a.cpMu.Lock()
	if !a.cpActive || a.cpRewinding || len(a.checkpoints) == 0 {
		a.cpMu.Unlock()
		return
	}
	cp := &a.checkpoints[len(a.checkpoints)-1]
	// Copy-on-first-write: skip if already captured.
	if _, ok := cp.Files[canonical]; ok {
		a.cpMu.Unlock()
		return
	}
	// Record the checkpoint generation for the second-phase re-check.
	// Generation is immutable, so it remains valid even if the checkpoint
	// stack is restructured (compaction, new turn, session rotation) —
	// the capture simply won't store into a different checkpoint.
	cpGen := cp.Generation
	a.cpMu.Unlock()

	// Check file size first to avoid reading large files into memory.
	if a.Exec != nil {
		if size, err := a.Exec.StatFile(ctx, canonical); err == nil && size > checkpointMaxFileSize {
			a.cpMu.Lock()
			// Verify the checkpoint is still the active (last) one — a slow
			// StatFile may have spanned a turn boundary or compaction.
			if cp2 := a.activeCheckpointByGenLocked(cpGen); cp2 != nil {
				if _, ok := cp2.Files[canonical]; !ok {
					cp2.Files[canonical] = FileSnapshot{TooLarge: true}
				}
			}
			a.cpMu.Unlock()
			return
		}
	} else {
		return // no executor — can't capture
	}

	// Read the current (pre-edit) content. ReadFile returns a Go string which
	// preserves arbitrary bytes (byte-exact in both Docker and Direct executors).
	content, err := a.Exec.ReadFile(ctx, canonical)
	if err != nil {
		// Distinguish "not found" from other errors. A not-found file is
		// recorded as Exists=false (rewind will delete it — the file was
		// created during the turn). Other errors (permission, transport,
		// cancellation) are recorded as Unknown (rewind skips with warning).
		isNotFound := isFileNotFoundError(err)
		a.cpMu.Lock()
		// Verify the checkpoint is still the active (last) one.
		if cp2 := a.activeCheckpointByGenLocked(cpGen); cp2 != nil {
			if _, ok := cp2.Files[canonical]; !ok {
				if isNotFound {
					cp2.Files[canonical] = FileSnapshot{Exists: false}
				} else {
					cp2.Files[canonical] = FileSnapshot{Unknown: true}
				}
			}
		}
		a.cpMu.Unlock()
		return
	}

	bytes := []byte(content)
	// Post-read size check (StatFile may have been racy).
	if len(bytes) > checkpointMaxFileSize {
		a.cpMu.Lock()
		if cp2 := a.activeCheckpointByGenLocked(cpGen); cp2 != nil {
			if _, ok := cp2.Files[canonical]; !ok {
				cp2.Files[canonical] = FileSnapshot{TooLarge: true}
			}
		}
		a.cpMu.Unlock()
		return
	}

	a.cpMu.Lock()
	// Re-check: the checkpoint stack may have changed during I/O (compaction,
	// new turn, session rotation). Verify the checkpoint with the same
	// generation still exists and is still the last (active) one.
	cp2 := a.activeCheckpointByGenLocked(cpGen)
	if cp2 == nil {
		a.cpMu.Unlock()
		return // checkpoint stack changed — don't store stale data
	}
	if _, ok := cp2.Files[canonical]; ok {
		// Already captured by a concurrent call — don't overwrite.
		a.cpMu.Unlock()
		return
	}
	cp2.Files[canonical] = FileSnapshot{Exists: true, Content: bytes}
	a.cpTotalBytes += len(bytes)
	// Evict if total bytes exceeded.
	for a.cpTotalBytes > checkpointMaxTotalBytes && len(a.checkpoints) > 1 {
		a.evictOldestLocked()
	}
	a.cpMu.Unlock()
}

// activeCheckpointLocked returns a pointer to the most recent checkpoint, or
// nil if none exists. Caller must hold cpMu.
func (a *App) activeCheckpointLocked() *Checkpoint {
	if len(a.checkpoints) == 0 {
		return nil
	}
	return &a.checkpoints[len(a.checkpoints)-1]
}

// checkpointByGenLocked returns a pointer to the checkpoint with the given
// generation ID, or nil if it no longer exists (evicted, compacted, or
// session rotated). Caller must hold cpMu.
func (a *App) checkpointByGenLocked(gen int64) *Checkpoint {
	for i := range a.checkpoints {
		if a.checkpoints[i].Generation == gen {
			return &a.checkpoints[i]
		}
	}
	return nil
}

// activeCheckpointByGenLocked returns a pointer to the checkpoint with the
// given generation ID only if it is still the last (active) checkpoint.
// This prevents a slow capture from storing into a checkpoint that is no
// longer the active one (e.g. a new turn has started or compaction occurred).
// Returns nil if the checkpoint was evicted or is no longer the last one.
// Caller must hold cpMu.
func (a *App) activeCheckpointByGenLocked(gen int64) *Checkpoint {
	if len(a.checkpoints) == 0 {
		return nil
	}
	last := &a.checkpoints[len(a.checkpoints)-1]
	if last.Generation == gen {
		return last
	}
	return nil
}

// markCheckpointShell marks the current checkpoint as having had a non-read-only
// shell command or background process. Called when run_shell or run_background
// is dispatched. At rewind time, this triggers a warning that shell side
// effects are not reliably reverted.
func (a *App) markCheckpointShell() {
	a.cpMu.Lock()
	defer a.cpMu.Unlock()
	if !a.cpActive {
		return
	}
	if cp := a.activeCheckpointLocked(); cp != nil {
		cp.HadShellCmd = true
	}
}

// isFileNotFoundError reports whether err indicates the file does not exist.
// This distinguishes "not found" (safe to record as Exists=false for rewind
// to delete) from other errors (permission, transport, cancellation) which
// must be recorded as Unknown to prevent destructive deletion.
//
// Primary check is the typed sentinel exec.ErrFileNotFound (returned by both
// DirectExecutor and DockerExecutor). The string-based fallbacks remain for
// any errors that don't wrap the sentinel (e.g. raw os errors from paths
// outside the executor, or future executor implementations). "does not
// exist" is intentionally NOT matched — it matches Docker container errors
// ("container does not exist") which are transport failures, not file-not-
// found.
func isFileNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrFileNotFound) {
		return true
	}
	msg := err.Error()
	// Fallback: os.IsNotExist covers raw os errors not wrapped by the executor.
	// "no such file or directory" / "No such file" cover Docker shell errors
	// that may arrive unwrapped (e.g. from test helpers or future code paths).
	return os.IsNotExist(err) ||
		strings.Contains(strings.ToLower(msg), "no such file or directory") ||
		strings.Contains(msg, "No such file")
}

// endCheckpoint marks the current turn's checkpoint as no longer active.
// Called at the end of SendOutcome (via defer). Subsequent captures outside
// a turn (e.g. from async completions) won't be recorded.
func (a *App) endCheckpoint() {
	a.cpMu.Lock()
	a.cpActive = false
	a.cpMu.Unlock()
}

// clearCheckpoints discards all checkpoints. Called on compaction, session
// rotation, and resume — ConvLen values are invalid after Conv restructuring.
// If a rewind is in progress, this should not be called (the cpRewinding
// guard prevents concurrent turns that could trigger compaction).
func (a *App) clearCheckpoints() {
	a.cpMu.Lock()
	a.checkpoints = nil
	a.cpTurnCount = 0
	a.cpActive = false
	a.cpTotalBytes = 0
	a.cpMu.Unlock()
}

// rewindResult holds the outcome of a rewind operation.
type rewindResult struct {
	RestoredPaths  []string // files successfully restored
	DeletedPaths   []string // files successfully deleted (were created during rewound turns)
	TooLargePaths  []string // files that could not be restored (too large to capture)
	UnknownPaths   []string // files whose state was unknown (skipped — not deleted or restored)
	ShellWarnings  []string // turns that had shell commands
	TurnsRewound   int
	ConvTruncated  bool
	Errors         []string // per-path restore errors
}

func (r *rewindResult) Summary() string {
	if len(r.Errors) > 0 && r.TurnsRewound == 0 {
		// Rewind was aborted due to restore errors. Checkpoints are preserved
		// for retry. Report what was partially done + the errors.
		var b strings.Builder
		b.WriteString("⚠ rewind aborted — checkpoints preserved for retry\n")
		if len(r.RestoredPaths) > 0 {
			fmt.Fprintf(&b, "  %d file(s) were restored before failure:\n", len(r.RestoredPaths))
			for _, p := range r.RestoredPaths {
				b.WriteString("    · " + p + "\n")
			}
		}
		if len(r.DeletedPaths) > 0 {
			fmt.Fprintf(&b, "  %d file(s) were deleted before failure:\n", len(r.DeletedPaths))
			for _, p := range r.DeletedPaths {
				b.WriteString("    · " + p + "\n")
			}
		}
		fmt.Fprintf(&b, "  %d restore error(s):", len(r.Errors))
		for _, e := range r.Errors {
			b.WriteString("\n    · " + e)
		}
		return b.String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "rewound %d turn(s)", r.TurnsRewound)
	if len(r.RestoredPaths) > 0 {
		fmt.Fprintf(&b, "\n  restored %d file(s):", len(r.RestoredPaths))
		for _, p := range r.RestoredPaths {
			b.WriteString("\n    · " + p)
		}
	}
	if len(r.DeletedPaths) > 0 {
		fmt.Fprintf(&b, "\n  deleted %d file(s) (created during rewound turns):", len(r.DeletedPaths))
		for _, p := range r.DeletedPaths {
			b.WriteString("\n    · " + p)
		}
	}
	if len(r.TooLargePaths) > 0 {
		fmt.Fprintf(&b, "\n  ⚠ %d file(s) could not be restored (too large to capture):", len(r.TooLargePaths))
		for _, p := range r.TooLargePaths {
			b.WriteString("\n    · " + p)
		}
	}
	if len(r.UnknownPaths) > 0 {
		fmt.Fprintf(&b, "\n  ⚠ %d file(s) could not be restored (state unknown):", len(r.UnknownPaths))
		for _, p := range r.UnknownPaths {
			b.WriteString("\n    · " + p)
		}
	}
	if len(r.ShellWarnings) > 0 {
		fmt.Fprintf(&b, "\n  ⚠ shell commands ran in %d turn(s) — side effects are NOT reliably reverted:", len(r.ShellWarnings))
		for _, w := range r.ShellWarnings {
			b.WriteString("\n    · " + w)
		}
	}
	if !r.ConvTruncated && r.TurnsRewound > 0 {
		b.WriteString("\n  ⚠ conversation was not truncated (boundary invalid or stale)")
	}
	if len(r.Errors) > 0 {
		fmt.Fprintf(&b, "\n  ⚠ %d restore error(s):", len(r.Errors))
		for _, e := range r.Errors {
			b.WriteString("\n    · " + e)
		}
	}
	return b.String()
}

// rewind restores workspace files and truncates conversation to N checkpoints
// back. N=1 means undo the last turn, N=2 means undo the last 2 turns, etc.
//
// The algorithm:
// 1. Validate N (1 ≤ N ≤ len(checkpoints))
// 2. Build a restore manifest: for each path in the undone range, take the
//    OLDEST checkpoint's snapshot (that's the state before any rewound turn
//    touched it)
// 3. Restore each file: existing files via WriteFileBytes, non-existing via
//    DeletePath, too-large files and unknown-state files are skipped with
//    a warning
// 4. Truncate Conv to the target checkpoint's ConvLen
// 5. Remove the rewound checkpoints
//
// Note: session persistence (if any) is the caller's responsibility, not
// rewind's.
//
// Returns a structured result with per-path outcomes.
func (a *App) rewind(n int) rewindResult {
	a.cpMu.Lock()
	// Refuse rewind while a turn is active — file restores and Conv truncation
	// would race the turn's tool writes and Conv appends.
	if a.cpActive {
		a.cpMu.Unlock()
		return rewindResult{Errors: []string{"cannot rewind while a turn is in progress"}}
	}
	// Refuse rewind while another rewind is already in progress.
	if a.cpRewinding {
		a.cpMu.Unlock()
		return rewindResult{Errors: []string{"cannot rewind while a previous rewind is in progress"}}
	}
	if n < 1 || n > len(a.checkpoints) {
		count := len(a.checkpoints)
		a.cpMu.Unlock()
		return rewindResult{Errors: []string{fmt.Sprintf("invalid rewind: N=%d, available=%d", n, count)}}
	}

	targetIdx := len(a.checkpoints) - n
	target := a.checkpoints[targetIdx]

	// Build restore manifest: oldest snapshot per path in the undone range.
	// Process from the target (oldest in range) forward — the first occurrence
	// of each path is its pre-mutation state before any rewound turn touched it.
	manifest := make(map[string]FileSnapshot)
	for i := targetIdx; i < len(a.checkpoints); i++ {
		for path, snap := range a.checkpoints[i].Files {
			if _, ok := manifest[path]; !ok {
				manifest[path] = snap
			}
		}
	}

	// Collect shell warnings from all rewound turns.
	var shellWarns []string
	for i := targetIdx; i < len(a.checkpoints); i++ {
		if a.checkpoints[i].HadShellCmd {
			shellWarns = append(shellWarns, fmt.Sprintf("turn %d", a.checkpoints[i].TurnIndex))
		}
	}

	// Snapshot the target ConvLen and validate the boundary.
	convLen := target.ConvLen
	// Validate Conv boundary: Conv[convLen-1] should be a user message.
	// If it's not (due to compaction, preamble insertion, etc.), we skip
	// truncation rather than corrupting the conversation.
	a.convMu.RLock()
	convValid := convLen > 0 && convLen <= len(a.Conv)
	if convValid {
		// The checkpoint was created right after appending the user message,
		// so Conv[convLen-1] should be that user message. If it's not (due
		// to compaction restructuring Conv), the boundary is stale.
		convValid = a.Conv[convLen-1].Role == "user"
	}
	a.convMu.RUnlock()

	// Set cpRewinding to block new turns and captures during file restore I/O.
	// This acts as a turn-exclusion guard that persists through the entire
	// restore — unlike cpActive which is already false at this point.
	a.cpRewinding = true
	a.cpMu.Unlock()

	// Ensure cpRewinding is cleared even if a panic occurs during I/O.
	defer func() {
		a.cpMu.Lock()
		a.cpRewinding = false
		a.cpMu.Unlock()
	}()

	// Restore files outside the checkpoint lock (I/O).
	ctx := context.Background()
	result := rewindResult{
		TurnsRewound:  n,
		ShellWarnings: shellWarns,
	}

	// Sort paths for deterministic restore order.
	paths := make([]string, 0, len(manifest))
	for p := range manifest {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	anyError := false
	for _, path := range paths {
		snap := manifest[path]
		switch {
		case snap.Unknown:
			// State could not be determined at capture time — skip with warning.
			result.UnknownPaths = append(result.UnknownPaths, path)
		case snap.TooLarge:
			result.TooLargePaths = append(result.TooLargePaths, path)
		case !snap.Exists:
			// File was created during a rewound turn — delete it.
			if err := a.Exec.DeletePath(ctx, path); err != nil {
				// ENOENT is fine — file may have been deleted by a later
				// operation. Only report non-not-found errors.
				if !isFileNotFoundError(err) {
					result.Errors = append(result.Errors, fmt.Sprintf("delete %s: %v", path, err))
					anyError = true
				} else {
					// Already gone — count as deleted.
					result.DeletedPaths = append(result.DeletedPaths, path)
				}
			} else {
				result.DeletedPaths = append(result.DeletedPaths, path)
			}
			if a.LSP != nil {
				a.LSP.NotifyChange(ctx, path)
			}
		default:
			// File existed before — restore its content via WriteFileBytes
			// (byte-exact, handles both text and binary).
			if _, err := a.Exec.WriteFileBytes(ctx, path, snap.Content); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("restore %s: %v", path, err))
				anyError = true
			} else {
				result.RestoredPaths = append(result.RestoredPaths, path)
			}
			if a.LSP != nil {
				a.LSP.NotifyChange(ctx, path)
			}
		}
	}

	// If any restore error occurred, keep checkpoints intact so the user can
	// retry or inspect. Do NOT truncate conversation on partial failure.
	// Report the failure clearly — the rewind was aborted, not completed.
	if anyError {
		result.TurnsRewound = 0
		result.ConvTruncated = false
		return result
	}

	// Truncate Conv to the target checkpoint's ConvLen — only if the boundary
	// is valid. This removes the assistant's response, tool calls, and tool
	// results from the rewound turn(s), keeping the user message.
	// Re-check the boundary at truncation time to detect any Conv restructuring
	// that happened between the initial check and now.
	if convValid {
		a.convMu.Lock()
		if convLen > 0 && convLen <= len(a.Conv) && a.Conv[convLen-1].Role == "user" {
			a.Conv = a.Conv[:convLen]
			result.ConvTruncated = true
		}
		a.convMu.Unlock()
	}

	// Remove the rewound checkpoints from the stack and update byte counter.
	// This is deferred until AFTER successful file restoration so that a
	// failed restore leaves checkpoints intact for retry.
	a.cpMu.Lock()
	if len(a.checkpoints) >= targetIdx+n {
		newCheckpoints := make([]Checkpoint, targetIdx)
		copy(newCheckpoints, a.checkpoints[:targetIdx])
		for i := targetIdx; i < len(a.checkpoints); i++ {
			for _, snap := range a.checkpoints[i].Files {
				if snap.Content != nil {
					a.cpTotalBytes -= len(snap.Content)
				}
			}
		}
		a.checkpoints = newCheckpoints
	}
	a.cpActive = false
	a.cpMu.Unlock()

	return result
}

// checkpointStatus returns a display string for `/rewind` with no args.
// Lists available checkpoints with their relative N, turn index, file count,
// and shell warning.
func (a *App) checkpointStatus() string {
	a.cpMu.Lock()
	defer a.cpMu.Unlock()

	if len(a.checkpoints) == 0 {
		return "no checkpoints available — checkpoints are created at the start of each turn"
	}

	// Snapshot Conv preview data under convMu.RLock. cpMu → convMu is the
	// established lock order. Compact() calls clearCheckpoints (acquires
	// cpMu) BEFORE acquiring convMu, so no convMu → cpMu inversion exists.
	a.convMu.RLock()
	convCopy := make([]proxy.Message, len(a.Conv))
	copy(convCopy, a.Conv)
	a.convMu.RUnlock()

	var b strings.Builder
	fmt.Fprintf(&b, "%d checkpoint(s) available:\n", len(a.checkpoints))
	for i := len(a.checkpoints) - 1; i >= 0; i-- {
		cp := a.checkpoints[i]
		n := len(a.checkpoints) - i // relative N (1 = most recent)
		fileCount := len(cp.Files)
		// Extract a short preview of the user message for this turn.
		preview := ""
		if cp.ConvLen > 0 && cp.ConvLen <= len(convCopy) {
			for j := cp.ConvLen - 1; j >= 0; j-- {
				if convCopy[j].Role == "user" {
					preview = Truncate(strings.TrimSpace(DerefStr(convCopy[j].Content)), 50)
					break
				}
			}
		}
		fmt.Fprintf(&b, "  %d back — turn %d", n, cp.TurnIndex)
		if preview != "" {
			fmt.Fprintf(&b, " — %q", preview)
		}
		fmt.Fprintf(&b, " — %d file(s)", fileCount)
		if cp.HadShellCmd {
			b.WriteString(" · ⚠ shell")
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nusage: /rewind <N> — rewind N checkpoints (1 = last turn)")
	return strings.TrimRight(b.String(), "\n")
}