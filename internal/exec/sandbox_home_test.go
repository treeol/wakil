package exec

// H3 tests: sandbox-home persistence hole mitigation. Verifies that the
// sandbox-home docker args include tmpfs overlays on go/bin and .cargo/bin
// so installed binaries do not persist across sessions.

import (
	"strings"
	"testing"
)

// TestSandboxHomeArgs_TmpfsOverlays verifies that the two executable bin dirs
// get tmpfs overlays (H3: persistence hole fix).
func TestSandboxHomeArgs_TmpfsOverlays(t *testing.T) {
	args := sandboxHomeArgs("/home/test/.wakil/sandbox-home", 1000, 1000)
	joined := strings.Join(args, " ")

	// The tmpfs overlays must shadow the persistent bind mount at the
	// two PATH-first executable directories.
	required := []string{
		"--tmpfs /home/user/go/bin:rw,exec,nosuid,nodev,size=64m",
		"--tmpfs /home/user/.cargo/bin:rw,exec,nosuid,nodev,size=64m",
	}
	for _, flag := range required {
		if !strings.Contains(joined, flag) {
			t.Errorf("missing H3 tmpfs overlay: %q\ngot: %s", flag, joined)
		}
	}
}

// TestSandboxHomeArgs_BindMountPresent verifies the persistent bind mount
// still exists (caches need it).
func TestSandboxHomeArgs_BindMountPresent(t *testing.T) {
	home := "/home/test/.wakil/sandbox-home"
	args := sandboxHomeArgs(home, 1000, 1000)
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "-v "+home+":/home/user:z") {
		t.Errorf("bind mount for sandbox-home missing\ngot: %s", joined)
	}
}

// TestSandboxHomeArgs_PathIncludesBinDirs verifies PATH still puts the
// bin dirs first (so installed tools work within the session).
func TestSandboxHomeArgs_PathIncludesBinDirs(t *testing.T) {
	args := sandboxHomeArgs("/home/test/.wakil/sandbox-home", 1000, 1000)
	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "PATH=/home/user/go/bin:/home/user/.cargo/bin:") {
		t.Errorf("PATH should start with go/bin and .cargo/bin\ngot: %s", joined)
	}
}

// TestSandboxHomeArgs_NoPersistentBinDir verifies that the tmpfs flags
// appear AFTER the bind mount in the args list, so Docker applies them
// as overlays (not the reverse).
func TestSandboxHomeArgs_TmpfsAfterBindMount(t *testing.T) {
	args := sandboxHomeArgs("/home/test/.wakil/sandbox-home", 1000, 1000)

	bindIdx := -1
	goBinTmpfsIdx := -1
	for i, a := range args {
		// The bind mount is "-v <path>:/home/user:z" — two consecutive args.
		if a == "-v" && i+1 < len(args) && strings.Contains(args[i+1], "/home/user:z") {
			bindIdx = i
		}
		if a == "--tmpfs" && i+1 < len(args) && args[i+1] == "/home/user/go/bin:rw,exec,nosuid,nodev,size=64m" {
			goBinTmpfsIdx = i
		}
	}
	if bindIdx < 0 {
		t.Fatal("bind mount not found in args")
	}
	if goBinTmpfsIdx < 0 {
		t.Fatal("go/bin tmpfs not found in args")
	}
	if goBinTmpfsIdx < bindIdx {
		t.Errorf("tmpfs overlay (idx %d) should come after bind mount (idx %d)", goBinTmpfsIdx, bindIdx)
	}
}
