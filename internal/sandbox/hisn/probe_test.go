package hisn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func probeWith(t *testing.T, fe *fakeEnv, ver string) (string, string) {
	t.Helper()
	if ver != "" {
		setVersion(t, fe, ver)
	}
	return Probe(ctxT(t), fe.bin, "0.3.1")
}

func TestProbe(t *testing.T) {
	t.Run("usable", func(t *testing.T) {
		fe := newFakeEnv(t)
		if v, why := probeWith(t, fe, "0.3.1"); v != "0.3.1" || why != "" {
			t.Errorf("v=%q why=%q", v, why)
		}
	})
	t.Run("newer", func(t *testing.T) {
		fe := newFakeEnv(t)
		if v, why := probeWith(t, fe, "0.10.2"); v != "0.10.2" || why != "" {
			t.Errorf("v=%q why=%q", v, why)
		}
	})
	t.Run("too old", func(t *testing.T) {
		fe := newFakeEnv(t)
		v, why := probeWith(t, fe, "0.2.2")
		if v != "0.2.2" || !strings.Contains(why, "0.2.2 is older than 0.3.1") {
			t.Errorf("v=%q why=%q", v, why)
		}
	})
	t.Run("check fails", func(t *testing.T) {
		fe := newFakeEnv(t)
		_ = os.WriteFile(filepath.Join(fe.state, "check_fail"), nil, 0o600)
		_, why := probeWith(t, fe, "0.3.1")
		if !strings.Contains(why, "hisn check failed") || !strings.Contains(why, "user namespaces") {
			t.Errorf("why=%q", why)
		}
	})
	t.Run("not found", func(t *testing.T) {
		_, why := Probe(ctxT(t), filepath.Join(t.TempDir(), "nope"), "0.3.1")
		if why == "" {
			t.Error("a missing binary is a reason")
		}
		t.Setenv("PATH", t.TempDir())
		if _, why := Probe(ctxT(t), "", "0.3.1"); !strings.Contains(why, "not found in PATH") {
			t.Errorf("why=%q", why)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		fe := newFakeEnv(t)
		_ = os.WriteFile(filepath.Join(fe.state, "version"), []byte("banana"), 0o600)
		if _, why := Probe(ctxT(t), fe.bin, "0.3.1"); !strings.Contains(why, "cannot parse") {
			t.Errorf("why=%q", why)
		}
	})
	t.Run("read-only", func(t *testing.T) {
		fe := newFakeEnv(t)
		probeWith(t, fe, "0.3.1")
		c := calls(t, fe)
		for _, bad := range []string{"run", "kill", "grant", "ls"} {
			for _, l := range strings.Split(c, "\n") {
				if strings.HasPrefix(l, bad+" ") || l == bad {
					t.Errorf("a probe must only run --version and check, saw %q", l)
				}
			}
		}
	})
}
