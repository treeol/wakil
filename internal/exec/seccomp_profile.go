package exec

import (
	"encoding/json"
	"fmt"
	"slices"
)

// usernsSyscalls are the namespace and mount syscalls that the Docker default
// profile only allows with CAP_SYS_ADMIN (or, for clone/clone3, only without
// CLONE_NEW* flags). Inside an unprivileged user namespace they act on
// namespaces the process owns, so allowing them for the unprivileged uid is
// what makes `unshare -Ur` and hisn work in the container. Everything else in
// the profile stays as is.
var usernsSyscalls = []string{
	"unshare", "clone", "clone3", "setns",
	"mount", "umount2", "pivot_root",
	"open_tree", "move_mount", "mount_setattr",
	"fsopen", "fsconfig", "fsmount", "fspick",
}

var ioUringSyscalls = []string{"io_uring_setup", "io_uring_enter", "io_uring_register"}

// seccompRule is one entry of a Docker seccomp profile. Unknown fields are
// preserved verbatim via the raw map so the profile round-trips unchanged.
type seccompRule = map[string]any

// buildSeccompProfile returns the seccomp profile JSON for the requested
// options, or nil when Docker's built-in default applies (neither option).
//
//   - iouring only: the embedded profile, byte-for-byte (unchanged behaviour).
//   - userns: the embedded profile with usernsSyscalls allowed
//     unconditionally (their CAP_SYS_ADMIN gating, the clone CLONE_NEW* mask
//     rules and the clone3→ENOSYS rule are removed for those names only);
//     the io_uring syscalls are removed again unless iouring is also set.
func buildSeccompProfile(iouring, userns bool) ([]byte, error) {
	if !iouring && !userns {
		return nil, nil
	}
	if !userns {
		return seccompIOUringProfile, nil
	}
	var p map[string]any
	if err := json.Unmarshal(seccompIOUringProfile, &p); err != nil {
		return nil, fmt.Errorf("seccomp profile: %w", err)
	}
	raw, _ := p["syscalls"].([]any)
	var out []any
	for _, r := range raw {
		rule, ok := r.(seccompRule)
		if !ok {
			out = append(out, r)
			continue
		}
		names := ruleNames(rule)
		var keep []string
		for _, n := range names {
			if slices.Contains(usernsSyscalls, n) {
				continue // re-added unconditionally below
			}
			if !iouring && slices.Contains(ioUringSyscalls, n) {
				continue // userns-only: no io_uring
			}
			keep = append(keep, n)
		}
		if len(keep) == 0 {
			continue
		}
		if len(keep) != len(names) {
			rule["names"] = keep
		}
		out = append(out, rule)
	}
	out = append(out, seccompRule{
		"names":   append([]string(nil), usernsSyscalls...),
		"action":  "SCMP_ACT_ALLOW",
		"comment": "wakil sandbox.docker.allow_userns: namespace/mount syscalls for unprivileged user namespaces",
	})
	p["syscalls"] = out
	return json.Marshal(p)
}

func ruleNames(rule seccompRule) []string {
	arr, _ := rule["names"].([]any)
	names := make([]string, 0, len(arr))
	for _, n := range arr {
		if s, ok := n.(string); ok {
			names = append(names, s)
		}
	}
	return names
}
