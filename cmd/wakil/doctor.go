package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/sandbox/hisn"
)

// cmdRunner runs a container-runtime CLI command and returns its combined
// output. Injected so tests don't need a real daemon.
type cmdRunner func(ctx context.Context, name string, args ...string) (string, error)

func execRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// runDoctor implements `wakil doctor`. It only talks to the local container
// runtime (CLI → local daemon socket); no other network access. Returns the
// process exit code: 1 when no usable container runtime is found, else 0.
func runDoctor(w io.Writer, argv []string, run cmdRunner) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(w)
	cfgPath := fs.String("config", config.DefaultConfigPath(), "config file path")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 0. The default sandbox: hisn when it is usable and nothing in the config
	// chose a backend, else docker. Reported first; a docker runtime is only
	// REQUIRED when docker is the backend that would run.
	ctxH, cancelH := context.WithTimeout(ctx, 30*time.Second)
	hv, hwhy := hisn.Probe(ctxH, "", config.MinHisnDefaultVersion)
	cancelH()
	hisnUsable := hwhy == ""
	if hisnUsable {
		fmt.Fprintf(w, "[ ok ] hisn %s: `hisn check` passes — the default sandbox backend (set sandbox.backend: docker to opt out)\n", hv)
	} else {
		fmt.Fprintf(w, "[info] hisn is not the default sandbox backend: %s\n", hwhy)
	}
	hisnWillRun := hisnUsable && backendIsDefaulted(*cfgPath)

	// 1. Container runtime: docker or podman, binary + reachable daemon.
	runtime := ""
	for _, bin := range []string{"docker", "podman"} {
		path, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		if _, err := run(ctx, bin, "info", "--format", "{{.ID}}"); err != nil {
			fmt.Fprintf(w, "[warn] %s found at %s but daemon not reachable: %v\n", bin, path, err)
			continue
		}
		fmt.Fprintf(w, "[ ok ] container runtime: %s (%s), daemon reachable\n", bin, path)
		runtime = bin
		break
	}
	if runtime == "" {
		if hisnWillRun {
			fmt.Fprintln(w, "[warn] no usable container runtime — not needed while hisn is the sandbox (docker is only used with sandbox.backend: docker)")
		} else {
			fmt.Fprintln(w, "[FAIL] no usable container runtime (install docker or podman and start its daemon, or run wakil with --sandbox hisn|direct)")
		}
	}

	// 2. Config file: path and whether it loaded. Missing is fine (defaults).
	image := config.DefaultSandboxImage()
	if _, err := os.Stat(*cfgPath); err != nil {
		fmt.Fprintf(w, "[info] config: %s not found (built-in defaults; created on first run)\n", *cfgPath)
	} else if cfg, err := config.LoadConfig([]string{"--config", *cfgPath}); err != nil {
		fmt.Fprintf(w, "[warn] config: %s failed to load: %v\n", *cfgPath, err)
	} else {
		fmt.Fprintf(w, "[ ok ] config: %s loaded\n", *cfgPath)
		image = cfg.Image
	}

	// 3. Sandbox image present locally (never pulls; prints the command).
	if runtime != "" {
		if _, err := run(ctx, runtime, "image", "inspect", image); err != nil {
			fmt.Fprintf(w, "[warn] sandbox image not present locally: %s\n       pull it with: %s pull %s\n", image, runtime, image)
		} else {
			fmt.Fprintf(w, "[ ok ] sandbox image present: %s\n", image)
		}
	}
	if runtime == "" && !hisnWillRun {
		return 1
	}
	return 0
}

// backendIsDefaulted reports whether the config at path (plus env and defaults)
// leaves the backend to the default resolution.
func backendIsDefaulted(path string) bool {
	cfg, err := config.LoadConfig([]string{"--config", path})
	return err == nil && cfg.Sandbox.BackendDefaulted
}
