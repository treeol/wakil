package browser

import (
	"context"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ctxRecordingSession records the ctx the session exec was started with. The
// hisn exec client is bound to that ctx, so this is what decides whether the
// browser's host client dies when the launch call returns.
type ctxRecordingSession struct {
	*fakeSession
	got chan context.Context
}

func (f ctxRecordingSession) StartInteractiveCLI(ctx context.Context, _ string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	f.got <- ctx
	return nil, nil, nil, 0, context.Canceled // stop before any real process
}

// B1: the browser exec client must not be bound to the launch context. Once
// the launch context is cancelled (launchSession's deferred cancel), the ctx
// given to the session exec must still be live.
func TestBrowserExecCtxOutlivesLaunchContext(t *testing.T) {
	fs := &fakeSession{id: "s-b1", running: true, cli: true, chromiumOK: true}
	got := make(chan context.Context, 1)
	l, err := newSessionLauncher(fs, ctxRecordingSession{fs, got}, "sh")
	if err != nil {
		t.Fatal(err)
	}
	launchCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	_, _, _, err = l.launch(launchCtx)
	if err == nil {
		t.Fatal("stub exec error was swallowed")
	}
	execCtx := <-got
	cancel() // launchSession's deferred cancel
	select {
	case <-execCtx.Done():
		t.Fatal("exec ctx was cancelled with the launch ctx: the browser's host client would die after launch")
	default:
	}
}

// B2: the stop script signals the recorded process group only when the
// recorded leader still matches (pid and start time). A mismatched start time
// (a reused pid) must leave the group alone.
func TestStopScriptSkipsGroupOnStartTimeMismatch(t *testing.T) {
	suffix := "b2test"
	dir := "/tmp/wakil-chrome-" + suffix
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sleeper := osexec.Command("sh", "-c", "sleep 30")
	sleeper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
	pid := sleeper.Process.Pid
	for name, v := range map[string]string{"pid": strconv.Itoa(pid), "pgid": strconv.Itoa(pid), "start": "0"} {
		if err := os.WriteFile(dir+"/"+name, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Run the real stop script; the mismatch must skip the group kill.
	_ = osexec.Command("sh", "-c", stopScript(0), "stop", suffix).Run()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("leader with a mismatched start time was signalled: %v", err)
	}
}

// B2 positive: with the recorded identity matching, the group IS signalled.
func TestStopScriptSignalsGroupOnMatchingIdentity(t *testing.T) {
	suffix := "b2pos"
	dir := "/tmp/wakil-chrome-" + suffix
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sleeper := osexec.Command("sh", "-c", "sleep 30")
	sleeper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
	pid := sleeper.Process.Pid
	st, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	rest := string(st)[strings.LastIndex(string(st), ")")+2:]
	fields := strings.Fields(rest)
	for name, v := range map[string]string{"pid": strconv.Itoa(pid), "pgid": strconv.Itoa(pid), "start": fields[19]} {
		if err := os.WriteFile(dir+"/"+name, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = osexec.Command("sh", "-c", stopScript(0), "stop", suffix).Run()
	done := make(chan error, 1)
	go func() { done <- sleeper.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("matching identity: the recorded group was not signalled")
	}
}

// Q2: the session probe must refuse when python3 (needed by the CDP tunnel)
// is absent, even though chromium is present.
func TestSessionProbeRefusesWithoutPython3(t *testing.T) {
	fs := &fakeSession{id: "s-q2", running: true, cli: true, chromiumOK: true}
	// PATH without python3: chromium lookup is a fake that succeeds.
	exe := noPython{fs}
	err := sessionProbe(context.Background(), exe, "sh")
	if err == nil {
		t.Fatal("probe accepted a session without python3")
	}
}

type noPython struct{ *fakeSession }

func (n noPython) RunShell(ctx context.Context, command string) (string, error) {
	if command == "command -v python3" {
		return "", errors.New("not found")
	}
	return n.fakeSession.RunShell(ctx, command)
}
