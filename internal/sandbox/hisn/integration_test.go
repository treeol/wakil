package hisn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
)

// Integration tests against a REAL hisn. They SKIP (never fail) when hisn is
// missing, too old, or `hisn check` reports the host cannot run sessions
// (e.g. no unprivileged user namespaces, as in many build sandboxes).
//
// Run on a host with hisn >= 0.2.0:
//
//	go test -count=1 -v -timeout 5m -run TestIntegration ./internal/sandbox/hisn/
//
// `hisn run --detach` does NOT return: its supervisor IS the session. Start
// therefore spawns it, waits for readiness (control socket + `hisn ls
// --session`) and keeps the process as the session's lifetime handle. Every
// Start error carries the supervisor's stderr (captured with `-v`) and names
// <state>/<chat>.supervisor.log, which holds it for every start.
//
// Set WAKIL_HISN_BIN to test a specific binary.

func realBackend(t *testing.T, grantable ...string) (*Backend, string) {
	t.Helper()
	bin := os.Getenv("WAKIL_HISN_BIN")
	if bin == "" {
		p, err := osexec.LookPath("hisn")
		if err != nil {
			t.Skip("hisn not found on PATH")
		}
		bin = p
	}
	// 0.2.1 gate: --ready-fd, exec_id, --prune, stale-kill, session.shm/tmp_size.
	if out, err := osexec.Command(bin, "--version").Output(); err != nil {
		t.Skipf("hisn unusable: %v", err)
	} else if v, err := ParseVersion(string(out)); err != nil || !VersionAtLeast(v, [3]int{0, 2, 1}) {
		t.Skipf("hisn >= 0.2.1 required (got %q, err %v)", strings.TrimSpace(string(out)), err)
	}
	root := t.TempDir()
	// hisn forbids cwd under its runtime dir (/tmp/hisn-$UID); a t.TempDir()
	// below /tmp is fine, but resolve symlinks like Wakil does.
	cwd, err := filepath.EvalSymlinks(filepath.Join(root, "ws"))
	if err != nil {
		if mkerr := os.MkdirAll(filepath.Join(root, "ws"), 0o755); mkerr != nil {
			t.Fatal(mkerr)
		}
		cwd, _ = filepath.EvalSymlinks(filepath.Join(root, "ws"))
	}
	b, err := New(Options{
		Binary: bin, StateDir: filepath.Join(root, "state"), HomeRoot: filepath.Join(root, "home"),
		HostCwd: cwd, GrantableRW: grantable, EnvPass: []string{"CI", "GOFLAGS"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := b.Preflight(ctx); err != nil {
		t.Skipf("hisn unusable on this host: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b, cwd
}

// istart starts the session; on failure it reports the full error, which
// includes the supervisor's exit status and captured stderr.
func istart(t *testing.T, b *Backend, id, cwd string) {
	t.Helper()
	if err := b.Start(ictx(t), sandbox.Session{ID: id, HostCwd: cwd}); err != nil {
		t.Fatalf("Start(%s) failed: %v", id, err)
	}
}

func ictx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func irun(t *testing.T, b *Backend, cwd string, argv ...string) (string, sandbox.ExitInfo) {
	t.Helper()
	var out bytes.Buffer
	h, err := b.Exec(ictx(t), sandbox.ExecSpec{Argv: argv, Cwd: cwd, Stdout: &out})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	info, err := h.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), info
}

func TestIntegrationStartExec(t *testing.T) {
	b, cwd := realBackend(t)
	istart(t, b, "wakil-it-1", cwd)
	for i := 0; i < 3; i++ { // 3 sequential
		out, info := irun(t, b, "", "sh", "-c", "echo seq-$$ && pwd")
		if info.Code != 0 || !strings.Contains(out, "seq-") || !strings.Contains(out, cwd) {
			t.Fatalf("seq %d: %q %+v (cwd must be the identical host path %s)", i, out, info, cwd)
		}
	}
	var wg sync.WaitGroup // 3 concurrent
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, info := irun(t, b, "", "sh", "-c", "sleep 1; echo par")
			if info.Code != 0 || strings.TrimSpace(out) != "par" {
				t.Errorf("concurrent: %q %+v", out, info)
			}
		}()
	}
	wg.Wait()
	if _, info := irun(t, b, "", "sh", "-c", "exit 9"); info.Code != 9 {
		t.Errorf("exit code = %+v, want 9", info)
	}
	// The workspace is the only writable host location.
	if _, info := irun(t, b, "", "sh", "-c", "echo x > "+filepath.Join(cwd, "w.txt")); info.Code != 0 {
		t.Errorf("write into cwd failed: %+v", info)
	}
	if _, err := os.Stat(filepath.Join(cwd, "w.txt")); err != nil {
		t.Errorf("file written in the sandbox is not on the host: %v", err)
	}
}

func TestIntegrationEnvIsAllowlisted(t *testing.T) {
	b, cwd := realBackend(t)
	t.Setenv("WAKIL_IT_SECRET", "must-not-leak")
	t.Setenv("CI", "1")
	istart(t, b, "wakil-it-env", cwd)
	out, _ := irun(t, b, "", "sh", "-c", "env")
	if strings.Contains(out, "WAKIL_IT_SECRET") || strings.Contains(out, "must-not-leak") {
		t.Errorf("host environment leaked into the sandbox:\n%s", out)
	}
	if !strings.Contains(out, "CI=1") {
		t.Errorf("allowlisted CI missing:\n%s", out)
	}
}

// TestIntegrationSSHAgentForwarded: when sandbox.hisn.ssh_agent is true,
// SSH_AUTH_SOCK reaches the host-side hisn supervisor so the session agent
// can see the allowlisted keys. The in-sandbox environment stays allowlisted:
// the host socket path must not appear inside the sandbox (hisn's proxy
// provides the in-sandbox socket).
//
// A throwaway agent is started in the test (ssh-agent -s with a t.TempDir
// socket, ssh-keygen ed25519 temp key, ssh-add), allowlisting only that key's
// fingerprint. Inside the session, `ssh-add -l` must list exactly that one
// fingerprint. Skips when ssh-agent or ssh-keygen is missing.
func TestIntegrationSSHAgentForwarded(t *testing.T) {
	// Find ssh-agent and ssh-keygen.
	agentPath, err := osexec.LookPath("ssh-agent")
	if err != nil {
		t.Skip("ssh-agent not found on PATH")
	}
	keygenPath, err := osexec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not found on PATH")
	}

	// Generate a throwaway ed25519 key.
	keyDir := t.TempDir()
	keyPath := filepath.Join(keyDir, "id_ed25519")
	if _, err := osexec.Command(keygenPath, "-t", "ed25519", "-N", "", "-f", keyPath).Output(); err != nil {
		t.Skipf("ssh-keygen failed: %v", err)
	}

	// Start a throwaway ssh-agent with a temp-dir socket.
	socket := filepath.Join(t.TempDir(), "agent.sock")
	agentOut, err := osexec.Command(agentPath, "-s", "-a", socket).Output()
	if err != nil {
		t.Skipf("ssh-agent failed: %v", err)
	}
	// Parse the agent output to get SSH_AUTH_SOCK and SSH_AGENT_PID.
	// Format: "SSH_AUTH_SOCK=/path; export SSH_AUTH_SOCK;\nSSH_AGENT_PID=123; export SSH_AGENT_PID;"
	var agentSock, agentPid string
	for _, line := range strings.Split(string(agentOut), "\n") {
		line = strings.TrimSpace(line)
		if kv := strings.SplitN(line, ";", 2); len(kv) > 0 {
			if strings.HasPrefix(kv[0], "SSH_AUTH_SOCK=") {
				agentSock = strings.TrimPrefix(kv[0], "SSH_AUTH_SOCK=")
			}
			if strings.HasPrefix(kv[0], "SSH_AGENT_PID=") {
				agentPid = strings.TrimPrefix(kv[0], "SSH_AGENT_PID=")
			}
		}
	}
	if agentSock == "" || agentPid == "" {
		t.Fatalf("could not parse ssh-agent output: %s", agentOut)
	}
	pid, _ := strconv.Atoi(agentPid)
	if pid > 0 {
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
	}

	// Add the throwaway key to the agent. SSH_AUTH_SOCK must point at our
	// throwaway agent, not whatever the test runner happened to inherit.
	t.Setenv("SSH_AUTH_SOCK", agentSock)
	if err := osexec.Command("ssh-add", keyPath).Run(); err != nil {
		t.Skipf("ssh-add failed: %v", err)
	}
	// Get the fingerprint of the throwaway key.
	pubFp, err := osexec.Command("ssh-keygen", "-lf", keyPath+".pub").Output()
	if err != nil {
		t.Skipf("ssh-keygen -lf failed: %v", err)
	}
	// Format: "<bits> SHA256:... <path>"
	fpLine := strings.TrimSpace(string(pubFp))
	fields := strings.Fields(fpLine)
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "SHA256:") {
		t.Skipf("could not parse fingerprint from %q", fpLine)
	}
	fp := fields[1]

	// Read the .pub file content (the public key line) for the signing key.
	pubBytes, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Skipf("reading .pub file: %v", err)
	}
	pubKeyLine := strings.TrimSpace(string(pubBytes))

	// Build a backend with ssh_agent=true and that one fingerprint allowed.
	bin := os.Getenv("WAKIL_HISN_BIN")
	if bin == "" {
		p, err := osexec.LookPath("hisn")
		if err != nil {
			t.Skip("hisn not found on PATH")
		}
		bin = p
	}
	if out, err := osexec.Command(bin, "--version").Output(); err != nil {
		t.Skipf("hisn unusable: %v", err)
	} else if v, err := ParseVersion(string(out)); err != nil || !VersionAtLeast(v, [3]int{0, 3, 2}) {
		t.Skipf("hisn >= 0.3.2 required for ssh_agent (got %q)", strings.TrimSpace(string(out)))
	}
	root := t.TempDir()
	cwd := filepath.Join(root, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, _ = filepath.EvalSymlinks(cwd)

	b, err := New(Options{
		Binary:    bin,
		StateDir:  filepath.Join(root, "state"),
		HomeRoot:  filepath.Join(root, "home"),
		HostCwd:   cwd,
		SSHAgent:  true,
		SSHKeys:   []string{fp},
		EnvPass:   []string{"CI", "GOFLAGS"},
		Surface:   Surface{RiskAck: "signing"},
		SigningKey: pubKeyLine, // F5: signing key for the session gitconfig
		GitName:   "Wakil Test",
		GitEmail:  "test@wakil.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := b.Preflight(ctx); err != nil {
		t.Skipf("hisn unusable on this host: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	if err := b.Start(ictx(t), sandbox.Session{ID: "wakil-it-ssh-agent", HostCwd: cwd}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Inside the session: ssh-add -l must list exactly that one fingerprint.
	out, info := irun(t, b, "", "sh", "-c", "ssh-add -l 2>&1")
	if info.Code != 0 {
		t.Fatalf("ssh-add -l failed inside the session: %q %+v", out, info)
	}
	// ssh-add -l output: "<n> SHA256:... <comment> (<type>)"
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var found bool
	for _, l := range lines {
		if strings.Contains(l, fp) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ssh-add -l inside the session does not list the allowlisted fingerprint %s:\n%s", fp, out)
	}

	// The host socket path must not appear inside the sandbox.
	if strings.Contains(out, socket) {
		t.Errorf("host SSH_AUTH_SOCK path leaked into the sandbox: %s appears in:\n%s", socket, out)
	}

	// F5+F8: make a signed commit inside the session and verify the signature on
	// the host side. The session gitconfig (written by withGitConfig under
	// HomeRoot on the host, pointed at by GIT_CONFIG_GLOBAL=<SandboxHome>/.wakil-gitconfig
	// inside the session) carries user.signingkey, gpg.format=ssh,
	// commit.gpgsign=true, and the allowed_signers file. The agent proxy
	// forwards the key. Nothing is set inside the session by the test — the
	// product path (host file + in-session env) is what is tested.
	if _, info := irun(t, b, cwd, "git", "init", "-q"); info.Code != 0 {
		t.Fatalf("git init inside session failed: %+v", info)
	}
	// Verify the session gitconfig is visible at the in-session path.
	gcOut, gcInfo := irun(t, b, cwd, "git", "config", "--show-origin", "--get", "user.signingkey")
	if gcInfo.Code != 0 {
		t.Fatalf("git config --show-origin --get user.signingkey inside session failed: %q %+v", gcOut, gcInfo)
	}
	if !strings.Contains(gcOut, "file:"+SandboxHome+"/.wakil-gitconfig") {
		t.Errorf("user.signingkey not reported from the in-session gitconfig:\n%s", gcOut)
	}
	if !strings.Contains(gcOut, "key::") {
		t.Errorf("user.signingkey value is not a key:: literal:\n%s", gcOut)
	}
	gpgOut, gpgInfo := irun(t, b, cwd, "git", "config", "--get", "commit.gpgsign")
	if gpgInfo.Code != 0 || strings.TrimSpace(gpgOut) != "true" {
		t.Fatalf("commit.gpgsign inside session = %q (code %d), want true", gpgOut, gpgInfo.Code)
	}
	if _, info := irun(t, b, cwd, "sh", "-c", "echo test > file.txt"); info.Code != 0 {
		t.Fatalf("write file inside session failed: %+v", info)
	}
	if _, info := irun(t, b, cwd, "git", "add", "file.txt"); info.Code != 0 {
		t.Fatalf("git add inside session failed: %+v", info)
	}
	commitOut, commitInfo := irun(t, b, cwd, "git", "commit", "-m", "test signed commit")
	if commitInfo.Code != 0 {
		t.Fatalf("git commit inside session failed: %q %+v", commitOut, commitInfo)
	}
	// Verify the commit is signed on the host side.
	signOut, err := osexec.Command("git", "-C", cwd, "log", "-1", "--format=%G?").Output()
	if err != nil {
		t.Fatalf("git log on host failed: %v", err)
	}
	// %G? = G for a good signature, B for a bad signature, U for a good
	// signature with unknown validity, N for no signature.
	signStatus := strings.TrimSpace(string(signOut))
	if signStatus != "G" && signStatus != "U" {
		t.Errorf("signed commit not verified on host: %%G?=%q (want G or U)", signStatus)
	}
}

func TestIntegrationGrantRevokeVisibility(t *testing.T) {
	b, cwd := realBackend(t)
	istart(t, b, "wakil-it-grant", cwd)
	outside := t.TempDir()
	// t.TempDir lives under /tmp, which hisn masks with a private tmpfs; use a
	// directory under the user's cache instead when /tmp is the parent.
	if strings.HasPrefix(outside, "/tmp/") {
		home, _ := os.UserHomeDir()
		d, err := os.MkdirTemp(home, ".wakil-hisn-it-")
		if err != nil {
			t.Skipf("no usable directory outside /tmp: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		outside = d
	}
	if err := os.WriteFile(filepath.Join(outside, "f.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, info := irun(t, b, "", "cat", filepath.Join(outside, "f.txt")); info.Code == 0 {
		t.Fatal("an ungranted host path must not exist in the sandbox (ENOENT)")
	}
	if err := b.Grant(ictx(t), outside, false); err != nil {
		t.Fatal(err)
	}
	if out, info := irun(t, b, "", "cat", filepath.Join(outside, "f.txt")); info.Code != 0 || out != "data" {
		t.Fatalf("granted path not readable: %q %+v", out, info)
	}
	if _, info := irun(t, b, "", "sh", "-c", "echo x > "+filepath.Join(outside, "w.txt")); info.Code == 0 {
		t.Error("a ro grant must reject writes (EROFS)")
	}
	if err := b.Revoke(ictx(t), outside); err != nil {
		t.Fatal(err)
	}
	if _, info := irun(t, b, "", "cat", filepath.Join(outside, "f.txt")); info.Code == 0 {
		t.Error("revoked path must be ENOENT again")
	}
	// rw outside every grantable prefix is E_NOT_GRANTABLE.
	if err := b.Grant(ictx(t), outside, true); !errors.Is(err, sandbox.ErrNotGrantable) {
		t.Errorf("rw outside grantable_rw = %v, want ErrNotGrantable", err)
	}
}

func TestIntegrationStopKillsSleeper(t *testing.T) {
	b, cwd := realBackend(t)
	istart(t, b, "wakil-it-kill", cwd)
	pidfile := filepath.Join(cwd, "sleeper.pid")
	// A detached sleeper that outlives its launcher exec.
	if _, info := irun(t, b, "", "sh", "-c", "setsid nohup sleep 300 >/dev/null 2>&1 & echo $! > "+pidfile); info.Code != 0 {
		t.Fatalf("launch: %+v", info)
	}
	out, info := irun(t, b, "", "sh", "-c", "kill -0 $(cat "+pidfile+") && echo alive")
	if info.Code != 0 || !strings.Contains(out, "alive") {
		t.Fatalf("background process did not survive its launcher exec: %q %+v", out, info)
	}
	if err := b.Stop(ictx(t)); err != nil {
		t.Fatal(err)
	}
	sessions, err := b.listSessions(ictx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, still := sessions["wakil-it-kill"]; still {
		t.Fatal("session survived Stop")
	}
	// Nothing named sleep 300 may remain on the host.
	if o, _ := osexec.Command("pgrep", "-f", "sleep 300").Output(); strings.TrimSpace(string(o)) != "" {
		for _, f := range strings.Fields(string(o)) {
			var pid int
			_, _ = fmtSscan(f, &pid)
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		t.Errorf("sleeper survived the session: pids %s", o)
	}
}

func TestIntegrationOrphanReconcile(t *testing.T) {
	b, cwd := realBackend(t)
	istart(t, b, "wakil-it-orphan", cwd)
	// Forge a dead owner, as if the Wakil that created the session crashed.
	rec, err := b.readRecord("wakil-it-orphan")
	if err != nil {
		t.Fatal(err)
	}
	rec.Pid, rec.PidStart = 999999, ""
	if err := b.rewriteRecord(rec); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.cur = nil
	b.mu.Unlock()

	b2, err := New(b.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := b2.Reconcile(ictx(t)); err != nil {
		t.Fatal(err)
	}
	sessions, err := b2.listSessions(ictx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, still := sessions["wakil-it-orphan"]; still {
		t.Fatal("orphan session was not reaped")
	}
}

func TestIntegrationStartRefusesForeignSession(t *testing.T) {
	b, cwd := realBackend(t)
	// A session created outside Wakil's control (no owner record). `hisn run
	// --detach` blocks, so run it as a child and wait until it is live.
	var ferr bytes.Buffer
	foreign := osexec.Command(b.opts.Binary, "run", "--detach", "-v", "--session-id", "wakil-it-foreign", "--cwd", cwd)
	foreign.Stdout, foreign.Stderr = &ferr, &ferr
	foreign.SysProcAttr = supervisorSysProcAttr()
	if err := foreign.Start(); err != nil {
		t.Skipf("cannot create a foreign session: %v", err)
	}
	fdone := make(chan struct{})
	go func() { _ = foreign.Wait(); close(fdone) }()
	t.Cleanup(func() {
		_ = osexec.Command(b.opts.Binary, "kill", "--session", "wakil-it-foreign").Run()
		select {
		case <-fdone:
		case <-time.After(5 * time.Second):
			_ = foreign.Process.Kill()
			<-fdone
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		m, err := b.listSessions(ictx(t))
		if err == nil && m["wakil-it-foreign"].Live() {
			break
		}
		select {
		case <-fdone:
			t.Skipf("cannot create a foreign session: supervisor exited: %s", ferr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Skipf("foreign session never became live: %s", ferr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	err := b.Start(ictx(t), sandbox.Session{ID: "wakil-it-foreign", HostCwd: cwd})
	if !errors.Is(err, sandbox.ErrSessionInUse) {
		t.Fatalf("want ErrSessionInUse, got %v", err)
	}
	sessions, _ := b.listSessions(ictx(t))
	if _, ok := sessions["wakil-it-foreign"]; !ok {
		t.Fatal("Wakil killed a session it did not create")
	}
}

// The supervisor carries Pdeathsig: a Wakil that dies without stopping anything
// must not leave a live sandbox behind. The session is started from a child
// process that then exits (a "crash"); `hisn ls` must stop listing it.
func TestIntegrationSupervisorDiesWithParent(t *testing.T) {
	b, cwd := realBackend(t)
	cfg, err := json.Marshal(parentHelperCfg{Options: b.opts, ID: "wakil-it-parent"})
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := osexec.Command(self)
	cmd.Env = []string{parentHelperEnv + "=" + string(cfg), "PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"), "XDG_RUNTIME_DIR=" + os.Getenv("XDG_RUNTIME_DIR")}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var herr bytes.Buffer
	cmd.Stderr = &herr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	_ = cmd.Wait() // the helper has exited without Stop/Close
	if !strings.HasPrefix(line, "started ") {
		t.Fatalf("helper could not start a session: %q stderr: %s", line, herr.String())
	}
	t.Cleanup(func() { // belt and braces: never leave it behind if the assertion fails
		_ = osexec.Command(b.opts.Binary, "kill", "--session", "wakil-it-parent", "--signal", "KILL").Run()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		m, err := b.listSessions(ictx(t))
		if err != nil {
			t.Fatalf("hisn ls: %v", err)
		}
		if _, still := m["wakil-it-parent"]; !still {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the supervisor outlived its parent for 5s: hisn ls = %+v (cwd %s)", m, cwd)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func fmtSscan(s string, n *int) (int, error) {
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		v = v*10 + int(c-'0')
	}
	*n = v
	return 1, nil
}

// The supervisor's lifetime belongs to the SESSION, not to the tool call that
// happened to start it: cancelling that call's context must not touch it.
func TestIntegrationSupervisorSurvivesToolCallContext(t *testing.T) {
	b, cwd := realBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	if err := b.Start(ctx, sandbox.Session{ID: "wakil-it-ctx", HostCwd: cwd}); err != nil {
		cancel()
		t.Fatalf("Start failed: %v", err)
	}
	cancel() // the tool call that started the session is over
	time.Sleep(1500 * time.Millisecond)
	if b.supOf().hasExited() {
		t.Fatalf("cancelling the starting call's context killed the supervisor: %s\n%s",
			b.supOf().exitStatus(), readLog(t, b, "wakil-it-ctx"))
	}
	out, info := irun(t, b, "", "sh", "-c", "echo after-cancel")
	if info.Code != 0 || strings.TrimSpace(out) != "after-cancel" {
		t.Fatalf("exec after the start context was cancelled: %q %+v\nsupervisor log:\n%s", out, info, readLog(t, b, "wakil-it-ctx"))
	}
}

// A dead (stale) leftover of THIS chat id is replaced, not an obstacle: SIGKILL
// a real Wakil-started supervisor so its directory stays behind, forget the
// session as a restarted Wakil would, then Start the same id.
func TestIntegrationStaleDirReplaced(t *testing.T) {
	b, cwd := realBackend(t)
	const id = "wakil-it-stale"
	istart(t, b, id, cwd)
	sup := b.supOf()
	_ = syscall.Kill(sup.pid(), syscall.SIGKILL) // no cleanup: the directory stays behind
	if !sup.waitExit(5 * time.Second) {
		t.Fatal("supervisor did not die")
	}
	var st string
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if st, _ = b.sessionState(ictx(t), id); st != "live" {
			break
		}
	}
	if st != "stale" {
		t.Skipf("hisn left no stale directory after SIGKILL (state %q); nothing to replace", st)
	}
	// A restarted Wakil knows nothing of the old process or its record.
	b.mu.Lock()
	b.cur, b.orphans = nil, nil
	b.mu.Unlock()
	b.removeRecord(id)

	if err := b.Start(ictx(t), sandbox.Session{ID: id, HostCwd: cwd}); err != nil {
		t.Fatalf("a stale directory of this chat blocked the start: %v", err)
	}
	if out, info := irun(t, b, "", "sh", "-c", "echo replaced"); info.Code != 0 || strings.TrimSpace(out) != "replaced" {
		t.Fatalf("replacement session unusable: %q %+v\nlog:\n%s", out, info, readLog(t, b, id))
	}
	if st, _ := b.sessionState(ictx(t), id); st != "live" {
		t.Fatalf("session state after replacement = %q, want live", st)
	}
	if !strings.Contains(readLog(t, b, id), "replacing stale session directory") {
		t.Errorf("replacement not recorded in the supervisor log:\n%s", readLog(t, b, id))
	}
}

// 0.2.1 follow-ups, against a REAL hisn >= 0.2.1 (realBackend skips below it):

// Kill (cancel) a ctl exec by exec_id: TERM reaches the program's process
// group through the same connection; a stale exec_id is E_NOT_FOUND.
func TestIntegrationSignalByExecID(t *testing.T) {
	b, cwd := realBackend(t)
	istart(t, b, "wakil-it-sig", cwd)
	var out bytes.Buffer
	h, err := b.Exec(ictx(t), sandbox.ExecSpec{Argv: []string{"sh", "-c", "trap '' TERM; sleep 60; echo done"}, Cwd: cwd, Stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let the trap install
	if err := h.Kill(); err != nil {   // TERM (ignored) then KILL by exec_id
		t.Fatal(err)
	}
	info, werr := h.Wait()
	if werr != nil {
		t.Fatal(werr)
	}
	if info.Code != 128+9 {
		t.Fatalf("signalled exec: %+v, want 137 (SIGKILL)", info)
	}
}

// `hisn ls --prune` at startup (Reconcile) removes only provably stale dirs;
// the live session this test just started survives it.
func TestIntegrationPruneKeepsLiveSessions(t *testing.T) {
	b1, cwd := realBackend(t)
	istart(t, b1, "wakil-it-prune", cwd)
	// A second backend over the same state: Reconcile runs ls --prune first.
	b2, err := New(Options{
		Binary: b1.opts.Binary, StateDir: b1.opts.StateDir, HomeRoot: b1.opts.HomeRoot,
		HostCwd: cwd, EnvPass: b1.opts.EnvPass,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b2.Reconcile(ictx(t)); err != nil {
		t.Fatal(err)
	}
	sessions, err := b1.listSessions(ictx(t))
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := sessions["wakil-it-prune"]; !ok || !e.Live() {
		t.Fatalf("prune removed or lost the live session: %+v ok=%v", e, ok)
	}
}

// A ctl exec cancelled AFTER the daemon accepted it is stopped by exec_id
// (TERM → 5 s → KILL) and Wait returns promptly: within 6 s, with the latched
// signalled status, and nothing of the program is left running in the session.
//
// "Nothing left running" is checked from inside the session (a /proc scan, the
// only evidence that does not depend on hisn-ls/v1 carrying an exec list — its
// documented row is {id,state,…}) and, defensively, against the `hisn ls
// --json` row: the session must still be live, and if the row exposes an exec
// list under a known key it must be empty.
func TestIntegrationCancelAfterAccept(t *testing.T) {
	b, cwd := realBackend(t)
	istart(t, b, "wakil-it-cancel", cwd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := b.Exec(ctx, sandbox.ExecSpec{Argv: []string{"sleep", "60"}, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let the program start
	start := time.Now()
	cancel()
	type res struct {
		info sandbox.ExitInfo
		err  error
	}
	done := make(chan res, 1)
	go func() { i, e := h.Wait(); done <- res{i, e} }()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Wait: %v", r.err)
		}
		if r.info.Lost || r.info.Code == 0 {
			t.Fatalf("cancelled exec reported %+v, want a signalled (non-zero, not lost) status", r.info)
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("Wait still blocked %v after cancel (limit 6s)", time.Since(start))
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("cancel took %v, limit 6s", d)
	}

	// 1. Inside the session: no sleep 60 survives. ("slee[p]" keeps this very
	// command line from matching itself.)
	out, info := irun(t, b, cwd, "sh", "-c",
		`n=0; for d in /proc/[0-9]*; do tr '\0' ' ' < $d/cmdline 2>/dev/null | grep -q 'slee[p] 60' && n=$((n+1)); done; echo running=$n`)
	if info.Code != 0 || !strings.Contains(out, "running=0") {
		t.Fatalf("a cancelled exec is still running in the session: %q %+v", out, info)
	}

	// 2. hisn ls --json: the session is live, and any exec list is empty.
	raw, err := b.output(ictx(t), false, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &doc); err != nil {
		t.Fatalf("ls --json: %v: %q", err, raw)
	}
	found := false
	for _, row := range doc.Sessions {
		var id, state string
		_ = json.Unmarshal(row["id"], &id)
		if id != "wakil-it-cancel" {
			continue
		}
		found = true
		_ = json.Unmarshal(row["state"], &state)
		if state != "live" {
			t.Fatalf("session state = %q after cancel, want live", state)
		}
		for _, k := range []string{"execs", "running_execs", "exec_ids"} {
			if v, ok := row[k]; ok {
				var list []json.RawMessage
				if json.Unmarshal(v, &list) == nil && len(list) != 0 {
					t.Fatalf("hisn ls --json shows running execs under %q after cancel: %s", k, v)
				}
			}
		}
	}
	if !found {
		t.Fatalf("session missing from hisn ls --json: %q", raw)
	}
}
