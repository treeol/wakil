package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/initcmd"
	"github.com/treeol/wakil/internal/sandbox/hisn"
)

// readSecretTTY reads one line from the controlling terminal with echo off. It
// fails (and init then refuses to read a key) when echo cannot be disabled.
func readSecretTTY(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("stdin is not a terminal")
	}
	old, err := term.GetState(fd)
	if err != nil {
		return "", fmt.Errorf("cannot read the terminal state: %w", err)
	}
	fmt.Fprint(os.Stdout, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stdout)
	_ = term.Restore(fd, old)
	if err != nil {
		return "", fmt.Errorf("cannot disable echo: %w", err)
	}
	return string(b), nil
}

func initEnv(in io.Reader, out io.Writer, interactive bool, cfgPath string) initcmd.Env {
	return initcmd.Env{
		In:          in,
		Out:         out,
		Interactive: interactive,
		ReadSecret:  readSecretTTY,
		ConfigPath:  cfgPath,
		ProbeSandbox: func(ctx context.Context) (string, string) {
			return hisn.Probe(ctx, "", config.MinHisnDefaultVersion)
		},
		DockerAvailable: func() bool { _, err := exec.LookPath("docker"); return err == nil },
	}
}

// stdioIsTerminal reports whether stdin and stdout are both terminals.
func stdioIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// runInit implements `wakil init`. It returns the exit code.
func runInit(args []string) int {
	o, err := initcmd.ParseArgs(args, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	path := config.DefaultConfigPath()
	return initcmd.Run(context.Background(), o, initEnv(os.Stdin, os.Stdout, stdioIsTerminal(), path))
}

// firstRun handles a NoConfigError at start. It returns true when setup ran
// and the caller should reload the config, false when the caller must exit
// with the returned code.
func firstRun(nc *config.NoConfigError, interactive bool, in io.Reader, out, errw io.Writer, doInit func(initcmd.Options) int) (retry bool, code int) {
	if !interactive {
		fmt.Fprintln(errw, "no config: run `wakil init` (or see README Quickstart)")
		return false, 1
	}
	prompt := "No config found. Run setup now? [Y/n] "
	opts := initcmd.Options{}
	defaultYes := true
	if nc.FileExists {
		// An existing file is only ever replaced on an explicit "y" (the old
		// file is copied to a 0600 backup first).
		prompt = fmt.Sprintf("%s has no endpoint configured. Replace it with a new config? It is backed up first. [y/N] ", nc.Path)
		opts.Force = true
		defaultYes = false
	}
	fmt.Fprint(out, prompt)
	a := strings.TrimSpace(readLine(in))
	yes := a != "" && (a[0] == 'y' || a[0] == 'Y')
	if a == "" {
		yes = defaultYes
	}
	if !yes {
		fmt.Fprintln(errw, "Run `wakil init` when you are ready (see README Quickstart for the manual way).")
		return false, 1
	}
	if c := doInit(opts); c != 0 {
		return false, c
	}
	return true, 0
}

// readLine reads one line byte by byte, so nothing past the newline is
// consumed from a stream the next reader will use.
func readLine(in io.Reader) string {
	var out []byte
	var b [1]byte
	for len(out) < 256 {
		n, err := in.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				break
			}
			out = append(out, b[0])
		}
		if err != nil {
			break
		}
	}
	return string(out)
}
