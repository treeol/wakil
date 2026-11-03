package exec

import (
	"context"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
)

// shBackend is a sandbox.Backend that runs argv with the REAL local shell in a
// temp dir standing in for the sandbox. It records session lifecycle so tests
// can assert on it. It is a test double only; production never runs on the host.
type shBackend struct {
	root string
	pm   *sandbox.PathMap

	mu         sync.Mutex
	started    []string
	stopped    int
	live       string
	execs      int
	dead       bool // the next Exec/Grant reports the session dead (and clears it)
	deadOnWait bool // the next exec's Wait reports the session dead (death in flight)
}

func (b *shBackend) Start(_ context.Context, s sandbox.Session) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started = append(b.started, s.ID)
	b.live = s.ID
	return nil
}

type shHandle struct {
	done chan struct{}
	info sandbox.ExitInfo
	err  error
}

func (h *shHandle) Wait() (sandbox.ExitInfo, error) { <-h.done; return h.info, h.err }
func (h *shHandle) Kill() error                     { return nil }
func (h *shHandle) Pid() int                        { return 4242 }

func (b *shBackend) Exec(ctx context.Context, spec sandbox.ExecSpec) (sandbox.ExecHandle, error) {
	b.mu.Lock()
	if b.live == "" {
		b.mu.Unlock()
		return nil, sandbox.ErrNoSession
	}
	if b.dead {
		b.dead, b.live = false, ""
		b.mu.Unlock()
		return nil, sandbox.ErrSessionDead
	}
	b.execs++
	b.mu.Unlock()
	cmd := osexec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = b.root
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	if spec.Stderr != nil {
		cmd.Stderr = spec.Stderr
	} else {
		cmd.Stderr = spec.Stdout
	}
	h := &shHandle{done: make(chan struct{})}
	b.mu.Lock()
	dieInFlight := b.deadOnWait
	b.deadOnWait = false
	b.mu.Unlock()
	if dieInFlight {
		go func() { h.err = sandbox.ErrSessionDead; b.mu.Lock(); b.live = ""; b.mu.Unlock(); close(h.done) }()
		return h, nil
	}
	go func() {
		defer close(h.done)
		err := cmd.Run()
		var ee *osexec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee):
			h.info.Code = ee.ExitCode()
		default:
			h.info.BackendError = true
			if spec.Stdout != nil {
				_, _ = io.WriteString(spec.Stdout, err.Error())
			}
		}
	}()
	return h, nil
}

func (b *shBackend) Grant(context.Context, string, bool) error { return nil }
func (b *shBackend) Revoke(context.Context, string) error      { return nil }
func (b *shBackend) Stop(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.live != "" {
		b.stopped++
	}
	b.live = ""
	return nil
}
func (b *shBackend) Close(ctx context.Context) error  { return b.Stop(ctx) }
func (b *shBackend) MapPath(p string) (string, error) { return b.pathMap().MapPath(p) }
func (b *shBackend) UnmapPath(p string) (string, error) {
	return b.pathMap().UnmapPath(p)
}

func (b *shBackend) pathMap() *sandbox.PathMap {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pm == nil {
		b.pm = sandbox.NewPathMap(b.root, b.root, "/tmp")
	}
	return b.pm
}
func (b *shBackend) AgentRoot() string      { return b.root }
func (b *shBackend) SandboxTempDir() string { return "/tmp" }

func newTestBackendExec(t *testing.T) (*BackendExecutor, *shBackend) {
	t.Helper()
	dir := t.TempDir()
	sb := &shBackend{root: dir}
	ex := NewBackendExecutor(sb, BackendOptions{Label: "fake"})
	t.Cleanup(func() { _ = ex.Close() })
	return ex, sb
}

func TestBackendExecutorRequiresBoundChat(t *testing.T) {
	ex, sb := newTestBackendExec(t)
	if _, err := ex.RunShell(context.Background(), "true"); err == nil {
		t.Fatal("RunShell with no chat bound must fail, never run unsandboxed")
	}
	if len(sb.started) != 0 {
		t.Fatalf("session started without a chat: %v", sb.started)
	}
}

