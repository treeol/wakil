package sandbox

import (
	"os"
	"strings"
)

// ContainerUsernsHint is the one-line explanation shown when hisn cannot use
// user namespaces and Wakil itself runs inside a container.
const ContainerUsernsHint = "running in a container: Docker's default seccomp profile blocks user namespaces; " +
	"use `--security-opt seccomp=unconfined` (plus `label=disable` on SELinux hosts), run on the host, or use `--sandbox direct`"

// ContainerProbe is the host surface InContainerWith reads. Tests fill it with
// fake files; InContainer uses the real one.
type ContainerProbe struct {
	// Exists reports whether a path exists.
	Exists func(path string) bool
	// ReadFile returns a file's content ("" and an error when unreadable).
	ReadFile func(path string) ([]byte, error)
	// Getenv reads an environment variable.
	Getenv func(key string) string
}

// InContainer reports whether this process appears to run inside a container
// (Docker, Podman, Kubernetes, LXC). It is a heuristic for choosing a hint,
// never a security decision.
func InContainer() bool {
	return InContainerWith(ContainerProbe{
		Exists:   func(p string) bool { _, err := os.Stat(p); return err == nil },
		ReadFile: os.ReadFile,
		Getenv:   os.Getenv,
	})
}

// InContainerWith is InContainer over an injectable host surface.
func InContainerWith(p ContainerProbe) bool {
	if p.Exists != nil && (p.Exists("/.dockerenv") || p.Exists("/run/.containerenv")) {
		return true
	}
	if p.Getenv != nil && p.Getenv("container") != "" {
		return true
	}
	if p.ReadFile != nil {
		if b, err := p.ReadFile("/proc/1/cgroup"); err == nil {
			s := strings.ToLower(string(b))
			for _, m := range []string{"docker", "kubepods", "containerd", "libpod", "lxc"} {
				if strings.Contains(s, m) {
					return true
				}
			}
		}
	}
	return false
}

// IsUsernsFailure reports whether a hisn probe failure reason is about user
// namespaces (the failure Docker's default seccomp profile causes).
func IsUsernsFailure(reason string) bool {
	r := strings.ToLower(reason)
	return strings.Contains(r, "namespace") || strings.Contains(r, "userns") || strings.Contains(r, "unshare") || strings.Contains(r, "newuser")
}

// ContainerHintFor returns ContainerUsernsHint when the hisn failure reason is
// about user namespaces and the process runs in a container, else "".
func ContainerHintFor(reason string) string {
	if reason != "" && IsUsernsFailure(reason) && InContainer() {
		return ContainerUsernsHint
	}
	return ""
}
