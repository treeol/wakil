package hisn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
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
	commitOut, commitInfo := irunSign(t, b, cwd, "git", "commit", "-m", "test signed commit")
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
	// Extract the commit's gpgsig (host git) and require both its base64
	// armor body (as one joined line — the journal's JSON line wraps
	// don't re-wrap it) and its decoded-bytes hex form to be absent.
	sigOut, err := func() ([]byte, error) {
		cmd := osexec.Command("git", "-C", cwd, "log", "-1", "--format=%GG")
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		return cmd.Output()
	}()
	if err != nil {
		t.Fatalf("git log %%GG on host failed: %v", err)
	}
	gpgsig := strings.TrimSpace(string(sigOut))
	if gpgsig == "" {
		t.Fatalf("git log %%GG empty: the commit carries no signature to check the journal against")
	}
	// The base64 armor body: strip PGP-armor header/footer lines ("-----",
	// "Version:", ...) and fold the line-wrapped base64 back into one
	// string. Concatenated-wrap is the base64 the proxy would have had to
	// journal verbatim to leak it.
	var body strings.Builder
	for _, l := range strings.Split(gpgsig, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "-----") || strings.Contains(l, ": ") {
			continue
		}
		body.WriteString(l)
	}
	armorBody := body.String()
	if armorBody != "" && strings.Contains(jout, armorBody) {
		t.Errorf("the journal carries the commit's signature bytes (base64):\n%s", jout)
	}
	if decoded, err := base64.StdEncoding.DecodeString(armorBody); err == nil && len(decoded) > 0 {
		hexForm := hex.EncodeToString(decoded)
		if strings.Contains(jout, hexForm) {
			t.Errorf("the journal carries the commit's signature bytes (hex):\n%s", jout)
		}
	}
	// Belt-and-braces: the commit message itself must not appear in the
	// journal either (the closed data key set is the real leak guard;
	// this catches a future record that echoes the payload elsewhere).
	if strings.Contains(jout, "test signed commit") {
		t.Errorf("the journal carries the commit payload:\n%s", jout)
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