func TestBackendExecutorLazyStartAndFileOps(t *testing.T) {
	ex, sb := newTestBackendExec(t)
	ctx := context.Background()
	if err := ex.BindChat(ctx, "chat-1"); err != nil {
		t.Fatal(err)
	}
	if len(sb.started) != 0 {
		t.Fatal("BindChat must not start the session (lazy)")
	}
	if _, err := ex.WriteFile(ctx, "sub/a.txt", "hello"); err != nil {
		t.Fatal(err)
	}
	if len(sb.started) != 1 || sb.started[0] != "chat-1" {
		t.Fatalf("started = %v", sb.started)
	}
	got, err := ex.ReadFile(ctx, "sub/a.txt")
	if err != nil || got != "hello" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if n, err := ex.StatFile(ctx, "sub/a.txt"); err != nil || n != 5 {
		t.Fatalf("StatFile = %d, %v", n, err)
	}
	if _, err := ex.ReadFile(ctx, "missing"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("missing file = %v, want ErrFileNotFound", err)
	}
	if out, _, err := ex.ReadFileBounded(ctx, "sub/a.txt", 3); err != nil || out != "hel" {
		t.Fatalf("ReadFileBounded = %q %v", out, err)
	}
	ls, err := ex.ListDir(ctx, "sub")
	if err != nil || strings.TrimSpace(ls) != "a.txt" {
		t.Fatalf("ListDir = %q %v", ls, err)
	}
	if err := ex.MovePath(ctx, "sub/a.txt", "sub/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := ex.DeletePath(ctx, "sub/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := ex.DeletePath(ctx, "sub/b.txt"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("delete missing = %v", err)
	}
	if ok, err := ex.EntryExists(ctx, "sub"); err != nil || !ok {
		t.Fatalf("EntryExists(sub) = %v %v", ok, err)
	}
	if ok, err := ex.EntryExists(ctx, "nope"); err != nil || ok {
		t.Fatalf("EntryExists(nope) = %v %v", ok, err)
	}
}

func TestBackendExecutorRunShellExitStatus(t *testing.T) {
	ex, _ := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "c")
	out, err := ex.RunShell(ctx, "echo out; echo err >&2; exit 3")
	if !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Errorf("combined output = %q", out)
	}
	var es *ExitStatusError
	if !errors.As(err, &es) || es.Code != 3 || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v, want exit status 3", err)
	}
	// Runs from the workspace root, not the process cwd.
	pwd, _ := ex.RunShell(ctx, "pwd")
	if !strings.HasSuffix(pwd, strings.TrimPrefix(ex.Cwd(), "/")) && pwd != ex.Cwd() {
		t.Errorf("pwd = %q, want %q", pwd, ex.Cwd())
	}
}

func TestBackendExecutorOutputCap(t *testing.T) {
	sb := &shBackend{root: t.TempDir()}
	ex := NewBackendExecutor(sb, BackendOptions{ShellOutputCap: 64})
	defer ex.Close()
	_ = ex.BindChat(context.Background(), "c")
	out, err := ex.RunShell(context.Background(), "yes x | head -c 5000")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[truncated,") {
		t.Errorf("cap not applied: len=%d", len(out))
	}
}

func TestBackendExecutorRebindStopsOldSession(t *testing.T) {
	ex, sb := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "a")
	_, _ = ex.RunShell(ctx, "true")
	g1 := ex.Generation()
	if err := ex.BindChat(ctx, "a"); err != nil || ex.Generation() != g1 {
		t.Fatalf("same-chat rebind must be a no-op (gen %d→%d, err %v)", g1, ex.Generation(), err)
	}
	if err := ex.BindChat(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if sb.stopped != 1 {
		t.Errorf("old session not stopped (stopped=%d)", sb.stopped)
	}
	if ex.Generation() == g1 {
		t.Error("generation must advance on every replacement")
	}
	_, _ = ex.RunShell(ctx, "true")
	if len(sb.started) != 2 || sb.started[1] != "b" {
		t.Errorf("new chat did not start its own session: %v", sb.started)
	}
}

