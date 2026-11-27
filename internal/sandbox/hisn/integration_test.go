package hisn

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/browser"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/sandbox"
	"github.com/treeol/wakil/internal/staging"
	"github.com/treeol/wakil/internal/timeline"
)

// Integration tests against a REAL hisn. They SKIP (never fail) when hisn is
// missing, too old, or `hisn check` reports the host cannot run sessions
// (e.g. no unprivileged user namespaces, as in many build sandboxes).
// With WAKIL_HISN_REQUIRE=1 a preflight/version failure is a FAIL instead:
// acceptance runs set it so a green run proves the tests actually ran
// against the 0.6.7 binary (Mashūra round 1: without it the suite can go
// green without ever touching 0.6.7).
//
// Run on a host with hisn >= 0.2.0:
//
//	go test -count=1 -v -timeout 5m -run TestIntegration ./internal/sandbox/hisn/
//
// On hisn 0.4.0 `hisn run --detach` does NOT return: its supervisor IS the
// session, and Start keeps the process as the session's lifetime handle. From
// 0.4.1 on it exits 0 once the detached supervisor is up, and Start tracks the
// pid in <session-dir>/supervisor.pid instead. Every test stops and prunes its
// sessions (cleanupSessions) and TestMain sweeps leftovers. Every
// Start error carries the supervisor's stderr (captured with `-v`) and names
// <state>/<chat>.supervisor.log, which holds it for every start.
//
// Set WAKIL_HISN_BIN to test a specific binary.

// hisnRequire reports WAKIL_HISN_REQUIRE=1: preflight/version skips become
// failures so acceptance runs cannot pass without exercising the real binary.
func hisnRequire() bool { return os.Getenv("WAKIL_HISN_REQUIRE") == "1" }

