package agent

// async_heartbeat.go: proactive progress/stall observer for suspended turns.
//
// When the turn suspends on pending async work (WaitForAsyncCompletion), the
// heartbeat goroutine periodically polls each pending async op for liveness
// signals and emits AsyncProgressMsg so the TUI shows a dynamic "waiting"
// status line instead of a static label. The heartbeat is REPORT-ONLY — it
// does not terminalize ops. The existing watchdogs (shell, subagent, Mashūra)
// remain the primary bounds; the heartbeat gives the user live visibility.
//
// Liveness signals by op kind:
//   - run_shell / run_background: log file size growth (via Exec.StatFile)
//   - dispatch_subagent: subagentCheckpointed[] progress (completed/total)
//   - mashura: last chunk timestamp (lastChunkAt field, set by the chunk forwarder)
//
// Stall detection (report-only):
//   - run_shell / run_background: log not growing for >bgShellStallThreshold
//     → "log idle Xm". If the process group is gone but the op is not terminal
//     → "process gone, op stuck" (the waiting-hang signature — loudest signal).
//   - Other kinds: informational only (watchdogs are the primary bound).

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/treeol/wakil/internal/safe"
)

const (
	// heartbeatInterval is the poll rate for the async progress heartbeat.
	heartbeatInterval = 5 * time.Second

	// bgShellStallThreshold is the maximum time a bg shell's log may be
	// idle (no size growth) before it's reported as stalled. Generous for
	// builds that write infrequently, but finite so the user sees a clear
	// signal before the 1h watchdog fires.
	bgShellStallThreshold = 5 * time.Minute
)

// startAsyncHeartbeat launches a goroutine that periodically polls pending
// async ops and emits AsyncProgressMsg. It runs until ctx is cancelled
// (by the caller when the turn resumes or is cancelled). An immediate first
// tick fires so the user sees progress within the first tick, not after
// 5s of static "waiting".
func (a *App) startAsyncHeartbeat(ctx context.Context) {
	safe.Go("async-heartbeat", func() {
		// Immediate first tick — don't make the user wait 5s for the first
		// progress line after suspension.
		if msg := a.buildProgressMsg(ctx); msg != nil {
			a.sendEvent(*msg)
		}
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				msg := a.buildProgressMsg(ctx)
				if msg != nil {
					a.sendEvent(*msg)
				}
			}
		}
	})
}

// buildProgressMsg snapshots all pending async ops and returns a progress
// message. Returns nil if there are no active ops (nothing to report).
// Ops are sorted by createdAt so the status line is stable (not flipping
// between map-iteration-order entries).
func (a *App) buildProgressMsg(ctx context.Context) *AsyncProgressMsg {
	a.asyncMu.Lock()
	if a.asyncActive == 0 {
		a.asyncMu.Unlock()
		return nil
	}
	ops := make([]*asyncOp, 0, a.asyncActive)
	for _, op := range a.asyncOps {
		ops = append(ops, op)
	}
	a.asyncMu.Unlock()

	// Sort by createdAt for stable display ordering.
	sort.Slice(ops, func(i, j int) bool {
		return ops[i].createdAt.Before(ops[j].createdAt)
	})

	progress := make([]AsyncProgressOp, 0, len(ops))
	for _, op := range ops {
		// Snapshot the fields we need under op.mu, then release before
		// any blocking calls (os.Stat, IsProcessGroupAlive).
		op.mu.Lock()
		if op.terminal {
			op.mu.Unlock()
			continue
		}
		kind := op.toolName
		label := op.label
		createdAt := op.createdAt
		startedAt := op.startedAt
		// Subagent progress.
		completed := 0
		total := len(op.subagentCheckpointed)
		for _, c := range op.subagentCheckpointed {
			if c {
				completed++
			}
		}
		// Mashūra last-chunk time.
		lastChunkAt := op.lastChunkAt
		op.mu.Unlock()

		// Compute elapsed from startedAt if available (queue time excluded),
		// otherwise from createdAt.
		elapsedFrom := createdAt
		if !startedAt.IsZero() {
			elapsedFrom = startedAt
		}
		elapsed := time.Since(elapsedFrom).Truncate(time.Second).String()
		activity := ""
		stalled := false

		switch kind {
		case "run_shell", "run_background":
			activity, stalled = a.checkBgShellLiveness(ctx, op)
		case "dispatch_subagent", "dispatch_subagents":
			if total > 0 {
				activity = fmt.Sprintf("%d/%d children done", completed, total)
			} else {
				activity = "running"
			}
		default:
			// Mashūra and other uiJob ops.
			if !lastChunkAt.IsZero() {
				since := time.Since(lastChunkAt).Truncate(time.Second)
				activity = fmt.Sprintf("last chunk %s ago", since)
			} else if !startedAt.IsZero() {
				activity = "calling panel"
			} else {
				activity = "starting"
			}
		}

		progress = append(progress, AsyncProgressOp{
			OpID:     op.id,
			Kind:     kind,
			Label:    label,
			Elapsed:  elapsed,
			Activity: activity,
			Stalled:  stalled,
		})
	}

	if len(progress) == 0 {
		return nil
	}
	return &AsyncProgressMsg{Ops: progress}
}