func TestBackendExecutorResetSandbox(t *testing.T) {
	ex, sb := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "a")
	_, _ = ex.RunShell(ctx, "true")
	g := ex.Generation()
	if err := ex.ResetSandbox(ctx); err != nil {
		t.Fatal(err)
	}
	if ex.Generation() == g || sb.stopped != 1 {
		t.Errorf("reset: gen %d→%d stopped=%d", g, ex.Generation(), sb.stopped)
	}
	_, _ = ex.RunShell(ctx, "true")
	if len(sb.started) != 2 || sb.started[1] != "a" {
		t.Errorf("restart for the same chat expected: %v", sb.started)
	}
	// Reset with nothing running is a no-op.
	_ = ex.ResetSandbox(ctx)
	_ = ex.ResetSandbox(ctx)
}

func TestBackendExecutorCloseIsFinal(t *testing.T) {
	ex, sb := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "a")
	_, _ = ex.RunShell(ctx, "true")
	if err := ex.Close(); err != nil {
		t.Fatal(err)
	}
	if sb.stopped != 1 {
		t.Errorf("Close must stop the session (stopped=%d)", sb.stopped)
	}
	if _, err := ex.RunShell(ctx, "true"); err == nil {
		t.Fatal("a call after Close must not lazily restart a session")
	}
	if len(sb.started) != 1 {
		t.Fatalf("session restarted after Close: %v", sb.started)
	}
}

func TestBackendExecutorConcurrentCallsAndRebind(t *testing.T) {
	ex, _ := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "a")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = ex.RunShell(ctx, "true")
			if i == 3 {
				_ = ex.BindChat(ctx, "b")
			}
			if i == 5 {
				_ = ex.ResetSandbox(ctx)
			}
		}(i)
	}
	wg.Wait()
}

func TestBackendExecutorWorktreeChild(t *testing.T) {
	ex, _ := newTestBackendExec(t)
	var wh WorktreeHost = ex
	if wh.WorktreeKind() != WorktreeSandboxTmp || wh.IsWorktreeChild() || wh.SandboxTempDir() != "/tmp" {
		t.Fatalf("parent worktree answers wrong: %v %v %q", wh.WorktreeKind(), wh.IsWorktreeChild(), wh.SandboxTempDir())
	}
	child, err := wh.NewWorktreeExecutor("/tmp/wakil-wt-x")
	if err != nil {
		t.Fatal(err)
	}
	cw := child.(WorktreeHost)
	if !cw.IsWorktreeChild() || child.WorkspaceRoot() != "/tmp/wakil-wt-x" {
		t.Fatalf("child wrong: %v %q", cw.IsWorktreeChild(), child.WorkspaceRoot())
	}
	if _, err := cw.NewWorktreeExecutor("/tmp/y"); err == nil {
		t.Error("nested worktree dispatch must be refused")
	}
	if err := child.Close(); err != nil {
		t.Error(err)
	}
	// Closing the child must not close the shared sandbox.
	if _, err := ex.RunShell(context.Background(), "true"); err == nil || !strings.Contains(err.Error(), "no chat") {
		t.Errorf("unexpected: %v", err)
	}
	if _, err := child.HostPathToURI("/tmp/wakil-wt-x/f"); err == nil {
		t.Error("a private worktree path must have no host/URI mapping")
	}
}

func TestBackendExecutorDeclaresNoBrowserNoKVR(t *testing.T) {
	ex, _ := newTestBackendExec(t)
	if ex.ContainerName() != "" || ex.CDPPort() != 0 || ex.KVRAvailable() || ex.KVRSocketPath() != "" {
		t.Error("backend executor must not advertise container/kvr features")
	}
	if ex.HostBrowserAllowed() {
		t.Error("browser must never be allowed to launch on the host")
	}
}

func TestBackendExecutorStartInteractivePidIsZero(t *testing.T) {
	ex, _ := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "c")
	in, out, _, pid, err := ex.StartInteractive(ctx, "cat")
	if err != nil {
		t.Fatal(err)
	}
	if pid != 0 {
		t.Errorf("pid = %d: a host client pid must never be exposed as a sandbox pid", pid)
	}
	_, _ = io.WriteString(in, "ping\n")
	buf := make([]byte, 5)
	if _, err := io.ReadFull(out, buf); err != nil || string(buf) != "ping\n" {
		t.Fatalf("echo = %q %v", buf, err)
	}
	_ = in.Close()
}