// requireSkip fails when hisnRequire, else skips (the pre-0.6.7 default for
// host-capability gates like `hisn check`).
func requireSkip(t *testing.T, format string, args ...any) {
	t.Helper()
	if hisnRequire() {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

func realBackend(t *testing.T, grantable ...string) (*Backend, string) {
	t.Helper()
	bin := os.Getenv("WAKIL_HISN_BIN")
	if bin == "" {
		p, err := osexec.LookPath("hisn")
		if err != nil {
			requireSkip(t, "hisn not found on PATH")
		}
		bin = p
	}
	// 0.2.1 gate: --ready-fd, exec_id, --prune, stale-kill, session.shm/tmp_size.
	if out, err := osexec.Command(bin, "--version").Output(); err != nil {
		requireSkip(t, "hisn unusable: %v", err)
	} else if v, err := ParseVersion(string(out)); err != nil || !VersionAtLeast(v, [3]int{0, 2, 1}) {
		requireSkip(t, "hisn >= 0.2.1 required (got %q, err %v)", strings.TrimSpace(string(out)), err)
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
		requireSkip(t, "hisn unusable on this host: %v", err)
	}
	cleanupSessions(t, b)
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

// irunSign is irun with the signing env injected (F9). The BackendExecutor
// layer injects GIT_CONFIG_* at COMMAND scope on every exec; the integration
// test talks to the Backend directly, so the env must be passed explicitly.
func irunSign(t *testing.T, b *Backend, cwd string, argv ...string) (string, sandbox.ExitInfo) {
	t.Helper()
	var out bytes.Buffer
	h, err := b.Exec(ictx(t), sandbox.ExecSpec{Argv: argv, Cwd: cwd, Stdout: &out, Env: signingExecEnv(b)})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	info, err := h.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), info
}

// signingExecEnv builds the GIT_CONFIG_* env map that BackendExecutor.signingEnv
// would inject (F9). Used by the integration test to simulate the
// BackendExecutor layer when calling the Backend directly.
func signingExecEnv(b *Backend) map[string]string {
	if b.opts.SigningKey == "" {
		return nil
	}
	type kv struct{ k, v string }
	pairs := []kv{
		{"user.signingkey", "key::" + b.opts.SigningKey},
		{"gpg.format", "ssh"},
		{"gpg.ssh.allowedSignersFile", filepath.Join(SandboxHome, ".wakil-allowed-signers")},
		{"gpg.ssh.program", "ssh-keygen"},
		{"commit.gpgsign", "true"},
		{"tag.gpgsign", "true"},
	}
	env := map[string]string{"GIT_CONFIG_COUNT": fmt.Sprintf("%d", len(pairs))}
	for i, p := range pairs {
		env[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)] = p.k
		env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)] = p.v
	}
	return env
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
	} else if v, err := ParseVersion(string(out)); err != nil || !VersionAtLeast(v, [3]int{0, 3, 3}) {
		t.Skipf("hisn >= 0.3.3 required for ssh_agent (got %q)", strings.TrimSpace(string(out)))
	}
	root := t.TempDir()
	cwd := filepath.Join(root, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, _ = filepath.EvalSymlinks(cwd)

	// A host known_hosts with a marker line: inside the session the file at
	// KnownHostsMount must show exactly this content (src:dst mount, 0.3.3).
	const khMarker = "wakil-it-known-hosts-marker ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMARKER"
	khHost := filepath.Join(root, "known_hosts")
	if err := os.WriteFile(khHost, []byte(khMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	b, err := New(Options{
		Binary:     bin,
		StateDir:   filepath.Join(root, "state"),
		HomeRoot:   filepath.Join(root, "home"),
		HostCwd:    cwd,
		SSHAgent:   true,
		SSHKeys:    []string{fp},
		EnvPass:    []string{"CI", "GOFLAGS"},
		Surface:    Surface{RiskAck: "signing"},
		SigningKey: pubKeyLine, // F5: signing key for the session gitconfig
		GitName:    "Wakil Test",
		GitEmail:   "test@wakil.test",
		KnownHosts: khHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := b.Preflight(ctx); err != nil {
		requireSkip(t, "hisn unusable on this host: %v", err)
	}
	cleanupSessions(t, b)
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	// The resolved binary must be exactly 0.6.7: the seat-role gate and the
	// ssh/sign journal records this test asserts on exist only there. A
	// PATH-fallback run against an older hisn must fail here, never pass.
	if got := b.ver; got != [3]int{0, 6, 7} {
		t.Fatalf("the signing test requires hisn exactly 0.6.7 (got %d.%d.%d from %s)", got[0], got[1], got[2], bin)
	}

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

	// known_hosts is mounted read-only at the in-session path: the host file's
	// content must be readable there, and GIT_SSH_COMMAND must name that path.
	kh, khInfo := irun(t, b, "", "sh", "-c", "cat "+KnownHostsMount+" 2>&1; echo; echo \"$GIT_SSH_COMMAND\"")
	if khInfo.Code != 0 {
		t.Fatalf("reading %s inside the session failed: %q %+v", KnownHostsMount, kh, khInfo)
	}
	if !strings.Contains(kh, khMarker) {
		t.Errorf("in-session %s does not carry the host known_hosts content %q:\n%s", KnownHostsMount, khMarker, kh)
	}
	if !strings.Contains(kh, "UserKnownHostsFile="+KnownHostsMount) {
		t.Errorf("GIT_SSH_COMMAND inside the session does not name %s:\n%s", KnownHostsMount, kh)
	}

	// The host socket path must not appear inside the sandbox.
	if strings.Contains(out, socket) {
		t.Errorf("host SSH_AUTH_SOCK path leaked into the sandbox: %s appears in:\n%s", socket, out)
	}

	// F5+F8+F9: make a signed commit inside the session and verify the signature
	// on the host side. The session gitconfig (written by withGitConfig under
	// HomeRoot on the host, pointed at by GIT_CONFIG_GLOBAL=<SandboxHome>/.wakil-
	// gitconfig inside the session) carries user.name and user.email. Signing
	// settings are injected at COMMAND scope via GIT_CONFIG_* env vars (F9) so a
	// repo-local .git/config cannot shadow them. The agent proxy forwards the
	// key. Nothing is set inside the session by the test — the product path
	// (host file + in-session env) is what is tested.
	if _, info := irunSign(t, b, cwd, "git", "init", "-q"); info.Code != 0 {
		t.Fatalf("git init inside session failed: %+v", info)
	}

	// 0.5.1 per-repo point: a repo-local user.signingkey naming a REAL, different
	// key. The repo's key is resolved on the HOST at plan time (wiring), and the
	// session signs with the resolved key at COMMAND scope — the repo-local
	// setting inside the session cannot change it. Generate a second throwaway
	// key and set IT as the repo-local signingkey AFTER the session key was
	// resolved (the session carries the first key); the signature must come from
	// the session key, never the repo-local one.
	otherKey := filepath.Join(keyDir, "other_ed25519")
	if _, err := osexec.Command(keygenPath, "-t", "ed25519", "-N", "", "-f", otherKey).Output(); err != nil {
		t.Skipf("ssh-keygen failed (second key): %v", err)
	}
	otherFpOut, err := osexec.Command(keygenPath, "-lf", otherKey+".pub").Output()
	if err != nil {
		t.Skipf("ssh-keygen -lf failed (second key): %v", err)
	}
	otherFields := strings.Fields(strings.TrimSpace(string(otherFpOut)))
	if len(otherFields) < 2 || !strings.HasPrefix(otherFields[1], "SHA256:") {
		t.Skipf("could not parse second key fingerprint from %q", string(otherFpOut))
	}
	otherFp := otherFields[1]
	if _, info := irunSign(t, b, cwd, "git", "config", "--local", "user.signingkey", otherKey+".pub"); info.Code != 0 {
		t.Fatalf("git config --local user.signingkey failed: %+v", info)
	}

	// Verify the enforced signing key is visible at command scope, not the
	// repo-local one.
	gcOut, gcInfo := irunSign(t, b, cwd, "git", "config", "--show-origin", "--get", "user.signingkey")
	if gcInfo.Code != 0 {
		t.Fatalf("git config --show-origin --get user.signingkey inside session failed: %q %+v", gcOut, gcInfo)
	}
	if !strings.Contains(gcOut, "command line:") {
		t.Errorf("user.signingkey not reported from command line:\n%s", gcOut)
	}
	if !strings.Contains(gcOut, "key::") {
		t.Errorf("user.signingkey value is not a key:: literal:\n%s", gcOut)
	}
	if strings.Contains(gcOut, "/nonexistent/path/key.pub") {
		t.Errorf("repo-local signingkey leaked past the command-scope override:\n%s", gcOut)
	}
	if strings.Contains(gcOut, otherKey+".pub") {
		t.Errorf("repo-local signingkey %s leaked past the command-scope override:\n%s", otherKey+".pub", gcOut)
	}
	gpgOut, gpgInfo := irunSign(t, b, cwd, "git", "config", "--get", "commit.gpgsign")
	if gpgInfo.Code != 0 || strings.TrimSpace(gpgOut) != "true" {
		t.Fatalf("commit.gpgsign inside session = %q (code %d), want true", gpgOut, gpgInfo.Code)
	}
	if _, info := irunSign(t, b, cwd, "sh", "-c", "echo test > file.txt"); info.Code != 0 {
		t.Fatalf("write file inside session failed: %+v", info)
	}
	if _, info := irunSign(t, b, cwd, "git", "add", "file.txt"); info.Code != 0 {
		t.Fatalf("git add inside session failed: %+v", info)
	}
	// The commit message goes in through a file written HERE, on the host, so
	// it is in no argv, env or cwd: hisn logs every exec's argv in
	// exec/request by design, and a message passed with -m would be in the
	// journal through argv, not through signing. The marker is random per run
	// so nothing else in the journal can collide with it. The file stays
	// untracked (only file.txt was added).
	markerRand := make([]byte, 16)
	if _, err := rand.Read(markerRand); err != nil {
		t.Fatalf("random commit marker: %v", err)
	}
	commitMarker := "wakil-it-payload-" + hex.EncodeToString(markerRand)
	if err := os.WriteFile(filepath.Join(cwd, ".wakil-it-commitmsg"), []byte(commitMarker+"\n"), 0o600); err != nil {
		t.Fatalf("writing the commit message file: %v", err)
	}
	commitOut, commitInfo := irunSign(t, b, cwd, "git", "commit", "-F", ".wakil-it-commitmsg")
	if commitInfo.Code != 0 {
		t.Fatalf("git commit inside session failed: %q %+v", commitOut, commitInfo)
	}
	// Verify the commit is signed on the host side. The host git needs
	// gpg.ssh.allowedSignersFile to verify SSH signatures; point it at the
	// allowed_signers file written under HomeRoot (the host copy).
	signOut, err := func() ([]byte, error) {
		cmd := osexec.Command("git", "-C", cwd,
			"-c", "gpg.format=ssh",
			"-c", "gpg.ssh.allowedSignersFile="+filepath.Join(b.opts.HomeRoot, ".wakil-allowed-signers"),
			"log", "-1", "--format=%G?")
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		return cmd.Output()
	}()
	if err != nil {
		t.Fatalf("git log on host failed: %v", err)
	}
	// %G? = G for a good signature, B for a bad signature, U for a good
	// signature with unknown validity, N for no signature.
	signStatus := strings.TrimSpace(string(signOut))
	if signStatus != "G" {
		t.Errorf("signed commit not verified on host: %%G?=%q (want G — the allowed-signers file names exactly this key principal)", signStatus)
	}
	// 0.5.1: the signature must be made by the RESOLVED session key (the
	// repo's key at plan time), not the repo-local key set inside the session.
	keyOut, err := func() ([]byte, error) {
		cmd := osexec.Command("git", "-C", cwd,
			"-c", "gpg.format=ssh",
			"-c", "gpg.ssh.allowedSignersFile="+filepath.Join(b.opts.HomeRoot, ".wakil-allowed-signers"),
			"log", "-1", "--format=%GK")
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		return cmd.Output()
	}()
	if err != nil {
		t.Fatalf("git log %%GK on host failed: %v", err)
	}
	signingKeyFp := strings.TrimSpace(string(keyOut))
	if signingKeyFp == "" {
		t.Fatal("commit carries no signature key fingerprint (percent-GK empty)")
	}
	if signingKeyFp == otherFp {
		t.Errorf("commit signed by the repo-local key %s, not the resolved session key %s", otherFp, fp)
	}
	if signingKeyFp != fp {
		t.Errorf("commit signed by %s, want the resolved session key %s", signingKeyFp, fp)
	}

	// 0.6.7: the sign must be journalled. hisn records one ssh/sign record
	// per sign request (key fingerprint, seat, exec id, result — never the
	// data being signed, never the signature). Read the journal through
	// b.output so the invocation inherits the session's XDG_RUNTIME_DIR from
	// b.baseEnv() (clientEnvAllow carries it). Per `journal --help` the
	// --json form prints "the records unchanged as JSON (one per line)", so
	// the output is a stream of JSON lines, not one document. On >= 0.6.7
	// this is a hard fail, never a skip: Preflight passing means the binary
	// supports journal, so a missing record is a real regression. The
	// session is seat-less (Wakil passes no seat flag), and hisn 0.6.7
	// allows signing in that mode, so the seat field is present and null.
	// A seated session in observer role would be refused with
	// SSH_AGENT_FAILURE (commit.gpgsign=true already fails the commit) —
	// the `observer`/other-value failure below is the forward guard for a
	// future seat-adopting feature.
	jout, jerr := b.output(ictx(t), false, "journal", "--session", "wakil-it-ssh-agent", "--json")
	if jerr != nil {
		t.Fatalf("hisn journal --session wakil-it-ssh-agent --json failed: %v\n%s", jerr, jout)
	}
	// The commit exec's real id: the journal's exec records carry the argv
	// that was run; the commit is the exec whose argv is `git commit`.
	// `data.exec_id` on a sign record must equal it. (No exec-id plumbing
	// exists on ExecHandle, so correlation through the journal is the
	// binding — never the ids mentioned by the sign records themselves.
	// Matched on the "exec" kind only if records carry an argv sibling;
	// see the family-name heuristic below.)
	var commitExecID string
	var signRecs []map[string]any
	var parsedRecs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(jout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "hisn:") {
			continue // envelope note lines (no-session fallbacks) are not records
		}
		var rec map[string]any
		if jerr := json.Unmarshal([]byte(line), &rec); jerr != nil {
			t.Fatalf("journal line is not a JSON record: %v\n%s", jerr, line)
		}
		parsedRecs = append(parsedRecs, rec)
		if kind, _ := rec["kind"].(string); kind == "ssh/sign" {
			signRecs = append(signRecs, rec)
		}
	}
	// Find the commit exec in the journal via the req_id link (see the
	// helper): the exec/request record carries the `git commit` argv and
	// the top-level client.req_id; the exec/exit record with the same
	// req_id carries the exec_id; exactly one ssh/sign record must name
	// that exec_id. Zero or two+ matches at any step fails with the
	// step and the count named. (The 0.6.7 journal splits one exec
	// across records: argv and exec_id never appear in the same record.)
	commitExecID, stepErr := journalCommitExecID(parsedRecs)
	if stepErr != "" {
		t.Fatalf("cannot correlate the commit exec in the journal: %s\n%s", stepErr, jout)
	}
	if commitExecID == "" {
		t.Fatalf("commit exec correlation returned an empty id:\n%s", jout)
	}
	if len(signRecs) == 0 {
		t.Fatalf("no ssh/sign record in the journal:\n%s", jout)
	}
	// The matched sign record (the helper already required exactly one
	// for the commit exec) — its content is verified below.
	var commitSigns []map[string]any
	for _, rec := range signRecs {
		data, _ := rec["data"].(map[string]any)
		if eid, _ := data["exec_id"].(string); eid == commitExecID {
			commitSigns = append(commitSigns, rec)
		}
	}
	if len(commitSigns) != 1 {
		t.Fatalf("want exactly 1 ssh/sign record for the commit exec %s, got %d (total ssh/sign %d):\n%s", commitExecID, len(commitSigns), len(signRecs), jout)
	}
	// Every ssh/sign record must be ok with the throwaway fingerprint —
	// a second broken sign anywhere in the session must fail too.
	allowed := map[string]bool{"attributed": true, "exec_id": true, "fingerprint": true, "result": true, "seat": true}
	var okCount int
	for _, rec := range signRecs {
		data, _ := rec["data"].(map[string]any)
		if data == nil {
			t.Fatalf("ssh/sign record without a data object: %v", rec)
		}
		// Closed data key set — any unknown key is a possible payload
		// passthrough and fails closed.
		for k := range data {
			if !allowed[k] {
				t.Errorf("ssh/sign data carries unexpected key %q (possible signed-payload passthrough): %v", k, rec)
			}
		}
		// Types: short strings / bool only. A long or composite value
		// (payload, signature bytes) fails.
		if attr, isBool := data["attributed"].(bool); !isBool || !attr {
			t.Errorf("ssh/sign attributed = %v, want true (the bind rests on hisn attributing the sign to this exec)", data["attributed"])
		}
		for _, k := range []string{"exec_id", "fingerprint", "result"} {
			if s, isStr := data[k].(string); !isStr || len(s) > 128 {
				t.Errorf("ssh/sign %s = %v (len > 128 or non-string), want a short string", k, data[k])
			}
		}
		result, _ := data["result"].(string)
		if result != "ok" {
			t.Errorf("ssh/sign record with result %q (want ok): %v", result, rec)
			continue
		}
		okCount++
		// Seat: present-but-null for this seat-less session; owner/member
		// for a seated one; observer or any other value fails.
		seatRaw, seatPresent := data["seat"]
		if !seatPresent {
			t.Errorf("ssh/sign record without a seat field: %v", rec)
		} else if seatRaw == nil {
			// present and null: the seat-less mode — signing is allowed
			// (0.6.7 host result: seat is null, result ok).
		} else if seat, isStr := seatRaw.(string); !isStr || (seat != "owner" && seat != "member") {
			t.Errorf("ssh/sign seat = %v, want null (seat-less session), owner, or member", seatRaw)
		}
		if f, _ := data["fingerprint"].(string); f != fp {
			t.Errorf("ssh/sign fingerprint = %q, want the throwaway key %s", f, fp)
		}
	}
	if okCount == 0 {
		t.Fatalf("no ssh/sign record with result ok:\n%s", jout)
	}
	if okCount != len(signRecs) {
		t.Fatalf("%d ssh/sign record(s) failed out of %d:\n%s", len(signRecs)-okCount, len(signRecs), jout)
	}
	// No signed payload / signature bytes anywhere in the raw journal.
	// What crosses the agent proxy is the SSHSIG blob (raw bytes) and the
	// raw signature bytes; the PEM header never appears. Reject the
	// blob's markers and the commit's own signature in base64 and in
	// hex-encoded-bytes form.
	if strings.Contains(jout, "SSHSIG") || strings.Contains(jout, "U1NIU0lH") ||
		strings.Contains(jout, "SSH SIGNATURE") {
		t.Errorf("the journal carries ssh-signature marker bytes:\n%s", jout)
	}
	// Extract the commit's gpgsig (host git, raw commit object) and
	// require both the whole decoded SSHSIG blob and its inner signature
	// bytes — the bytes that actually cross the agent proxy as the agent
	// reply — to be absent from the journal in raw, hex, and all three
	// base64 alignments.
	catOut, err := func() ([]byte, error) {
		cmd := osexec.Command("git", "-C", cwd, "cat-file", "commit", "HEAD")
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		return cmd.Output()
	}()
	if err != nil {
		t.Fatalf("git cat-file commit HEAD on host failed: %v", err)
	}
	sigBlob, parseErr := commitSignatureBlob(string(catOut))
	if parseErr != "" {
		t.Fatalf("cannot read the commit's signature from the commit object: %s", parseErr)
	}
	sigInner, parseErr := extractSSHSigInner(sigBlob)
	if parseErr != "" {
		t.Fatalf("cannot extract the inner signature from the commit's SSHSIG blob: %s", parseErr)
	}
	if signatureAppearsInJournal(jout, sigBlob) {
		t.Errorf("the journal carries the commit's signature bytes (whole blob):\n%s", jout)
	}
	if signatureAppearsInJournal(jout, sigInner) {
		t.Errorf("the journal carries the commit's inner signature bytes:\n%s", jout)
	}

	// The signed payload must not be in the journal either. Git signs the
	// commit object without its gpgsig header; ssh-keygen hashes it and the
	// agent receives only H(payload) inside the SSHSIG preimage, so a leak of
	// the sign request carries the digest, not the message. The needles are
	// the marker (the message), the tree and commit ids, and the digest of
	// the payload.
	//
	// First prove the digest is the one that was signed: an off-by-one in
	// commitPayloadBytes would make the digest needle search for nothing and
	// pass on any journal. The signature must verify over exactly those
	// bytes (the public key is inside the signature: no agent, no real key),
	// and the hash algorithm must be one we compute.
	payloadBytes, parseErr := commitPayloadBytes(string(catOut))
	if parseErr != "" {
		t.Fatalf("cannot read the commit's signed payload: %s", parseErr)
	}
	if msg := sshVerifyPayload(sigBlob, payloadBytes); msg != "" {
		t.Fatalf("the extracted commit payload is not what git signed: %s", msg)
	}
	hashAlg, parseErr := sshsigHashAlg(sigBlob)
	if parseErr != "" {
		t.Fatalf("cannot read the SSHSIG hash algorithm: %s", parseErr)
	}
	payloadDig, parseErr := payloadDigest(hashAlg, payloadBytes)
	if parseErr != "" {
		t.Fatal(parseErr)
	}
	hostGit := func(args ...string) string {
		t.Helper()
		cmd := osexec.Command("git", append([]string{"-C", cwd}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v on host failed: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	commitID := hostGit("rev-parse", "HEAD")
	treeID := hostGit("rev-parse", "HEAD^{tree}")
	var needles []payloadNeedle
	needles = append(needles, payloadNeedle{name: "marker", data: []byte(commitMarker), argvBlind: true})
	for _, id := range []struct{ name, text string }{{"commit", commitID}, {"tree", treeID}} {
		raw, err := hex.DecodeString(id.text)
		if err != nil || (len(id.text) != 40 && len(id.text) != 64) {
			t.Fatalf("%s id %q is not a 40 or 64 digit hex object id: %v", id.name, id.text, err)
		}
		needles = append(needles,
			payloadNeedle{name: id.name, data: raw},
			payloadNeedle{name: id.name + "-text", data: []byte(id.text)})
	}
	needles = append(needles, payloadNeedle{name: "payload-" + hashAlg, data: payloadDig})
	if msg := validatePayloadNeedles(needles); msg != "" {
		t.Fatalf("payload needles unusable: %s", msg)
	}
	// Canary: the scanner must see the digest it is about to declare absent.
	canary := `{"kind":"ssh/sign","data":{"x":"` + hex.EncodeToString(payloadDig) + `"}}`
	if got := journalPayloadLeaks(jout+"\n"+canary, needles); len(got) == 0 {
		t.Fatalf("canary: journalPayloadLeaks did not report a journal that carries the payload digest")
	}
	if leaks := journalPayloadLeaks(jout, needles); len(leaks) > 0 {
		t.Errorf("the journal carries signed data (%s):\n%s", strings.Join(leaks, ", "), jout)
	}
}

// journalCommitExecID correlates the journal's `git commit` exec to the
// ssh/sign record that names it, returning the exec_id and a step error
// naming the first step that did not yield exactly one match ("" on
// success). The 0.6.7 journal splits one exec across records and never
// puts argv and exec_id in the same record:
//
//   - an exec/request record carries data.argv and the top-level
//     client.req_id;
//   - the exec/exit record with the same client.req_id carries
//     data.exec_id and data.exit_code;
//   - an ssh/sign record names that exec_id in data.
//
// The link is client.req_id (with data.seq as corroboration). Every
// step requires exactly one match and a non-empty, correctly-typed
// identifier — an empty string never binds — and any failure is a step
// error, never a guess. This pins the 0.6.7 journal schema; the unit
// test covers it against recorded-fixture shapes.
func journalCommitExecID(recs []map[string]any) (string, string) {
	// Step 1: exactly one exec/request record whose data.argv starts
	// with `git` then `commit`.
	var reqs []map[string]any
	for _, rec := range recs {
		kind, _ := rec["kind"].(string)
		if kind != "exec/request" {
			continue
		}
		data, _ := rec["data"].(map[string]any)
		argv, _ := data["argv"].([]any)
		if len(argv) < 2 {
			continue
		}
		a, aStr := argv[0].(string)
		b, bStr := argv[1].(string)
		if aStr && bStr && a == "git" && b == "commit" {
			reqs = append(reqs, rec)
		}
	}
	if len(reqs) != 1 {
		return "", fmt.Sprintf("step 1: want exactly 1 exec/request for `git commit`, got %d", len(reqs))
	}
	// Step 2: the commit request's top-level client.req_id must be a
	// non-empty string.
	reqID, stepErr := journalRecordReqID(reqs[0])
	if stepErr != "" {
		return "", "step 2: " + stepErr
	}
	reqSeq, seqErr := journalRecordSeq(reqs[0])
	if seqErr != "" {
		return "", "step 2: " + seqErr
	}
	// Step 3: exactly one exec/exit record with the same client.req_id,
	// corroborated by data.seq, with exit_code 0.
	var exits []map[string]any
	for _, rec := range recs {
		kind, _ := rec["kind"].(string)
		if kind != "exec/exit" {
			continue
		}
		if id, errStr := journalRecordReqID(rec); errStr == "" && id == reqID {
			exits = append(exits, rec)
		}
	}
	if len(exits) != 1 {
		return "", fmt.Sprintf("step 3: want exactly 1 exec/exit for req_id %q, got %d", reqID, len(exits))
	}
	exitRec := exits[0]
	exitSeq, seqErr := journalRecordSeq(exitRec)
	if seqErr != "" {
		return "", "step 3: " + seqErr
	}
	if exitSeq != reqSeq {
		return "", fmt.Sprintf("step 3: seq mismatch (request %v, exit %v)", reqSeq, exitSeq)
	}
	exitData, _ := exitRec["data"].(map[string]any)
	exitID, idOK := exitData["exec_id"].(string)
	if !idOK || exitID == "" {
		return "", "step 3: exec/exit carries no non-empty data.exec_id"
	}
	exitCode, codeOK := journalRecordExitCode(exitRec)
	if !codeOK || exitCode != 0 {
		return "", fmt.Sprintf("step 3: want exit_code 0, got %v", exitData["exit_code"])
	}
	// Step 4: exactly one ssh/sign record naming that exec_id.
	var signs []map[string]any
	for _, rec := range recs {
		kind, _ := rec["kind"].(string)
		if kind != "ssh/sign" {
			continue
		}
		data, _ := rec["data"].(map[string]any)
		if eid, isStr := data["exec_id"].(string); isStr && eid == exitID {
			signs = append(signs, rec)
		}
	}
	if len(signs) != 1 {
		return "", fmt.Sprintf("step 4: want exactly 1 ssh/sign for exec_id %q, got %d", exitID, len(signs))
	}
	return exitID, ""
}

// journalRecordReqID returns the top-level client.req_id of a journal
// record. Missing, non-string, or empty is a step error, not an empty
// string that could match another empty string.
func journalRecordReqID(rec map[string]any) (string, string) {
	client, _ := rec["client"].(map[string]any)
	switch v := client["req_id"].(type) {
	case string:
		if v == "" {
			return "", "client.req_id is empty"
		}
		return v, ""
	default:
		return "", "client.req_id is missing or not a string"
	}
}

// journalRecordSeq returns data.seq as an integer. A missing or
// non-integer seq is a step error (the 0.6.7 shape always carries it).
func journalRecordSeq(rec map[string]any) (int, string) {
	data, _ := rec["data"].(map[string]any)
	switch v := data["seq"].(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, fmt.Sprintf("data.seq = %v, want an integer", v)
		}
		return int(v), ""
	default:
		return 0, "data.seq is missing or not a number"
	}
}

// journalRecordExitCode returns data.exit_code as an integer.
func journalRecordExitCode(rec map[string]any) (int, bool) {
	data, _ := rec["data"].(map[string]any)
	switch v := data["exit_code"].(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	default:
		return 0, false
	}
}

// TestCommitSignatureParser unit-tests the gpgsig parser and leak check.
// The fixture is a real SSH signature produced by a throwaway key in the
// test's own temp dir (never a real key, never the host agent: SSH_AUTH_SOCK
// is cleared and HOME points at a temp dir), assembled into a commit object
// by hand. "Expected" is outside the parser: the decoded .sig body.
func TestCommitSignatureParser(t *testing.T) {
	keygenPath, err := osexec.LookPath("ssh-keygen")
	if err != nil {
		t.Skipf("ssh-keygen not found on PATH: %v", err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if out, err := osexec.Command(keygenPath, "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen throwaway key failed: %v\n%s", err, out)
	}
	msgPath := filepath.Join(dir, "msg")
	if err := os.WriteFile(msgPath, []byte("test signed commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Sign with the throwaway key FILE (never the agent). SSH_AUTH_SOCK
	// stays empty and HOME is a temp dir so ssh-keygen cannot fall back.
	sigPath := msgPath + ".sig"
	env := append(os.Environ(), "HOME="+dir)
	signEnv := append(env, "SSH_AUTH_SOCK=")
	// Clear any inherited SSH_AUTH_SOCK by prepending the empty value.
	signCmd := osexec.Command(keygenPath, "-Y", "sign", "-f", keyPath, "-n", "git", "-P", "", msgPath)
	signCmd.Env = signEnv
	if out, err := signCmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -Y sign failed: %v\n%s", err, out)
	}
	sigBytes, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatalf("reading .sig file: %v", err)
	}
	sigText := string(sigBytes)
	sigLines := strings.Split(strings.TrimSuffix(sigText, "\n"), "\n")
	if len(sigLines) < 5 || sigLines[0] != "-----BEGIN SSH SIGNATURE-----" ||
		sigLines[len(sigLines)-1] != "-----END SSH SIGNATURE-----" {
		t.Fatalf("unexpected .sig armor shape: %q", sigText)
	}
	var body strings.Builder
	for _, l := range sigLines[1 : len(sigLines)-1] {
		body.WriteString(l)
	}
	wantBlob, err := base64.StdEncoding.DecodeString(body.String())
	if err != nil {
		t.Fatalf("decoding .sig body: %v", err)
	}
	if !bytes.HasPrefix(wantBlob, []byte("SSHSIG")) {
		t.Fatalf("decoded .sig body missing SSHSIG magic: %x", wantBlob[:16])
	}
	wantInner, innerErr := extractSSHSigInner(wantBlob)
	if innerErr != "" {
		t.Fatalf("oracle SSHSIG parse failed: %s", innerErr)
	}
	if len(wantInner) != 64 {
		t.Fatalf("oracle inner signature length = %d, want 64 (ed25519)", len(wantInner))
	}

	// Assemble a commit object: headers (incl. gpgsig with continuation),
	// blank line, message. This is what git cat-file commit prints.
	gpgsigHeader := "gpgsig " + sigLines[0]
	for _, l := range sigLines[1:] {
		gpgsigHeader += "\n " + l
	}
	mkCommit := func(headers string, message string) string {
		return headers + "\n\n" + message
	}
	baseHeaders := "tree " + strings.Repeat("a", 40) + "\n" +
		"author T <t@example.com> 0 +0000\n" +
		"committer T <t@example.com> 0 +0000\n" +
		gpgsigHeader + "\n"
	commitObj := mkCommit(baseHeaders, "test signed commit\n")

	blob, errStr := commitSignatureBlob(commitObj)
	if errStr != "" {
		t.Fatalf("clean commit object failed to parse: %s", errStr)
	}
	if !bytes.Equal(blob, wantBlob) {
		t.Errorf("parsed blob != .sig decoded blob:\n got %x\nwant %x", blob, wantBlob)
	}
	inner, errStr := extractSSHSigInner(blob)
	if errStr != "" {
		t.Fatalf("clean blob failed to yield inner signature: %s", errStr)
	}
	if !bytes.Equal(inner, wantInner) {
		t.Errorf("inner signature != oracle inner signature:\n got %x\nwant %x", inner, wantInner)
	}

	// Header-only scanning: a gpgsig block in the MESSAGE body must not
	// be read as a header (unsigned commit with a fake body).
	bodyOnly := "tree " + strings.Repeat("a", 40) + "\n" +
		"author T <t@example.com> 0 +0000\n" +
		"committer T <t@example.com> 0 +0000\n" +
		"\n" + gpgsigHeader + "\n\ntest signed commit\n"
	if _, errStr := commitSignatureBlob(bodyOnly); errStr == "" {
		t.Errorf("message-body gpgsig block: want a failure, got a clean parse")
	} else if !strings.Contains(errStr, "gpgsig") {
		t.Errorf("message-body gpgsig block: want the error to name gpgsig, got %q", errStr)
	}

	negatives := []struct {
		name   string
		object string
	}{
		{"no gpgsig header", mkCommit("tree "+strings.Repeat("a", 40)+"\nauthor T <t@example.com> 0 +0000\ncommitter T <t@example.com> 0 +0000\n", "test signed commit\n")},
		{"duplicate gpgsig header", mkCommit(baseHeaders+gpgsigHeader+"\n", "test signed commit\n")},
		{"broken armor start", strings.Replace(commitObj, "-----BEGIN SSH SIGNATURE-----", "-----BEGIN PGP SIGNATURE-----", 1)},
		{"continuation without the leading space", strings.Replace(commitObj, "\n "+sigLines[1], "\n"+sigLines[1], 1)},
		{"non-base64 body", "tree " + strings.Repeat("a", 40) + "\n" +
			"author T <t@example.com> 0 +0000\n" +
			"committer T <t@example.com> 0 +0000\n" +
			"gpgsig -----BEGIN SSH SIGNATURE-----\n !!!not-base64!!!\n -----END SSH SIGNATURE-----" +
			"\n\ntest signed commit\n"},
		{"blob without SSHSIG magic", func() string {
			bad := make([]byte, len(wantBlob))
			copy(bad, wantBlob)
			copy(bad, "XXXXXX")
			badBody := base64.StdEncoding.EncodeToString(bad)
			wrapped := ""
			for i := 0; i < len(badBody); i += 64 {
				end := i + 64
				if end > len(badBody) {
					end = len(badBody)
				}
				wrapped += badBody[i:end] + "\n"
			}
			return "tree " + strings.Repeat("a", 40) + "\n" +
				"author T <t@example.com> 0 +0000\n" +
				"committer T <t@example.com> 0 +0000\n" +
				"gpgsig -----BEGIN SSH SIGNATURE-----\n " +
				strings.ReplaceAll(strings.TrimSuffix(wrapped, "\n"), "\n", "\n ") +
				"\n -----END SSH SIGNATURE-----\n" +
				"\ntest signed commit\n"
		}()},
	}
	for _, n := range negatives {
		if b, errStr := commitSignatureBlob(n.object); errStr == "" {
			t.Errorf("%s: want a failure, got a clean parse (%d bytes)", n.name, len(b))
		} else if !strings.Contains(errStr, "gpgsig") && !strings.Contains(errStr, "SSHSIG") && !strings.Contains(errStr, "base64") && !strings.Contains(errStr, "armor") {
			t.Errorf("%s: want a clear message naming gpgsig/SSHSIG/base64/armor, got %q", n.name, errStr)
		}
	}

	// Leak-check needle negative: a journal containing only the inner
	// signature bytes (what the agent reply carries) must trip the check,
	// in hex and in base64.
	if !signatureAppearsInJournal(string(inner), inner) {
		t.Errorf("inner signature in hex: signatureAppearsInJournal missed a hex-encoded inner signature")
	}
	if !signatureAppearsInJournal(base64.StdEncoding.EncodeToString(inner), inner) {
		t.Errorf("inner signature in base64: signatureAppearsInJournal missed a base64-encoded inner signature")
	}
	if signatureAppearsInJournal("{\"kind\":\"ssh/sign\",\"data\":{\"result\":\"ok\"}}", inner) {
		t.Errorf("clean journal: signatureAppearsInJournal tripped on a journal with no signature bytes")
	}
}

// TestSignatureAppearsInJournalEmbedded pins the needle scanner: a needle
// embedded in a longer buffer, at any byte offset, is found in raw, hex,
// upper-case hex and base64 form, and an unrelated needle is not. The base64
// cases matter: the trailing characters of a base64 needle depend on the
// bytes that follow it, so a full-encoding needle misses an embedded leak.
func TestSignatureAppearsInJournalEmbedded(t *testing.T) {
	needle := make([]byte, 64)
	for i := range needle {
		needle[i] = byte(0xA0 + i*3)
	}
	other := make([]byte, 64)
	for i := range other {
		other[i] = byte(0x11 + i*7)
	}
	for prefix := 0; prefix <= 5; prefix++ {
		for suffix := 0; suffix <= 5; suffix++ {
			buf := append(append(bytes.Repeat([]byte{0x5A}, prefix), needle...), bytes.Repeat([]byte{0xC3}, suffix)...)
			forms := map[string]string{
				"raw":       string(buf),
				"hex":       hex.EncodeToString(buf),
				"hex-upper": strings.ToUpper(hex.EncodeToString(buf)),
				"base64":    base64.StdEncoding.EncodeToString(buf),
			}
			for name, journal := range forms {
				if !signatureAppearsInJournal("x"+journal+"y", needle) {
					t.Errorf("%s, prefix %d, suffix %d: embedded needle not found", name, prefix, suffix)
				}
				if signatureAppearsInJournal("x"+journal+"y", other) {
					t.Errorf("%s, prefix %d, suffix %d: unrelated needle reported", name, prefix, suffix)
				}
			}
		}
	}
}

// TestJournalPayloadLeaks unit-tests the payload leak scanner on fake
// journals (nothing real). The scan is argv-blind for the marker needle only:
// hisn logs every exec's argv in exec/request by design, so the marker
// appearing there is not a leak, but it appearing anywhere else is, and
// every other needle is searched over the whole raw line, argv included.
func TestJournalPayloadLeaks(t *testing.T) {
	marker := []byte("wakil-it-payload-0123456789abcdef0123456789abcdef")
	tree := make([]byte, 20)
	for i := range tree {
		tree[i] = byte(0x7e + i*5)
	}
	digest := make([]byte, 64)
	for i := range digest {
		digest[i] = byte(0x21 + i*11)
	}
	needles := []payloadNeedle{
		{name: "marker", data: marker, argvBlind: true},
		{name: "tree", data: tree},
		{name: "payload-sha512", data: digest},
	}
	client := `"client":{"uid":1000,"label":"wakil@fake","label_source":"none","req_id":"fake-1"}`
	leaks := func(lines ...string) []string {
		return journalPayloadLeaks(strings.Join(lines, "\n"), needles)
	}
	check := func(name string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: leaks = %v, want %v", name, got, want)
		}
	}
	m := string(marker)

	cleanReq := `{"kind":"exec/request",` + client + `,"data":{"argv":["git","commit","-F",".msg"],"cwd":"/tmp/ws","op":"exec","seq":19}}`
	check("clean journal", leaks(cleanReq, `{"kind":"ssh/sign","data":{"result":"ok","seat":null}}`))

	// Argv-blind by design: the marker only inside exec/request data.argv.
	argvReq := `{"kind":"exec/request",` + client + `,"data":{"argv":["git","commit","-m","` + m + `"],"cwd":"/tmp/ws","op":"exec","seq":19}}`
	check("marker only in exec/request data.argv", leaks(argvReq))

	// The marker anywhere else is a leak.
	for _, c := range []struct{ name, line string }{
		{"ssh/sign record", `{"kind":"ssh/sign","data":{"result":"ok","note":"` + m + `"}}`},
		{"exec/result record", `{"kind":"exec/result",` + client + `,"data":{"result":"ok","path":"` + m + `"}}`},
		{"exec/request data.cwd", `{"kind":"exec/request",` + client + `,"data":{"argv":["git"],"cwd":"/tmp/` + m + `"}}`},
		{"exec/request data.path", `{"kind":"exec/request",` + client + `,"data":{"argv":["git"],"path":"` + m + `"}}`},
		{"exec/request top-level argv", `{"kind":"exec/request","argv":["` + m + `"],"data":{"cwd":"/tmp/ws"}}`},
		{"unknown kind with data.argv", `{"kind":"exec/other","data":{"argv":["` + m + `"]}}`},
		{"exec/request whose data is not an object", `{"kind":"exec/request","data":"` + m + `"}`},
		{"exec/request without data.argv", `{"kind":"exec/request","data":{"note":"` + m + `"}}`},
		{"non-JSON line", `session ended ` + m},
	} {
		check(c.name, leaks(cleanReq, c.line), "marker")
	}

	// Digests and ids are not argv-blind: found inside data.argv too, in
	// hex, upper-case hex and base64 at any offset.
	embed := func(data []byte) map[string]string {
		out := map[string]string{
			"raw":       string(data),
			"hex":       hex.EncodeToString(data),
			"hex-upper": strings.ToUpper(hex.EncodeToString(data)),
		}
		for prefix := 0; prefix < 3; prefix++ {
			buf := append(bytes.Repeat([]byte{0x5A}, prefix), data...)
			buf = append(buf, 0xC3, 0xC4)
			out["base64+"+strconv.Itoa(prefix)] = base64.StdEncoding.EncodeToString(buf)
		}
		return out
	}
	for _, n := range []payloadNeedle{needles[1], needles[2]} {
		for form, text := range embed(n.data) {
			if strings.ContainsAny(text, "\"\\\n") {
				continue // a raw binary form that cannot sit in a JSON string unescaped
			}
			check(n.name+" "+form+" in ssh/sign", leaks(cleanReq, `{"kind":"ssh/sign","data":{"x":"`+text+`"}}`), n.name)
			check(n.name+" "+form+" in data.argv", leaks(`{"kind":"exec/request","data":{"argv":["`+text+`"]}}`), n.name)
		}
	}

	// Several needles in one journal come back sorted and once each.
	check("two needles", leaks(
		`{"kind":"ssh/sign","data":{"x":"`+hex.EncodeToString(digest)+`"}}`,
		`{"kind":"ssh/sign","data":{"y":"`+m+`"}}`,
		`{"kind":"ssh/sign","data":{"z":"`+m+`"}}`,
	), "marker", "payload-sha512")

	// The needle validator refuses what could pass vacuously.
	if msg := validatePayloadNeedles(needles); msg != "" {
		t.Errorf("valid needles refused: %s", msg)
	}
	for _, c := range []struct {
		name string
		ns   []payloadNeedle
	}{
		{"empty data", []payloadNeedle{{name: "a", data: nil}}},
		{"short data", []payloadNeedle{{name: "a", data: make([]byte, 15)}}},
		{"empty name", []payloadNeedle{{name: "", data: make([]byte, 20)}}},
		{"duplicate name", []payloadNeedle{{name: "a", data: make([]byte, 20)}, {name: "a", data: make([]byte, 21)}}},
		{"no needles", nil},
	} {
		if msg := validatePayloadNeedles(c.ns); msg == "" {
			t.Errorf("%s: validatePayloadNeedles accepted it", c.name)
		}
	}
}

// TestCommitPayloadBytes proves commitPayloadBytes returns exactly the bytes
// that were signed, with a throwaway key file (never the agent: the signing
// env drops SSH_AUTH_SOCK and HOME is a temp dir). The oracle is the payload
// file that was handed to ssh-keygen, not the parser.
func TestCommitPayloadBytes(t *testing.T) {
	headers := "tree " + strings.Repeat("a", 40) + "\n" +
		"author T <t@example.com> 0 +0000\n" +
		"committer T <t@example.com> 0 +0000\n"
	message := "\nwakil-it-payload-fake\n gpgsig-looking indented body line\n"
	payload := headers + message

	// Static cases need no ssh-keygen.
	armor := "gpgsig -----BEGIN SSH SIGNATURE-----\n AAAA\n BBBB\n -----END SSH SIGNATURE-----\n"
	got, errStr := commitPayloadBytes(headers + armor + message)
	if errStr != "" || string(got) != payload {
		t.Errorf("static: got (%q, %q), want (%q, \"\")", got, errStr, payload)
	}
	if _, errStr := commitPayloadBytes(payload); errStr == "" {
		t.Errorf("no gpgsig header: want a failure, got a clean result")
	}
	// A gpgsig line in the message body is not a header: nothing to remove.
	if _, errStr := commitPayloadBytes(headers + message + "gpgsig x\n"); errStr == "" {
		t.Errorf("gpgsig only in the message body: want a failure")
	}
	// Digests.
	for alg, want := range map[string]int{"sha256": 32, "sha512": 64} {
		d, errStr := payloadDigest(alg, []byte(payload))
		if errStr != "" || len(d) != want {
			t.Errorf("payloadDigest(%s): len %d, err %q, want len %d", alg, len(d), errStr, want)
		}
	}
	if _, errStr := payloadDigest("sha1", []byte(payload)); errStr == "" {
		t.Errorf("payloadDigest(sha1): want a failure")
	}

	keygenPath, err := osexec.LookPath("ssh-keygen")
	if err != nil {
		t.Skipf("ssh-keygen not found on PATH: %v", err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if out, err := osexec.Command(keygenPath, "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen throwaway key failed: %v\n%s", err, out)
	}
	payloadPath := filepath.Join(dir, "payload")
	if err := os.WriteFile(payloadPath, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	signCmd := osexec.Command(keygenPath, "-Y", "sign", "-f", keyPath, "-n", "git", "-P", "", payloadPath)
	signCmd.Env = signingTestEnv(dir)
	if out, err := signCmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -Y sign failed: %v\n%s", err, out)
	}
	sigBytes, err := os.ReadFile(payloadPath + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	sigLines := strings.Split(strings.TrimSuffix(string(sigBytes), "\n"), "\n")
	commitObj := headers + "gpgsig " + sigLines[0] + "\n"
	for _, l := range sigLines[1:] {
		commitObj += " " + l + "\n"
	}
	commitObj += message

	extracted, errStr := commitPayloadBytes(commitObj)
	if errStr != "" {
		t.Fatalf("commitPayloadBytes on the signed commit: %s", errStr)
	}
	if !bytes.Equal(extracted, []byte(payload)) {
		t.Fatalf("extracted payload != the bytes that were signed:\n got %q\nwant %q", extracted, payload)
	}
	blob, errStr := commitSignatureBlob(commitObj)
	if errStr != "" {
		t.Fatalf("commitSignatureBlob: %s", errStr)
	}
	if msg := sshVerifyPayload(blob, extracted); msg != "" {
		t.Errorf("a correct payload failed verification: %s", msg)
	}
	tampered := append(append([]byte{}, extracted...), '\n')
	if msg := sshVerifyPayload(blob, tampered); msg == "" {
		t.Errorf("a one-byte-longer payload passed verification: the oracle cannot catch an off-by-one")
	}
	alg, errStr := sshsigHashAlg(blob)
	if errStr != "" || alg != "sha512" {
		t.Errorf("sshsigHashAlg = (%q, %q), want (\"sha512\", \"\")", alg, errStr)
	}
	if _, errStr := sshsigHashAlg(blob[:20]); errStr == "" {
		t.Errorf("sshsigHashAlg on a truncated blob: want a failure")
	}
}

// commitSignatureBlob reads the commit object's gpgsig header from the raw
// object bytes (git cat-file commit HEAD) and returns the base64-decoded
// SSHSIG blob. The header block is scanned only up to the first blank
// line, so a gpgsig-looking line in the message body is never read as a
// header. Exactly one gpgsig header is allowed; a duplicate fails. The
// armor must read -----BEGIN SSH SIGNATURE----- ... -----END SSH SIGNATURE-----
// and the decoded blob must start with SSHSIG. A single trailing empty
// continuation line (version-dependent git output) is tolerated. Every
// failure names what was expected — an empty signature never passes.
func commitSignatureBlob(commitObj string) ([]byte, string) {
	lines := strings.Split(commitObj, "\n")
	var gpgsig []string
	seen := 0
	for _, line := range lines {
		if line == "" {
			break // headers end at the first blank line
		}
		if strings.HasPrefix(line, "gpgsig ") {
			seen++
			if seen > 1 {
				return nil, "commit carries duplicate gpgsig headers"
			}
			gpgsig = append(gpgsig, line)
			continue
		}
		if seen == 0 || !strings.HasPrefix(line, " ") {
			continue
		}
		gpgsig = append(gpgsig, line)
	}
	if len(gpgsig) == 0 {
		return nil, "commit carries no gpgsig header"
	}
	// The first gpgsig line is "gpgsig -----BEGIN SSH SIGNATURE-----";
	// continuation lines carry one leading space, stripped here.
	var text []string
	text = append(text, strings.TrimPrefix(gpgsig[0], "gpgsig "))
	for _, l := range gpgsig[1:] {
		if !strings.HasPrefix(l, " ") {
			return nil, "gpgsig continuation line is missing its leading space"
		}
		text = append(text, l[1:])
	}
	// Tolerate a single trailing empty continuation line.
	for len(text) > 0 && text[len(text)-1] == "" {
		text = text[:len(text)-1]
	}
	if len(text) < 2 {
		return nil, "gpgsig header holds no SSH signature armor"
	}
	if text[0] != "-----BEGIN SSH SIGNATURE-----" || text[len(text)-1] != "-----END SSH SIGNATURE-----" {
		return nil, "gpgsig armor does not start with -----BEGIN SSH SIGNATURE----- or end with -----END SSH SIGNATURE-----"
	}
	var body strings.Builder
	for _, l := range text[1 : len(text)-1] {
		body.WriteString(l)
	}
	if body.Len() == 0 {
		return nil, "gpgsig armor body is empty"
	}
	blob, err := base64.StdEncoding.DecodeString(body.String())
	if err != nil {
		return nil, fmt.Sprintf("gpgsig armor body is not valid base64: %v", err)
	}
	if !bytes.HasPrefix(blob, []byte("SSHSIG")) {
		return nil, "gpgsig blob does not start with SSHSIG"
	}
	if len(blob) == 0 {
		return nil, "gpgsig blob is empty"
	}
	return blob, ""
}

// extractSSHSigInner parses the SSH wire fields inside a decoded SSHSIG
// blob and returns the raw signature bytes inside the final string field.
// Format: "SSHSIG" | uint32 version | string pubkey | string namespace |
// string reserved | string hash-algorithm | string signature. The
// signature field is itself an SSH string (algo + raw sig). The raw 64-byte
// ed25519 signature is what crosses the agent proxy (as part of the agent
// reply), so it — not the whole blob alone — is the leak-check needle.
// A non-empty 64-byte signature is required; anything else fails.
func extractSSHSigInner(blob []byte) ([]byte, string) {
	if !bytes.HasPrefix(blob, []byte("SSHSIG")) {
		return nil, "blob does not start with SSHSIG"
	}
	off := len("SSHSIG") + 4 // magic + uint32 version
	readString := func() ([]byte, bool) {
		if off+4 > len(blob) {
			return nil, false
		}
		n := uint32(blob[off])<<24 | uint32(blob[off+1])<<16 | uint32(blob[off+2])<<8 | uint32(blob[off+3])
		off += 4
		if off+int(n) > len(blob) {
			return nil, false
		}
		s := blob[off : off+int(n)]
		off += int(n)
		return s, true
	}
	// Field 5 of 5 is the signature string (pubkey, namespace, reserved,
	// hash-algorithm, signature).
	var sig []byte
	for i := 0; i < 5; i++ {
		s, ok := readString()
		if !ok {
			return nil, fmt.Sprintf("SSHSIG blob truncated at string field %d", i+1)
		}
		if i == 4 {
			sig = s
		}
	}
	if off != len(blob) {
		return nil, "SSHSIG blob carries unexpected trailing bytes"
	}
	// sig is the 83-byte signature string field:
	// uint32 len=11 | "ssh-ed25519" | uint32 len=64 | 64 raw bytes —
	// an SSH string whose content is itself (algo string + sig string).
	if len(sig) == 64 {
		return sig, ""
	}
	if len(sig) > 8 {
		an := uint32(sig[0])<<24 | uint32(sig[1])<<16 | uint32(sig[2])<<8 | uint32(sig[3])
		if int(an)+4 < len(sig) {
			inner := sig[int(an)+4:]
			if len(inner) > 4 {
				sn := uint32(inner[0])<<24 | uint32(inner[1])<<16 | uint32(inner[2])<<8 | uint32(inner[3])
				if int(sn)+4 == len(inner) && len(inner[4:]) == 64 {
					return inner[4:], ""
				}
			}
		}
	}
	return nil, fmt.Sprintf("SSHSIG inner signature length = %d, want 64 (ed25519)", len(sig))
}

// signatureAppearsInJournal reports whether the journal text contains any
// needle derived from inner: the raw bytes, their hex form (lower and upper
// case), and base64 at all three alignments. It returns true on the first
// hit.
//
// A base64 occurrence of inner embedded in a longer buffer starts at one of
// three byte offsets relative to a 3-byte group. Only the groups lying wholly
// inside inner encode the same way whatever surrounds it, so for each offset
// the needle is the encoding of inner minus its leading partial group and
// minus its trailing partial group. Including the first or last partial
// group would make the needle depend on the neighbouring bytes and miss an
// embedded leak.
func signatureAppearsInJournal(journal string, inner []byte) bool {
	if len(inner) == 0 {
		return false
	}
	needleForms := []string{string(inner)}
	hexForm := hex.EncodeToString(inner)
	needleForms = append(needleForms, hexForm, strings.ToUpper(hexForm))
	for skip := 0; skip < 3; skip++ {
		if skip > len(inner) {
			break
		}
		sub := inner[skip:]
		sub = sub[:len(sub)-len(sub)%3]
		if len(sub) < 9 { // fewer than three whole groups: too weak to search for
			continue
		}
		needleForms = append(needleForms, base64.StdEncoding.EncodeToString(sub))
	}
	for _, n := range needleForms {
		if n != "" && strings.Contains(journal, n) {
			return true
		}
	}
	return false
}

// payloadNeedle is one thing the signing journal must not carry: data is
// searched in raw, hex and base64 form by signatureAppearsInJournal.
// argvBlind exempts exec/request data.argv from the search, for needles the
// test itself passes through an argv (none today; hisn logs every exec's argv
// by design).
type payloadNeedle struct {
	name      string
	data      []byte
	argvBlind bool
}

const minPayloadNeedleLen = 16

// validatePayloadNeedles returns "" when every needle is usable: a list that
// is empty, a needle with no name, a duplicate name or data shorter than
// minPayloadNeedleLen could pass vacuously.
func validatePayloadNeedles(needles []payloadNeedle) string {
	if len(needles) == 0 {
		return "no payload needles"
	}
	seen := map[string]bool{}
	for _, n := range needles {
		switch {
		case n.name == "":
			return "a payload needle has no name"
		case seen[n.name]:
			return fmt.Sprintf("duplicate payload needle %q", n.name)
		case len(n.data) < minPayloadNeedleLen:
			return fmt.Sprintf("payload needle %q is %d bytes, want at least %d", n.name, len(n.data), minPayloadNeedleLen)
		}
		seen[n.name] = true
	}
	return ""
}

// journalPayloadLeaks returns the sorted names of the needles found in the
// journal, one JSON record per line. A needle is searched over the whole raw
// line, except an argvBlind needle on an exec/request record whose data is an
// object with an argv key: that one is searched with data.argv removed. A
// line that does not parse as a JSON object is searched whole (fail closed).
func journalPayloadLeaks(journal string, needles []payloadNeedle) []string {
	found := map[string]bool{}
	for _, line := range strings.Split(journal, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		blindLine := line
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err == nil {
			if kind, _ := rec["kind"].(string); kind == "exec/request" {
				if data, ok := rec["data"].(map[string]any); ok {
					if _, hasArgv := data["argv"]; hasArgv {
						delete(data, "argv")
						if b, err := json.Marshal(rec); err == nil {
							blindLine = string(b)
						}
					}
				}
			}
		}
		for _, n := range needles {
			if found[n.name] {
				continue
			}
			scan := line
			if n.argvBlind {
				scan = blindLine
			}
			if signatureAppearsInJournal(scan, n.data) {
				found[n.name] = true
			}
		}
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// commitPayloadBytes returns the bytes git signs for a commit: the raw commit
// object (git cat-file commit) with its gpgsig header and that header's
// continuation lines removed. Only the header block is read, so a gpgsig line
// in the message is left alone; no gpgsig header, or two, is a failure.
func commitPayloadBytes(commitObj string) ([]byte, string) {
	lines := strings.Split(commitObj, "\n")
	var kept []string
	inSig := false
	seen := 0
	for i, line := range lines {
		if line == "" { // end of the header block: the rest is the message
			kept = append(kept, lines[i:]...)
			break
		}
		if strings.HasPrefix(line, "gpgsig ") {
			seen++
			inSig = true
			continue
		}
		if inSig && strings.HasPrefix(line, " ") {
			continue
		}
		inSig = false
		kept = append(kept, line)
	}
	switch seen {
	case 0:
		return nil, "commit carries no gpgsig header to remove from the payload"
	case 1:
		return []byte(strings.Join(kept, "\n")), ""
	}
	return nil, "commit carries duplicate gpgsig headers"
}

// payloadDigest hashes the signed payload the way ssh-keygen -Y sign does
// for the SSHSIG hash algorithm alg (sha256 or sha512).
func payloadDigest(alg string, payload []byte) ([]byte, string) {
	switch alg {
	case "sha256":
		d := sha256.Sum256(payload)
		return d[:], ""
	case "sha512":
		d := sha512.Sum512(payload)
		return d[:], ""
	}
	return nil, fmt.Sprintf("SSHSIG hash algorithm %q is not covered (want sha256 or sha512)", alg)
}

// sshsigHashAlg returns the hash-algorithm field of a decoded SSHSIG blob:
// "SSHSIG" | uint32 version | string pubkey | string namespace |
// string reserved | string hash-algorithm | string signature.
func sshsigHashAlg(blob []byte) (string, string) {
	if !bytes.HasPrefix(blob, []byte("SSHSIG")) {
		return "", "blob does not start with SSHSIG"
	}
	off := len("SSHSIG") + 4
	for i := 0; i < 4; i++ {
		if off+4 > len(blob) {
			return "", fmt.Sprintf("SSHSIG blob truncated at string field %d", i+1)
		}
		n := int(uint32(blob[off])<<24 | uint32(blob[off+1])<<16 | uint32(blob[off+2])<<8 | uint32(blob[off+3]))
		off += 4
		if n < 0 || off+n > len(blob) {
			return "", fmt.Sprintf("SSHSIG blob truncated at string field %d", i+1)
		}
		if i == 3 {
			return string(blob[off : off+n]), ""
		}
		off += n
	}
	return "", "unreachable"
}

// signingTestEnv is the environment for running ssh-keygen against a key
// FILE in the throwaway directory dir: HOME is dir and SSH_AUTH_SOCK is
// absent, so ssh-keygen can never reach the user's agent or keys.
func signingTestEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "SSH_AUTH_SOCK=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+dir)
}

// sshVerifyPayload checks that the SSHSIG blob is a valid signature over
// payload for namespace "git", with ssh-keygen -Y check-novalidate (the
// public key is inside the signature, so no key, agent or allowed-signers
// file is read). It returns "" on success. A wrong byte in payload fails it,
// which is what proves a digest computed from payload is the digest that was
// signed.
func sshVerifyPayload(blob, payload []byte) string {
	keygen, err := osexec.LookPath("ssh-keygen")
	if err != nil {
		return fmt.Sprintf("ssh-keygen not found: %v", err)
	}
	dir, err := os.MkdirTemp("", "wakil-sshsig-verify-")
	if err != nil {
		return fmt.Sprintf("temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	body := base64.StdEncoding.EncodeToString(blob)
	var armor strings.Builder
	armor.WriteString("-----BEGIN SSH SIGNATURE-----\n")
	for i := 0; i < len(body); i += 70 {
		end := i + 70
		if end > len(body) {
			end = len(body)
		}
		armor.WriteString(body[i:end] + "\n")
	}
	armor.WriteString("-----END SSH SIGNATURE-----\n")
	sigPath := filepath.Join(dir, "payload.sig")
	if err := os.WriteFile(sigPath, []byte(armor.String()), 0o600); err != nil {
		return fmt.Sprintf("writing the signature: %v", err)
	}
	cmd := osexec.Command(keygen, "-Y", "check-novalidate", "-n", "git", "-s", sigPath)
	cmd.Env = signingTestEnv(dir)
	cmd.Stdin = bytes.NewReader(payload)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Sprintf("ssh-keygen -Y check-novalidate rejected the signature over the payload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return ""
}

// TestJournalCommitExecID unit-tests the req_id correlation helper
// against recorded 0.6.7 journal line shapes (built as fixtures here
// with fake ids and a fake fingerprint — nothing real). It asserts the
// helper binds the right exec_id on the clean input and fails with the
// right step named on every corrupted variant.
func TestJournalCommitExecID(t *testing.T) {
	commitReq := `{"kind":"exec/request","client":{"uid":1000,"label":"wakil@rubin","label_source":"none","req_id":"fake-88202b85.0b3cf4-9"},"data":{"argv":["git","commit","-m","test signed commit"],"argv_bytes":29,"argv_count":4,"argv_truncated":false,"authz":{"allow":true},"cwd":"/tmp/ws-fake","op":"exec","seq":19,"stage":"request"}}`
	emptyReq := `{"kind":"exec/request","client":{"uid":1000,"label":"wakil@rubin","label_source":"none","req_id":"fake-88202b85.0b3cf4-8"},"data":{"path":"git","pid_peer":3185734,"result":null,"seq":19,"stage":"request"}}`
	commitExit := `{"kind":"exec/exit","client":{"uid":1000,"label":"wakil@rubin","label_source":"none","req_id":"fake-88202b85.0b3cf4-9"},"data":{"attach":false,"cancelled":false,"duration_ms":9,"exec_id":"fake-e-9","exit_code":0,"lost":false,"op":"exec","pid":30,"seq":19,"stage":"exit"}}`
	wrongExit := `{"kind":"exec/exit","client":{"uid":1000,"label":"wakil@rubin","label_source":"none","req_id":"fake-88202b85.0b3cf4-7"},"data":{"attach":false,"cancelled":false,"duration_ms":9,"exec_id":"fake-e-7","exit_code":0,"lost":false,"op":"exec","pid":31,"seq":19,"stage":"exit"}}`
	signRec := `{"kind":"ssh/sign","data":{"attributed":true,"exec_id":"fake-e-9","fingerprint":"SHA256:fakefingerprintfakefingerprintfakefp","result":"ok","seat":null}}`
	otherSign := `{"kind":"ssh/sign","data":{"attributed":true,"exec_id":"fake-e-7","fingerprint":"SHA256:fakefingerprintfakefingerprintfakefp","result":"ok","seat":null}}`

	parse := func(lines ...string) []map[string]any {
		var recs []map[string]any
		for _, line := range lines {
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("fixture is not valid JSON: %v\n%s", err, line)
			}
			recs = append(recs, rec)
		}
		return recs
	}

	// The clean input binds to the commit exec's id.
	if id, stepErr := journalCommitExecID(parse(emptyReq, commitReq, signRec, commitExit)); stepErr != "" || id != "fake-e-9" {
		t.Errorf("clean input: want (\"fake-e-9\", \"\"), got (%q, %q)", id, stepErr)
	}

	negatives := []struct {
		name       string
		step       string
		lines      []string
		wantPrefix string
	}{
		{"exit with a different req_id", "step 3", []string{commitReq, wrongExit, signRec}, "step 3: want exactly 1 exec/exit"},
		{"missing exit record", "step 3", []string{commitReq, signRec}, "step 3: want exactly 1 exec/exit"},
		{"duplicate exit for the same req_id", "step 3", []string{commitReq, commitExit, commitExit, signRec}, "step 3: want exactly 1 exec/exit"},
		{"two commit requests", "step 1", []string{commitReq, commitReq, commitExit, signRec}, "step 1: want exactly 1 exec/request"},
		{"no commit request", "step 1", []string{emptyReq, commitExit, signRec}, "step 1: want exactly 1 exec/request"},
		{"ssh/sign bound to another exec", "step 4", []string{commitReq, commitExit, otherSign}, "step 4: want exactly 1 ssh/sign"},
		{"two ssh/sign records for the commit exec", "step 4", []string{commitReq, commitExit, signRec, signRec}, "step 4: want exactly 1 ssh/sign"},
	}
	for _, n := range negatives {
		if id, stepErr := journalCommitExecID(parse(n.lines...)); stepErr == "" {
			t.Errorf("%s: want a step error, got a clean bind to %q", n.name, id)
		} else if !strings.HasPrefix(stepErr, n.wantPrefix) {
			t.Errorf("%s: want the error to name %q, got %q", n.name, n.step, stepErr)
		} else if id != "" {
			t.Errorf("%s: want an empty id on failure, got %q", n.name, id)
		}
	}

	// Field-level corruption: each variant mutates a field of the clean
	// records and the helper must fail at the step that reads it.
	mutate := func(lines []string, fn func(rec map[string]any) map[string]any) []string {
		var out []string
		for _, line := range lines {
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("fixture is not valid JSON: %v\n%s", err, line)
			}
			rec = fn(rec)
			b, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("re-marshal failed: %v", err)
			}
			out = append(out, string(b))
		}
		return out
	}
	clean := []string{commitReq, commitExit, signRec}
	set := func(rec map[string]any, path []string, val any) map[string]any {
		cur := rec
		for _, p := range path[:len(path)-1] {
			m, _ := cur[p].(map[string]any)
			if m == nil {
				m = map[string]any{}
			}
			cur[p] = m
			cur = m
		}
		cur[path[len(path)-1]] = val
		return rec
	}
	unset := func(rec map[string]any, path []string) map[string]any {
		if m, ok := rec[path[0]].(map[string]any); ok {
			delete(m, path[len(path)-1])
		}
		return rec
	}
	fieldNegatives := []struct {
		name       string
		lines      []string
		wantPrefix string
	}{
		{"missing req_id on the commit request", mutate(clean, func(r map[string]any) map[string]any {
			if c, ok := r["client"].(map[string]any); ok && r["kind"] == "exec/request" {
				delete(c, "req_id")
			}
			return r
		}), "step 2"},
		{"empty req_id on the commit request", mutate(clean, func(r map[string]any) map[string]any {
			if c, ok := r["client"].(map[string]any); ok && r["kind"] == "exec/request" {
				c["req_id"] = ""
			}
			return r
		}), "step 2"},
		{"non-string req_id on the commit request", mutate(clean, func(r map[string]any) map[string]any {
			if c, ok := r["client"].(map[string]any); ok && r["kind"] == "exec/request" {
				c["req_id"] = 9
			}
			return r
		}), "step 2"},
		{"seq mismatch", mutate(clean, func(r map[string]any) map[string]any {
			if r["kind"] == "exec/exit" {
				return set(r, []string{"data", "seq"}, 20)
			}
			return r
		}), "step 3"},
		{"missing seq on the exit", mutate(clean, func(r map[string]any) map[string]any {
			if r["kind"] == "exec/exit" {
				return unset(r, []string{"data", "seq"})
			}
			return r
		}), "step 3"},
		{"missing seq on the request", mutate(clean, func(r map[string]any) map[string]any {
			if r["kind"] == "exec/request" {
				return unset(r, []string{"data", "seq"})
			}
			return r
		}), "step 2"},
		{"exit_code non-zero", mutate(clean, func(r map[string]any) map[string]any {
			if r["kind"] == "exec/exit" {
				return set(r, []string{"data", "exit_code"}, 1)
			}
			return r
		}), "step 3"},
		{"exit_code missing", mutate(clean, func(r map[string]any) map[string]any {
			if r["kind"] == "exec/exit" {
				return unset(r, []string{"data", "exit_code"})
			}
			return r
		}), "step 3"},
		{"exit_code as a string", mutate(clean, func(r map[string]any) map[string]any {
			if r["kind"] == "exec/exit" {
				return set(r, []string{"data", "exit_code"}, "0")
			}
			return r
		}), "step 3"},
		{"missing exec_id on the exit", mutate(clean, func(r map[string]any) map[string]any {
			if r["kind"] == "exec/exit" {
				return unset(r, []string{"data", "exec_id"})
			}
			return r
		}), "step 3"},
		{"empty exec_id on the exit", mutate(clean, func(r map[string]any) map[string]any {
			if r["kind"] == "exec/exit" {
				return set(r, []string{"data", "exec_id"}, "")
			}
			return r
		}), "step 3"},
	}
	for _, n := range fieldNegatives {
		if id, stepErr := journalCommitExecID(parse(n.lines...)); stepErr == "" {
			t.Errorf("%s: want a step error, got a clean bind to %q", n.name, id)
		} else if !strings.HasPrefix(stepErr, n.wantPrefix) {
			t.Errorf("%s: want the error to name %q, got %q", n.name, n.wantPrefix, stepErr)
		}
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
	// hisn 0.4.0 keeps the directory of a stopped session (audited) until
	// `hisn ls --prune`: ended means not live and state stopped.
	if e, still := sessions["wakil-it-kill"]; still && (e.Live() || !e.Stopped()) {
		t.Fatalf("session survived Stop: %+v", e)
	}
	b.prune(ictx(t))
	if sessions, err = b.listSessions(ictx(t)); err != nil {
		t.Fatal(err)
	} else if _, still := sessions["wakil-it-kill"]; still {
		t.Fatal("hisn ls --prune left the stopped session behind")
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
	if e, still := sessions["wakil-it-orphan"]; still && (e.Live() || !e.Stopped()) {
		t.Fatalf("orphan session was not reaped: %+v", e)
	}
	b2.prune(ictx(t))
	if sessions, err = b2.listSessions(ictx(t)); err != nil {
		t.Fatal(err)
	} else if _, still := sessions["wakil-it-orphan"]; still {
		t.Fatal("hisn ls --prune left the stopped orphan behind")
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
	var fwait error
	go func() { fwait = foreign.Wait(); close(fdone) }()
	t.Cleanup(func() {
		_ = osexec.Command(b.opts.Binary, "stop", "--session", "wakil-it-foreign").Run()
		select {
		case <-fdone:
		case <-time.After(5 * time.Second):
			_ = foreign.Process.Kill()
			<-fdone
		}
		_ = osexec.Command(b.opts.Binary, "ls", "--prune").Run()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		m, err := b.listSessions(ictx(t))
		if err == nil && m["wakil-it-foreign"].Live() {
			break
		}
		select {
		case <-fdone:
			// hisn >= 0.4.1: the run process exits 0 once the detached
			// supervisor is up; only a failed start is a reason to skip.
			if fwait != nil {
				t.Skipf("cannot create a foreign session: supervisor exited: %v: %s", fwait, ferr.String())
			}
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
		_ = osexec.Command(b.opts.Binary, "stop", "--session", "wakil-it-parent").Run()
		_ = osexec.Command(b.opts.Binary, "kill", "--session", "wakil-it-parent", "--signal", "KILL").Run()
	})
	if VersionAtLeast(b.ver, [3]int{0, 4, 1}) {
		// hisn >= 0.4.1 detaches the supervisor (reparented, no Pdeathsig
		// from Wakil): it outlives the crashed Wakil by design. The dead
		// owner's record is what cleans it up: the next Reconcile stops it.
		if m, err := b.listSessions(ictx(t)); err != nil || !m["wakil-it-parent"].Live() {
			t.Fatalf("0.4.1: the detached session must survive its Wakil: %+v err %v", m["wakil-it-parent"], err)
		}
		b2, err := New(b.opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := b2.Reconcile(ictx(t)); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		m, err := b.listSessions(ictx(t))
		if err != nil {
			t.Fatal(err)
		}
		if e, ok := m["wakil-it-parent"]; ok && !e.Ended() {
			t.Fatalf("Reconcile left the dead owner's session %s: %+v", e.State, e)
		}
		return
	}
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
	// hisn 0.4.0 reports a SIGKILLed supervisor's directory as "crashed"
	// (0.3.x: "stale"); either way it is dead and replaceable.
	if st != "crashed" && st != "stale" {
		t.Skipf("hisn left no dead directory after SIGKILL (state %q); nothing to replace", st)
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
		if !(lsEntry{State: state}).Live() {
			t.Fatalf("session state = %q after cancel, want live (v1) or running (v2)", state)
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

// Stop (hisn stop, directory kept as "stopped") followed by a Start of the SAME
// chat id and name works against the real hisn: the stopped directory is
// replaced, the name is free, and the session is addressable by id and by name.
func TestIntegrationRestartAfterStop(t *testing.T) {
	b, cwd := realBackend(t)
	const id = "wakil-it-restart"
	istart(t, b, id, cwd)
	name := b.sessionName(id)
	if m, err := b.listSessions(ictx(t)); err != nil || m[id].Name != name || !m[id].Live() {
		t.Fatalf("listing after start: %+v err %v (want name %q live)", m[id], err, name)
	}
	if err := b.Stop(ictx(t)); err != nil {
		t.Fatal(err)
	}
	if m, _ := b.listSessions(ictx(t)); !m[id].Stopped() {
		t.Fatalf("state after Stop = %+v, want stopped", m[id])
	}
	istart(t, b, id, cwd)
	if out, info := irun(t, b, "", "sh", "-c", "echo again"); info.Code != 0 || strings.TrimSpace(out) != "again" {
		t.Fatalf("restarted session unusable: %q %+v", out, info)
	}
	// By name too, through the hisn CLI (what attach and a second client use).
	if out, err := b.output(ictx(t), false, "ls", "--session", name); err != nil {
		t.Fatalf("ls --session %s: %v: %s", name, err, out)
	}
}

// The real shutdown path ends the session: Start, one exec, Close, and `hisn ls
// --json` must no longer show it running (stopped is fine). Against hisn >=
// 0.4.1 the run process exited long before Close, so this proves stopLocked
// ends the detached supervisor itself, not a process that was already gone.
func TestIntegrationCloseStopsSession(t *testing.T) {
	b, cwd := realBackend(t)
	const id = "wakil-it-close"
	istart(t, b, id, cwd)
	sup := b.supOf()
	if out, info := irun(t, b, "", "sh", "-c", "echo up"); info.Code != 0 || strings.TrimSpace(out) != "up" {
		t.Fatalf("exec: %q %+v", out, info)
	}
	supPid := sup.pid()
	if err := b.Close(ictx(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}
	m, err := b.listSessions(ictx(t))
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := m[id]; ok && (e.Live() || e.transitional()) {
		t.Fatalf("session still %s after Close: %+v", e.State, e)
	}
	if sup.detachExited && procAlive(supPid, sup.supStart) {
		t.Fatalf("detached supervisor pid %d still alive after Close", supPid)
	}
}

// TestIntegrationOpenNetwork: in an `open` session (hisn 0.6.0):
//
//   - a public https request succeeds (HTTP 2xx or 3xx); FAILS otherwise;
//     skip only when WAKIL_TEST_OFFLINE=1 is set.
//   - a private address (10.0.0.1, RFC1918) is refused with EACCES specifically;
//     a timeout or any other error is a FAIL (it would mean `open` is not blocking).
//   - loopback TCP works: a listener on 127.0.0.1 accepts a connection.
//   - the kvr staging service works: one Set and one Get through it (when
//     kvr-server is visible inside the sandbox); SKIP otherwise.
//   - the browser tools work: Navigate to about:blank (when chromium is visible
//     inside the sandbox); SKIP otherwise — never PASS without opening a page.
//
// `open` narrows outbound to public TCP only: unix and loopback work;
// private/LAN addresses, UDP and other families get EACCES. The kvr
// service and the browser CDP tunnel use unix sockets (the kvr socket under
// .wakil/, the CDP tunnel over a unix-socket pair), so they are not blocked
// by `open`.
//
// Skips when hisn < 0.6.7 or `open` is refused (kernel older than 6.7 /
// Landlock ABI v4, Yama ptrace_scope too high, or missing DNS upstream).
func TestIntegrationOpenNetwork(t *testing.T) {
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
	} else if v, err := ParseVersion(string(out)); err != nil || !VersionAtLeast(v, [3]int{0, 6, 7}) {
		t.Skipf("hisn >= 0.6.7 required for network = open (got %q)", strings.TrimSpace(string(out)))
	}

	// Detect a visible kvr-server for the KVR sub-test.
	var kvrBin string
	if p, err := osexec.LookPath("kvr-server"); err == nil {
		if real, _ := filepath.EvalSymlinks(p); real != "" {
			if _, ok := binaryVisible(real, nil); ok {
				kvrBin = real
			}
		}
	}

	// Use Open (not New) so planKVR runs and the kvr service is planned.
	mut := func(o *Options) {
		o.Network = "open"
		if kvrBin != "" {
			o.KVR = kvrOpts(kvrBin)
		}
	}
	b, cwd, err := realProfileBackend(t, mut)
	if err != nil {
		// Fail closed: if hisn refuses `open`, skip (the host may not
		// support it). Wakil never falls back to a wider mode.
		if strings.Contains(err.Error(), "network") || strings.Contains(err.Error(), "open") {
			t.Skipf("hisn refused open network mode: %v", err)
		}
		t.Fatalf("Open: %v", err)
	}
	const id = "wakil-it-net-open"
	istart(t, b, id, cwd)

	// ---- 1. Public https must succeed ----
	if _, err := osexec.LookPath("curl"); err != nil {
		t.Skip("curl not found on PATH — needed for the public https test")
	}
	out, info := irun(t, b, "", "sh", "-c",
		"curl -s -o /dev/null -w '%{http_code}' --max-time 10 https://example.com 2>&1")
	code := strings.TrimSpace(out)
	if info.Code != 0 || code == "" || code == "000" {
		if os.Getenv("WAKIL_TEST_OFFLINE") == "1" {
			t.Skipf("WAKIL_TEST_OFFLINE=1: public https failed: code=%s out=%q", code, out)
		}
		t.Fatalf("public https to example.com failed: code=%s out=%q (set WAKIL_TEST_OFFLINE=1 to skip when the host has no external network)", code, out)
	}
	// HTTP 2xx or 3xx; anything else (4xx/5xx) means the request reached
	// something but did not succeed.
	n, _ := strconv.Atoi(code)
	if n < 200 || n >= 400 {
		if os.Getenv("WAKIL_TEST_OFFLINE") == "1" {
			t.Skipf("WAKIL_TEST_OFFLINE=1: public https returned HTTP %s", code)
		}
		t.Fatalf("public https to example.com returned HTTP %s — want 2xx or 3xx", code)
	}
	t.Logf("public https: HTTP %s", code)

	// ---- 2. Private address must be refused with EACCES specifically ----
	privateScript := `import socket, sys, errno
try:
    s = socket.create_connection(("10.0.0.1", 1), timeout=5)
    s.close()
    print("private-connected:UNEXPECTED")
except PermissionError as e:
    # EACCES is errno 13; hisn ` + "`open`" + ` returns it for private addresses.
    print("private-eacces:" + str(e))
    print("errno=" + str(e.errno))
except (socket.timeout, TimeoutError) as e:
    print("private-timeout:" + str(e))
except OSError as e:
    print("private-oserror:errno=" + str(e.errno) + ":" + str(e))
`
	out, info = irun(t, b, "", "python3", "-c", privateScript)
	t.Logf("private 10.0.0.1: %s (code=%d)", strings.TrimSpace(out), info.Code)
	if strings.Contains(out, "private-connected") {
		t.Fatalf("private address 10.0.0.1 was connected to — `open` must refuse it: %q", out)
	}
	if strings.Contains(out, "private-eacces") {
		// EACCES (errno 13) is exactly what `open` returns for private addresses.
		t.Logf("private address refused with EACCES (correct)")
	} else if strings.Contains(out, "private-timeout") {
		t.Fatalf("private address 10.0.0.1 timed out — `open` must return EACCES, not a timeout: %q", out)
	} else if strings.Contains(out, "private-oserror") {
		t.Fatalf("private address 10.0.0.1 failed with an OSError that is not EACCES — `open` must return EACCES: %q", out)
	} else {
		t.Fatalf("private address 10.0.0.1: unexpected output %q (code=%d)", out, info.Code)
	}

	// ---- 3. Loopback TCP works: a listener on 127.0.0.1 accepts a connection ----
	loopbackScript := `import socket, sys
# Listen on 127.0.0.1 and accept a connection from the same process.
srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", 0))
srv.listen(1)
port = srv.getsockname()[1]
import threading
def serve():
    c, _ = srv.accept()
    c.sendall(b"loopback-ok")
    c.close()
    srv.close()
t = threading.Thread(target=serve)
t.start()
cli = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
cli.settimeout(3)
cli.connect(("127.0.0.1", port))
data = cli.recv(16)
cli.close()
t.join()
print(data.decode("utf-8", "replace"))
`
	out, info = irun(t, b, "", "python3", "-c", loopbackScript)
	if !strings.Contains(out, "loopback-ok") {
		t.Fatalf("loopback TCP listener failed: %q (code=%d) — `open` must allow loopback", out, info.Code)
	}
	t.Logf("loopback: %s", strings.TrimSpace(out))

	// ---- 4. KVR staging service: one Set + one Get ----
	if kvrBin == "" {
		t.Logf("kvr: SKIP (no kvr-server visible inside the sandbox)")
	} else {
		sock := b.KVRSocket()
		if sock == "" {
			t.Fatalf("kvr: staging must be on: %s", b.SandboxStatus(ictx(t)).KVR)
		}
		c := staging.NewClient(sock)
		c.SetPathGuard(func() error { return GuardKVRSocket(cwd, sock) })
		defer c.Close()
		ctx := ictx(t)
		if err := c.Set(ctx, "it/net-open/k", "v"); err != nil {
			t.Fatalf("kvr Set: %v", err)
		}
		got, found, err := c.Get(ctx, "it/net-open/k")
		if err != nil || !found || got != "v" {
			t.Fatalf("kvr Get: got=%q found=%v err=%v (want v)", got, found, err)
		}
		t.Logf("kvr: Set+Get OK (value=%q)", got)
	}

	// ---- 5. Browser tools: Navigate to about:blank ----
	// The browser needs chromium and python3 inside the sandbox session.
	// We wrap the Backend in a BackendExecutor so the browser manager can
	// use the CLI transport (hisn exec --session). The session is already
	// running; BindChat + EnsureSession reuses it (Start returns nil when
	// the session id is already live).
	be := exec.NewBackendExecutor(b, exec.BackendOptions{Label: "hisn"})
	bctx := ictx(t)
	if err := be.BindChat(bctx, id); err != nil {
		t.Fatalf("BindChat: %v", err)
	}
	if err := be.EnsureSession(bctx); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	// Probe for chromium and python3 inside the session; SKIP (never PASS)
	// when either is missing.
	probeOut, _ := be.RunShell(bctx, "command -v chromium || command -v chromium-browser || command -v google-chrome")
	if strings.TrimSpace(probeOut) == "" {
		t.Logf("browser: SKIP (no chromium binary visible inside the sandbox session)")
	} else if _, err := be.RunShell(bctx, "command -v python3"); err != nil {
		t.Logf("browser: SKIP (python3 not found in the sandbox session — the browser tunnel needs it)")
	} else {
		bm, err := browser.NewManager(be, strings.TrimSpace(probeOut), browser.WithIdleTimeout(60*time.Second))
		if err != nil {
			t.Fatalf("browser NewManager: %v", err)
		}
		defer bm.Close()
		// Bounded: the launch waits for CDP readiness and retries the
		// handshake inside launchTimeout (30 s); a dead browser fails with
		// that bound named instead of hanging. The outer 60 s is the test's
		// own guard so a hang is a test failure, not a stuck CI job.
		nctx, ncancel := context.WithTimeout(bctx, 60*time.Second)
		defer ncancel()
		title, finalURL, err := bm.Navigate(nctx, "about:blank")
		if err != nil {
			t.Fatalf("browser Navigate to about:blank failed within the 60s test bound (launch bound 30s): %v", err)
		}
		t.Logf("browser: Navigate OK (title=%q url=%q)", title, finalURL)
	}
}

// The phase timeline of one real start/exec/stop, for the lifecycle numbers
// ("hisn session ready", "hisn stop"). Opt-in: WAKIL_TRACE=1. It prints the
// hisn spans and leaves the timeline file in the test's temp dir.
func TestIntegrationTraceTimeline(t *testing.T) {
	if !timeline.EnvEnabled() {
		t.Skip("set WAKIL_TRACE=1 to record the hisn timeline")
	}
	b, cwd := realBackend(t)
	timeline.Reset()
	path := timeline.Start(filepath.Join(t.TempDir(), "timeline.json"), "start", time.Now())
	t.Cleanup(timeline.Reset)
	istart(t, b, "wakil-it-trace", cwd)
	irun(t, b, "", "true")
	timeline.Enter("exit")
	if err := b.Close(ictx(t)); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := timeline.Flush(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc timeline.Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, tr := range doc.Transitions {
		for _, s := range tr.Spans {
			if strings.HasPrefix(s.Name, "hisn ") || strings.HasPrefix(s.Name, "supervisor ") {
				t.Logf("TRACE %-6s %-45s %8.2f ms", tr.Name, s.Name, s.DurMS)
			}
		}
	}
}
