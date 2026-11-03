package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// unconditional returns the syscall names allowed with no caps gate and no
// argument filter — what an unprivileged uid under --cap-drop=ALL may call.
func unconditional(t *testing.T, profile []byte) map[string]bool {
	t.Helper()
	var p struct {
		Syscalls []struct {
			Names    []string       `json:"names"`
			Action   string         `json:"action"`
			Args     []any          `json:"args"`
			Includes map[string]any `json:"includes"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal(profile, &p); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range p.Syscalls {
		if r.Action != "SCMP_ACT_ALLOW" || len(r.Args) > 0 {
			continue
		}
		if _, gated := r.Includes["caps"]; gated {
			continue
		}
		if _, arch := r.Includes["arches"]; arch {
			continue
		}
		for _, n := range r.Names {
			out[n] = true
		}
	}
	return out
}

func TestBuildSeccompProfile(t *testing.T) {
	if p, _ := buildSeccompProfile(false, false); p != nil {
		t.Error("no options → docker default (nil profile)")
	}
	io, _ := buildSeccompProfile(true, false)
	if !bytes.Equal(io, seccompIOUringProfile) {
		t.Error("io_uring only must be the embedded profile byte-for-byte")
	}

	us, err := buildSeccompProfile(false, true)
	if err != nil {
		t.Fatal(err)
	}
	allowed := unconditional(t, us)
	for _, n := range usernsSyscalls {
		if !allowed[n] {
			t.Errorf("allow_userns: %s not allowed for the unprivileged uid", n)
		}
	}
	for _, n := range ioUringSyscalls {
		if allowed[n] {
			t.Errorf("allow_userns alone must not enable %s", n)
		}
	}
	// Everything else stays as in the base profile: still denied/gated.
	for _, n := range []string{"bpf", "keyctl", "reboot", "init_module", "sethostname", "chroot", "kexec_load"} {
		if allowed[n] {
			t.Errorf("allow_userns must not open %s", n)
		}
	}
	// clone3 must not be forced to ENOSYS any more.
	if strings.Contains(string(us), `"errnoRet":38`) {
		t.Error("clone3→ENOSYS rule must be removed under allow_userns")
	}
	// Base unconditional set is preserved.
	base := unconditional(t, seccompIOUringProfile)
	for n := range base {
		if !allowed[n] && !slices.Contains(ioUringSyscalls, n) {
			t.Errorf("allow_userns dropped base syscall %s", n)
		}
	}

	both, _ := buildSeccompProfile(true, true)
	ab := unconditional(t, both)
	for _, n := range append(append([]string{}, usernsSyscalls...), ioUringSyscalls...) {
		if !ab[n] {
			t.Errorf("io_uring+userns: %s missing", n)
		}
	}
}

func TestDockerHardeningArgs_AllowUserns(t *testing.T) {
	on := strings.Join(dockerHardeningArgs(DockerOpts{DockerAllowUserns: true, IOUringProfilePath: "/tmp/p.json"}), " ")
	for _, want := range []string{"--security-opt=apparmor=unconfined", "--security-opt=systempaths=unconfined", "--security-opt=no-new-privileges", "--cap-drop=ALL", "--security-opt=seccomp=/tmp/p.json"} {
		if !strings.Contains(on, want) {
			t.Errorf("missing %s: %s", want, on)
		}
	}
	if strings.Contains(on, "seccomp=unconfined") {
		t.Errorf("seccomp must not be unconfined: %s", on)
	}
	off := strings.Join(dockerHardeningArgs(DockerOpts{}), " ")
	for _, bad := range []string{"apparmor=", "systempaths=", "seccomp="} {
		if strings.Contains(off, bad) {
			t.Errorf("flag off must not add %s: %s", bad, off)
		}
	}
}

// TestLiveDockerAllowUserns is the acceptance test against a real daemon:
// with the flag, docker inspect shows Wakil's variant seccomp profile plus
// apparmor/systempaths unconfined and no-new-privileges, and `unshare -Ur id`
// prints uid=0 (and `hisn check` passes when hisn is in the image); without
// it, unshare fails and none of those options are set.
func TestLiveDockerAllowUserns(t *testing.T) {
	requireDocker(t)
	// Ubuntu 24.04+ hosts can forbid unprivileged userns for unconfined
	// processes, which no container option can override.
	if b, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); err == nil && strings.TrimSpace(string(b)) == "1" {
		t.Skip("host sets kernel.apparmor_restrict_unprivileged_userns=1; user namespaces unavailable in containers")
	}
	for _, on := range []bool{true, false} {
		ex, err := NewDockerExecutor(DockerOpts{Image: "wakil-dev:latest", Workdir: "/work", HostMount: t.TempDir(), DockerAllowUserns: on})
		if err != nil {
			if strings.Contains(err.Error(), "image") || strings.Contains(err.Error(), "pull") {
				t.Skipf("wakil-dev image not available: %v", err)
			}
			t.Fatalf("allow_userns=%v: %v", on, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		insp, _ := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .HostConfig}}", ex.container).Output()
		var hc struct {
			SecurityOpt []string
			MaskedPaths []string
		}
		_ = json.Unmarshal(insp, &hc)
		opts := hc.SecurityOpt
		// systempaths=unconfined is not recorded in SecurityOpt: docker
		// stores it as an empty MaskedPaths list (/proc unmasked).
		unmasked := len(hc.MaskedPaths) == 0
		has := func(p string) bool {
			return slices.ContainsFunc(opts, func(o string) bool { return strings.HasPrefix(o, p) })
		}
		out, uerr := ex.RunShell(ctx, "unshare -Ur id -u")
		hisnOut, hisnErr := ex.RunShell(ctx, "command -v hisn >/dev/null && hisn check || echo NOHISN")
		cancel()
		_ = ex.Close()

		if !has("no-new-privileges") {
			t.Errorf("allow_userns=%v: no-new-privileges missing: %v", on, opts)
		}
		if on {
			if !has("apparmor=unconfined") || !unmasked || !has("seccomp={") || has("seccomp=unconfined") {
				t.Errorf("flag on: apparmor/seccomp/unmasked wrong (unmasked=%v), opts=%.200v", unmasked, opts)
			}
			if uerr != nil || strings.TrimSpace(out) != "0" {
				t.Errorf("flag on: unshare -Ur id -u = %q, %v (want 0)", out, uerr)
			}
			if !strings.Contains(hisnOut, "NOHISN") && hisnErr != nil {
				t.Errorf("flag on: hisn check failed: %s %v", hisnOut, hisnErr)
			}
		} else {
			if has("apparmor=") || unmasked || has("seccomp=") {
				t.Errorf("flag off: unexpected security opts %v (unmasked=%v)", opts, unmasked)
			}
			if uerr == nil && strings.TrimSpace(out) == "0" {
				t.Errorf("flag off: unshare must fail, got %q", out)
			}
			if !strings.Contains(hisnOut, "NOHISN") && hisnErr == nil {
				t.Errorf("flag off: hisn check must fail, got %s", hisnOut)
			}
		}
	}
}
