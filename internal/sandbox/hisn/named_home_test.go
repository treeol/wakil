package hisn

import (
	"strings"
	"testing"
)

// B1: a named profile cannot carry the [home] fragment, so an effective tmpfs
// home must be refused by the constructor, naming the key, never dropped.
func TestNamedProfileRefusesEffectiveTmpfsHome(t *testing.T) {
	fe := newFakeEnv(t)
	opts := fe.options()
	opts.Surface = Surface{Profile: "mine"}
	opts.Home = HomeTmpfs // global sandbox.hisn.home
	_, err := New(opts)
	if err == nil {
		t.Fatal("named profile with global tmpfs home was accepted")
	}
	if !strings.Contains(err.Error(), "sandbox.hisn.home") || !strings.Contains(err.Error(), `"mine"`) {
		t.Fatalf("error must name the key and the profile: %v", err)
	}
}

// B1: a project-level tmpfs override on a named profile is refused too.
func TestNamedProfileRefusesProjectTmpfsHome(t *testing.T) {
	fe := newFakeEnv(t)
	opts := fe.options()
	opts.Surface = Surface{Profile: "mine", Home: HomeTmpfs}
	opts.Home = HomePersist
	if _, err := New(opts); err == nil {
		t.Fatal("named profile with project tmpfs home was accepted")
	}
}

// B1 negative: a named profile with the default persist home is accepted.
func TestNamedProfileAcceptsPersistHome(t *testing.T) {
	fe := newFakeEnv(t)
	opts := fe.options()
	opts.Surface = Surface{Profile: "mine"}
	opts.Home = HomePersist
	if _, err := New(opts); err != nil {
		t.Fatalf("named profile with persist home refused: %v", err)
	}
}
