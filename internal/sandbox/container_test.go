package sandbox

import (
	"errors"
	"testing"
)

func fakeProbe(files map[string]string, env map[string]string) ContainerProbe {
	return ContainerProbe{
		Exists: func(p string) bool { _, ok := files[p]; return ok },
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("not found")
		},
		Getenv: func(k string) string { return env[k] },
	}
}

func TestInContainerWith(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		env   map[string]string
		want  bool
	}{
		{"bare host", map[string]string{"/proc/1/cgroup": "0::/init.scope\n"}, nil, false},
		{"dockerenv", map[string]string{"/.dockerenv": ""}, nil, true},
		{"podman containerenv", map[string]string{"/run/.containerenv": ""}, nil, true},
		{"container env var", nil, map[string]string{"container": "podman"}, true},
		{"cgroup v1 docker", map[string]string{"/proc/1/cgroup": "12:cpu:/docker/abc\n"}, nil, true},
		{"cgroup kubepods", map[string]string{"/proc/1/cgroup": "0::/kubepods/burstable/pod1\n"}, nil, true},
		{"nothing readable", nil, nil, false},
	}
	for _, c := range cases {
		if got := InContainerWith(fakeProbe(c.files, c.env)); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestIsUsernsFailure(t *testing.T) {
	for r, want := range map[string]bool{
		"hisn check failed: [FAIL] user namespaces":           true,
		"hisn check failed: unshare: Operation not permitted": true,
		"hisn not found in PATH":                              false,
		"hisn 0.3.0 is older than 0.4.2":                      false,
		"":                                                    false,
	} {
		if IsUsernsFailure(r) != want {
			t.Errorf("%q: want %v", r, want)
		}
	}
}
