package hisn

import (
	"errors"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
)

// TestRootfsTable covers the [rootfs] fragment: exactly one key, never an empty
// table, name form for store names and path form for absolute paths.
func TestRootfsTable(t *testing.T) {
	pp := policyParams{Cwd: "/work", Home: "/state/h", Hostname: "wakil-x", ShmSize: 64, TmpSize: 1024}
	cases := []struct {
		name, rootfs string
		want         string // substring expected in the profile ("" = no [rootfs])
	}{
		{"unset emits nothing", "", ""},
		{"store name", "wakil-dev", "[rootfs]"},
		{"absolute path", "/srv/rootfs", "[rootfs]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := buildProfile(pp, Surface{Rootfs: c.rootfs}, "")
			if err != nil {
				t.Fatal(err)
			}
			s := string(out)
			if c.want == "" {
				if strings.Contains(s, "[rootfs]") {
					t.Fatalf("unset rootfs must not emit [rootfs]:\n%s", s)
				}
				return
			}
			if !strings.Contains(s, c.want) {
				t.Fatalf("want %q in profile:\n%s", c.want, s)
			}
			// Exactly one key, with the right value; never an empty key.
			key := "name = \"" + c.rootfs + "\""
			if strings.HasPrefix(c.rootfs, "/") {
				key = "path = \"" + c.rootfs + "\""
			}
			if !strings.Contains(s, key) {
				t.Fatalf("want %s under [rootfs]:\n%s", key, s)
			}
			if strings.Contains(s, "name = \"\"") || strings.Contains(s, "path = \"\"") {
				t.Fatalf("empty rootfs key emitted:\n%s", s)
			}
		})
	}
}

// TestValidateRootfsForms: store names pass; relative paths fail; the path form
// is refused in the project file (allowPath=false).
func TestValidateRootfsForms(t *testing.T) {
	ok := []struct {
		r         string
		allowPath bool
	}{{"", false}, {"wakil-dev", false}, {"wakil-dev", true}, {"/abs/dir", true}}
	for _, c := range ok {
		if err := ValidateRootfs(c.r, c.allowPath); err != nil {
			t.Errorf("ValidateRootfs(%q,%v) = %v, want nil", c.r, c.allowPath, err)
		}
	}
	bad := []struct {
		r         string
		allowPath bool
		want      string
	}{
		{"/abs/dir", false, "global only"},
		{"rel/path", true, "want a store name"},
		{"has space", true, "want a store name"},
	}
	for _, c := range bad {
		err := ValidateRootfs(c.r, c.allowPath)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateRootfs(%q,%v) = %v, want containing %q", c.r, c.allowPath, err, c.want)
		}
	}
}

// TestNormalizeRefusesRootfsWithNamedProfile: a named profile is used as is, so
// a rootfs (or a non-default home) must be refused, never silently dropped.
func TestNormalizeRefusesRootfsWithNamedProfile(t *testing.T) {
	_, err := Surface{Profile: "mine", Rootfs: "wakil-dev"}.Normalize(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "cannot be combined with hisn.profile") {
		t.Fatalf("named profile + rootfs must be refused, got %v", err)
	}
	_, err = Surface{Profile: "mine", Home: "tmpfs"}.Normalize(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("named profile + home=tmpfs must be refused, got %v", err)
	}
	if _, err := (Surface{Profile: "mine"}).Normalize(t.TempDir()); err != nil {
		t.Fatalf("named profile alone must stay valid: %v", err)
	}
}

// TestRootfsErrorFix: hisn's missing-rootfs failure carries the plan's fix line.
func TestRootfsErrorFix(t *testing.T) {
	base := errors.New("hisn profile validate failed: exit status 125: hisn: rootfs.path: /x/wakil-dev: No such file or directory (os error 2)")
	err := rootfsError(base, "wakil-dev")
	if err == nil || !strings.Contains(err.Error(), "hisn rootfs import wakil-dev --from-image wakil-dev") {
		t.Fatalf("missing store rootfs must name the fix, got %v", err)
	}
	if got := rootfsError(base, ""); !errors.Is(got, base) {
		t.Fatalf("no rootfs: error must pass through unchanged")
	}
	other := errors.New("some other failure")
	if got := rootfsError(other, "wakil-dev"); !errors.Is(got, other) {
		t.Fatalf("unrelated failure must pass through unchanged")
	}
}

// TestRealHisnAcceptsRootfs: with hisn >= 0.3.2 and the store rootfs present,
// the generated [rootfs] profile validates. Without the store rootfs (this
// sandbox: the rootfs store lives on the host) the test SKIPS with the reason.
func TestRealHisnAcceptsRootfs(t *testing.T) {
	bin := realHisnOffline(t)
	if v, err := ParseVersion(mustVersion(t, bin)); err != nil || !VersionAtLeast(v, [3]int{0, 3, 2}) {
		t.Skipf("[rootfs] needs hisn >= %s; this hisn is %q", ProfileHomeMinVersion, mustVersion(t, bin))
	}
	if err := osexec.Command(bin, "rootfs", "path", "wakil-dev").Run(); err != nil {
		t.Skip("rootfs wakil-dev is not in this host's hisn store; import it with `hisn rootfs import wakil-dev --from-image wakil-dev` to run this test")
	}
	d := realDirs(t, "ws", "home", "out")
	prof, err := buildProfile(policyParams{Cwd: d[0], Home: d[1], Hostname: "wakil-0123456789abcdef",
		GrantableRW: []string{d[2]}, EnvPass: []string{"CI"}, ShmSize: 64, TmpSize: 1024}, Surface{Rootfs: "wakil-dev"}, "")
	if err != nil {
		t.Fatal(err)
	}
	p := d[2] + "/rootfs.profile.toml"
	if err := os.WriteFile(p, prof, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := hisnRun(t, bin, "profile", "validate", p); err != nil {
		t.Fatalf("real hisn rejects the [rootfs] profile: %v\n%s", err, out)
	}
}