func TestDockerAndDirectAreWorktreeHosts(t *testing.T) {
	var _ WorktreeHost = (*DockerExecutor)(nil)
	var _ WorktreeHost = (*DirectExecutor)(nil)
	var _ WorktreeHost = (*dockerWorktreeExecutor)(nil)
	d, err := NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if d.WorktreeKind() != WorktreeHostFS || d.IsWorktreeChild() {
		t.Error("direct executor worktree answers wrong")
	}
	if os.TempDir() != d.SandboxTempDir() {
		t.Error("direct sandbox temp dir should be the host temp dir")
	}
}

// ---- round-2 regressions ----

// A call that began under one chat must never run in the session of a chat
// bound later. The gate between "no session" and "start it" is forced open
// deterministically: the call is pinned to a scope, then the chat changes.
func TestBackendExecutorStaleScopeCannotStartReplacement(t *testing.T) {
	ex, sb := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "old")
	scoped := ex.withScope(ctx) // call begins under chat "old"
	_ = ex.BindChat(ctx, "new") // ... the chat changes before it is admitted
	_, err := ex.RunShell(scoped, "touch must-not-run")
	if !errors.Is(err, errRebound) {
		t.Fatalf("stale call = %v, want errRebound", err)
	}
	if len(sb.started) != 0 {
		t.Fatalf("a stale call started a session: %v", sb.started)
	}
	if _, serr := os.Stat(ex.Cwd() + "/must-not-run"); serr == nil {
		t.Fatal("stale call executed")
	}
	// A fresh call under the new chat is fine.
	if _, err := ex.RunShell(ctx, "true"); err != nil {
		t.Fatal(err)
	}
	if len(sb.started) != 1 || sb.started[0] != "new" {
		t.Fatalf("started = %v", sb.started)
	}
}

func TestBackendExecutorGenerationBumpRejectsStaleScope(t *testing.T) {
	ex, _ := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "a")
	_, _ = ex.RunShell(ctx, "true")
	scoped := ex.withScope(ctx)
	_ = ex.ResetSandbox(ctx) // same chat, new generation
	if _, err := ex.RunShell(scoped, "true"); !errors.Is(err, errRebound) {
		t.Fatalf("call pinned to the pre-reset generation = %v, want errRebound", err)
	}
}

func TestBackendExecutorMovePathPinnedToOneSession(t *testing.T) {
	ex, _ := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "a")
	if _, err := ex.WriteFile(ctx, "src.txt", "x"); err != nil {
		t.Fatal(err)
	}
	if err := ex.MovePath(ctx, "src.txt", "dst.txt"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := ex.EntryExists(ctx, "dst.txt"); !ok {
		t.Fatal("move did not happen")
	}
}

func TestBackendExecutorChildCannotBindOrReset(t *testing.T) {
	ex, sb := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "parent")
	_, _ = ex.RunShell(ctx, "true")
	child, _ := ex.NewWorktreeExecutor("/tmp/wakil-wt-x")
	if err := child.(ChatBinder).BindChat(ctx, "subagent-chat"); err == nil {
		t.Error("a worktree child must not bind the shared session")
	}
	if err := child.(SandboxResetter).ResetSandbox(ctx); err == nil {
		t.Error("a worktree child must not reset the shared session")
	}
	if sb.stopped != 0 || sb.live != "parent" {
		t.Fatalf("the parent's session was disturbed: stopped=%d live=%q", sb.stopped, sb.live)
	}
}

