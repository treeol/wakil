package hisn

import (
	"testing"
)

// TestSessionArgsMatchesOldBuilders proves the single sessionArgs builder
// produces the same argv the old builders produced for the same inputs.
// The old builders (supervisorArgs, profileArgs, namedArgs) were folded into
// sessionArgs in commit a263684; this test is the proof that the merge did
// not change the argv.
//
// The bare-policy builder (supervisorArgs) was removed with the version gates
// (hisn < 0.3.1 is no longer supported): it used --policy with a hisn-policy/v1
// file and carried --network host on the CLI. The profile and named builders
// both used --profile with a hisn-profile/v1 file and carry no --network (the
// network comes from the profile's [policy] table). sessionArgs covers both
// remaining paths.
func TestSessionArgsMatchesOldBuilders(t *testing.T) {
	const (
		id      = "chat-abc"
		name    = "wakil-key-chat-abc"
		profile = "/state/hisn/chat-abc.profile.toml"
		cwd     = "/home/user/project"
	)

	// --- old profileArgs (hisn >= 0.3.1, generated profile) ---
	// The old profileArgs was:
	//   []string{"run", "--detach", "-v", "--session-id", id,
	//     "--profile", profile, "--cwd", cwd, "--ready-fd", "3", "--name", name}
	oldProfileArgs := []string{
		"run", "--detach", "-v", "--session-id", id,
		"--profile", profile, "--cwd", cwd, "--ready-fd", "3", "--name", name,
	}

	// sessionArgs with no grants = profileArgs.
	got := sessionArgs(id, name, profile, cwd, nil, nil)
	if !sliceEq(got, oldProfileArgs) {
		t.Fatalf("profileArgs mismatch:\n  old: %v\n  new: %v", oldProfileArgs, got)
	}

	// --- old namedArgs (named profile + grants) ---
	// The old namedArgs was profileArgs + --grant p:ro / --grant p:rw.
	ro := []string{"/opt/data", "/opt/ref"}
	rw := []string{"/srv/out"}
	oldNamedArgs := append(append([]string{}, oldProfileArgs...),
		"--grant", "/opt/data:ro", "--grant", "/opt/ref:ro",
		"--grant", "/srv/out:rw",
	)

	got = sessionArgs(id, name, profile, cwd, ro, rw)
	if !sliceEq(got, oldNamedArgs) {
		t.Fatalf("namedArgs mismatch:\n  old: %v\n  new: %v", oldNamedArgs, got)
	}

	// --- old supervisorArgs (bare policy, hisn < 0.3.1) ---
	// The old supervisorArgs was:
	//   []string{"run", "--detach", "-v", "--session-id", id,
	//     "--policy", policy, "--cwd", cwd, "--network", "host",
	//     "--ready-fd", "3", "--name", name}
	// This path used --policy (not --profile) and --network host on the CLI.
	// It was removed with the version gates: hisn < 0.3.1 is no longer
	// supported, so sessionArgs does not reproduce it. The generated profile
	// path (sessionArgs) carries the network in the profile's [policy] table,
	// not on the CLI, so the argv differs by design (--profile, no --network).
	// This is documented here so the removal is auditable.
	oldSupervisorArgs := []string{
		"run", "--detach", "-v", "--session-id", id,
		"--policy", profile, "--cwd", cwd, "--network", "host",
		"--ready-fd", "3", "--name", name,
	}
	// Confirm the old supervisorArgs is NOT what sessionArgs produces
	// (it used --policy + --network host; sessionArgs uses --profile).
	if sliceEq(got, oldSupervisorArgs) {
		t.Fatal("sessionArgs must NOT reproduce the old bare-policy supervisorArgs " +
			"(that path used --policy + --network host and was removed with the gates)")
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
