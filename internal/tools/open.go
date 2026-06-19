package tools

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// OpenOnHost opens a URL or file path in the user's default application on the
// host — the machine wakil runs on, in the user's desktop session — regardless
// of where run_shell executes. This is the point: when shell commands run inside
// a sandbox container, that container is headless and isolated, so xdg-open there
// can't reach the host browser; wakil's own process can.
//
// The launcher is started detached and reaped asynchronously: xdg-open hands the
// URL to the desktop and the browser keeps running independently, so we must not
// block on (or kill) it. A failure to launch at all (e.g. xdg-open missing) is
// reported.
func OpenOnHost(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("no URL or path given")
	}
	cmd := hostOpenCmd(target)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go func() { _ = cmd.Wait() }() // reap so the launcher isn't left a zombie
	return "opened " + target + " in the host's default application", nil
}

// hostOpenCmd builds the platform-appropriate "open" command.
func hostOpenCmd(target string) *exec.Cmd {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target)
	case "windows":
		return exec.Command("cmd", "/c", "start", "", target)
	default: // linux, *bsd
		return exec.Command("xdg-open", target)
	}
}
