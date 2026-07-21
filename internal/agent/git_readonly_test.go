package agent

import "testing"

// TestGitReadOnly_Subcommands pins the read-only classification of common git
// investigative subcommands. These should be auto-approved (IsReadOnlyShell=true)
// so the agent can run git diff/status/log without a confirmation prompt.
func TestGitReadOnly_Subcommands(t *testing.T) {
	readOnly := []string{
		"git status",
		"git status --short",
		"git status --porcelain",
		"git diff",
		"git diff --stat",
		"git diff --name-only",
		"git diff HEAD",
		"git diff --cached",
		"git log",
		"git log --oneline -5",
		"git log --oneline --graph -10",
		"git show",
		"git show HEAD",
		"git show --stat HEAD",
		"git blame file.go",
		"git shortlog -sn",
		"git ls-files",
		"git rev-parse HEAD",
		"git describe --tags",
		"git reflog",
		"git diff-tree HEAD",
		"git cat-file -p HEAD",
		"git ls-remote --heads origin",
		"git for-each-ref",
		"git rev-list --count HEAD",
		"git grep pattern",
		"git range-diff main...feature",
		"git merge-base main feature",
		"git cherry",
		"git branch",
		"git branch -v",
		"git branch -a",
		"git stash list",
		"git config --get user.name",
		"git config --list",
	}
	for _, cmd := range readOnly {
		if !IsReadOnlyShell(cmd) {
			t.Errorf("IsReadOnlyShell(%q) = false; want true (read-only git subcommand)", cmd)
		}
		// Read-only commands must never also be destructive (mutual exclusion).
		if IsDestructiveShell(cmd) {
			t.Errorf("IsDestructiveShell(%q) = true; read-only command must not be destructive", cmd)
		}
	}
}

// TestGitMutating_NotReadOnly pins that git mutating subcommands are NOT
// auto-approved — they must go through the normal confirmation gate.
func TestGitMutating_NotReadOnly(t *testing.T) {
	mutating := []string{
		"git add .",
		"git add -A",
		"git commit -m message",
		"git commit --amend",
		"git stash",
		"git stash push",
		"git stash pop",
		"git stash apply",
		"git merge main",
		"git rebase main",
		"git checkout main",
		"git checkout -b feature",
		"git switch main",
		"git fetch",
		"git pull",
		"git push",
		"git push origin main",
		"git tag v1.0",
		"git config user.name New",
		"git config --set user.name New",
	}
	for _, cmd := range mutating {
		if IsReadOnlyShell(cmd) {
			t.Errorf("IsReadOnlyShell(%q) = true; want false (mutating git subcommand)", cmd)
		}
	}
}

// TestGitDestructive_RemainsDestructive pins that git subcommands that are
// destructive (reset, clean, checkout --, push --force, stash drop/clear,
// branch -D) remain classified as destructive even after adding git to the
// read-only allowlist. The destructive classifier runs first in the confirmer.
func TestGitDestructive_RemainsDestructive(t *testing.T) {
	destructive := []string{
		"git reset --hard",
		"git reset --hard HEAD~1",
		"git clean -fdx",
		"git clean -fd",
		"git checkout -- .",
		"git checkout -- file.go",
		"git push --force",
		"git push -f origin main",
		"git stash drop",
		"git stash clear",
		"git branch -D feature",
	}
	for _, cmd := range destructive {
		if !IsDestructiveShell(cmd) {
			t.Errorf("IsDestructiveShell(%q) = false; want true (destructive git subcommand)", cmd)
		}
		// Destructive commands must not also be classified as read-only.
		if IsReadOnlyShell(cmd) {
			t.Errorf("IsReadOnlyShell(%q) = true; destructive command must not be read-only", cmd)
		}
	}
}

// TestGitBare_NotReadOnly pins that bare "git" with no subcommand is not
// auto-approved.
func TestGitBare_NotReadOnly(t *testing.T) {
	if IsReadOnlyShell("git") {
		t.Error("IsReadOnlyShell(\"git\") = true; bare git should not be auto-approved")
	}
}