// checkBgShellLiveness checks whether a bg shell op's log is growing.
// Returns (activity description, stalled). This does NOT hold op.mu —
// the caller snapshots the needed fields and passes the op pointer only
// to find the matching bgEntry. All blocking calls use the heartbeat's
// ctx so cancellation propagates.
//
// bgEntry.lastLogSize and lastLogGrowth are read and written under bgMu to
// prevent a data race when two heartbeat goroutines overlap (hbCancel does
// not join the goroutine, so a fast resume + re-suspend can start a new
// heartbeat while the old one is still in buildProgressMsg).
func (a *App) checkBgShellLiveness(ctx context.Context, op *asyncOp) (string, bool) {
	// Find the bgEntry for this op by pointer match. Hold bgMu only briefly.
	a.bgMu.RLock()
	var entry *bgEntry
	for _, e := range a.bgProcs {
		if e.asyncOp == op {
			entry = e
			break
		}
	}
	a.bgMu.RUnlock()
	if entry == nil || entry.logPath == "" {
		return "no log", false
	}

	// Snapshot the current tracking state under bgMu before blocking calls.
	a.bgMu.RLock()
	prevSize := entry.lastLogSize
	prevGrowth := entry.lastLogGrowth
	a.bgMu.RUnlock()

	// Use Exec.StatFile (works in docker/sandbox mode, unlike os.Stat).
	size, err := a.Exec.StatFile(ctx, entry.logPath)
	if err != nil {
		return "log unreadable", false
	}

	// First observation — record and don't judge.
	if prevSize == 0 && prevGrowth.IsZero() {
		a.bgMu.Lock()
		// Recheck under write lock: another heartbeat may have initialized
		// while we were in StatFile. Only initialize if still unset.
		if entry.lastLogSize == 0 && entry.lastLogGrowth.IsZero() {
			entry.lastLogSize = size
			entry.lastLogGrowth = time.Now()
		}
		a.bgMu.Unlock()
		return "monitoring", false
	}

	if size > prevSize {
		// Log grew — update the last-growth time with a monotonic CAS:
		// only write if size exceeds the current value, so a stale
		// snapshot from a slower heartbeat can't regress the tracking state.
		a.bgMu.Lock()
		if size > entry.lastLogSize {
			entry.lastLogSize = size
			entry.lastLogGrowth = time.Now()
		}
		a.bgMu.Unlock()
		return "log growing", false
	}

	// Log didn't grow. Check how long it's been idle. Re-read the current
	// lastLogGrowth under the lock to avoid using a stale snapshot.
	a.bgMu.RLock()
	currentGrowth := entry.lastLogGrowth
	a.bgMu.RUnlock()
	idle := time.Since(currentGrowth)
	if idle >= bgShellStallThreshold {
		// Recheck op.terminal before the stall warning — the watchdog or
		// reaper may have terminalized the op while we were in StatFile
		// or waiting for the idle threshold. If the op is already terminal,
		// report "completed" instead of a misleading "stuck" warning.
		op.mu.Lock()
		terminal := op.terminal
		op.mu.Unlock()
		if terminal {
			return "completed", false
		}
		// Stalled: log not growing for >threshold. Check if the process
		// group is still alive.
		if a.Exec.IsProcessGroupAlive(ctx, entry.pgid) {
			// Recheck terminal after the blocking probe — the op may have
			// been terminalized while we were checking process liveness.
			op.mu.Lock()
			terminal = op.terminal
			op.mu.Unlock()
			if terminal {
				return "completed", false
			}
			return fmt.Sprintf("log idle %s, process alive", idle.Truncate(time.Second)), true
		}
		// Process gone but op not terminal — the waiting-hang signature.
		// This is the loudest signal: the process exited but the async op
		// was never terminalized (reaper missed it, or a leak).
		return fmt.Sprintf("process gone, op stuck %s", idle.Truncate(time.Second)), true
	}

	return fmt.Sprintf("log idle %s", idle.Truncate(time.Second)), false
}