// ws/link -> a directory outside the workspace: opened on the HOST it is one
// object, resolved in the sandbox (private /tmp, other mounts) another. Such
// a path must not get a host<->URI mapping.
func TestBackendExecutorHostIdentityRejectsEscapingSymlink(t *testing.T) {
	ex, _ := newTestBackendExec(t)
	outside := t.TempDir()
	link := ex.Cwd() + "/link"
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := ex.HostPathToURI(link + "/f.go"); err == nil {
		t.Error("HostPathToURI must refuse a path that resolves outside the workspace on the host")
	}
	uri := "file://" + link + "/f.go"
	if _, err := ex.URIToHostPath(uri); err == nil {
		t.Error("URIToHostPath must refuse a path that resolves outside the workspace on the host")
	}
	// A normal in-workspace path still maps.
	_ = os.WriteFile(ex.Cwd()+"/ok.go", []byte("x"), 0o600)
	u, err := ex.HostPathToURI(ex.Cwd() + "/ok.go")
	if err != nil {
		t.Fatal(err)
	}
	if back, err := ex.URIToHostPath(u); err != nil || back != ex.Cwd()+"/ok.go" {
		t.Fatalf("round trip = %q %v", back, err)
	}
}

// A child that exits while the caller keeps stdin open and never reads stdout
// must be reaped; teardown must not depend on the caller's pipes.
func TestBackendExecutorInteractiveChildExitWithOpenStdin(t *testing.T) {
	ex, _ := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "c")
	in, out, errp, _, err := ex.StartInteractive(ctx, "echo bye; exit 0")
	if err != nil {
		t.Fatal(err)
	}
	// stdin deliberately left open; stdout deliberately not drained first.
	done := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(out) // sees EOF once the child exits
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stdout never reached EOF after the child exited (teardown depends on the caller)")
	}
	_ = in.Close()
	_ = out.Close()
	_ = errp.Close()
}

// A dead session fails the call that found it (no retry: it may not be
// idempotent) and the NEXT call starts a fresh session, like a missing one.
func TestBackendExecutorDeadSessionRestartsOnNextCall(t *testing.T) {
	ex, sb := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "chat-d")
	if _, err := ex.RunShell(ctx, "true"); err != nil {
		t.Fatal(err)
	}
	g1 := ex.Generation()
	sb.mu.Lock()
	sb.dead = true
	sb.mu.Unlock()
	if _, err := ex.RunShell(ctx, "echo must-not-run"); !errors.Is(err, sandbox.ErrSessionDead) {
		t.Fatalf("call on a dead session = %v, want ErrSessionDead", err)
	}
	if ex.Generation() == g1 {
		t.Error("generation must advance so bg entries of the dead session are not trusted")
	}
	sb.mu.Lock()
	execsAfterFailure := sb.execs
	sb.mu.Unlock()
	if execsAfterFailure != 1 {
		t.Errorf("the failing call was retried: %d execs", execsAfterFailure)
	}
	out, err := ex.RunShell(ctx, "echo fresh")
	if err != nil || !strings.Contains(out, "fresh") {
		t.Fatalf("next call after a dead session = %q, %v", out, err)
	}
	if len(sb.started) != 2 || sb.started[1] != "chat-d" {
		t.Errorf("a fresh session for the same chat was not started: %v", sb.started)
	}
}

// Death while the call is in flight (reported by Wait): the call fails typed,
// the generation advances, and the very next call starts a fresh session.
func TestBackendExecutorDeathInFlightRestartsOnNextCall(t *testing.T) {
	ex, sb := newTestBackendExec(t)
	ctx := context.Background()
	_ = ex.BindChat(ctx, "chat-f")
	if _, err := ex.RunShell(ctx, "true"); err != nil {
		t.Fatal(err)
	}
	g1 := ex.Generation()
	sb.mu.Lock()
	sb.deadOnWait = true
	sb.mu.Unlock()
	if _, err := ex.RunShell(ctx, "sleep 1"); !errors.Is(err, sandbox.ErrSessionDead) {
		t.Fatalf("in-flight death = %v, want ErrSessionDead", err)
	}
	if ex.Generation() == g1 {
		t.Error("generation must advance after an in-flight death")
	}
	if out, err := ex.RunShell(ctx, "echo again"); err != nil || !strings.Contains(out, "again") {
		t.Fatalf("next call = %q, %v; want a fresh session", out, err)
	}
	if len(sb.started) != 2 {
		t.Errorf("fresh session not started: %v", sb.started)
	}
}
