package hisn

import (
	"context"
	"strings"
	"testing"
)

// B2: a named profile gets no session gitconfig, so a Wakil-requested git
// identity or signing key must be refused, never dropped silently.
func TestNamedProfileRefusesGitIdentityAndSigning(t *testing.T) {
	cases := map[string]Options{
		"identity": {GitName: "A", GitEmail: "a@example.invalid"},
		"signing":  {SigningKey: "ssh-ed25519 AAAAtest", SSHAgent: true, SSHKeys: []string{"SHA256:test"}},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			fe := newFakeEnv(t)
			opts := fe.options()
			opts.Surface = Surface{Profile: "mine"}
			opts.GitName, opts.GitEmail = extra.GitName, extra.GitEmail
			opts.SigningKey, opts.SSHAgent, opts.SSHKeys = extra.SigningKey, extra.SSHAgent, extra.SSHKeys
			b, err := New(opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = b.prepareLaunch(context.Background(), record{ID: "x", Cwd: t.TempDir()})
			if err == nil {
				t.Fatal("named profile with git identity/signing was accepted")
			}
			if !strings.Contains(err.Error(), `"mine"`) {
				t.Fatalf("error must name the profile: %v", err)
			}
		})
	}
}
