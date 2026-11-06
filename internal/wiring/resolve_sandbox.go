package wiring

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/sandbox/hisn"
	"github.com/treeol/wakil/internal/sandbox/project"
)

// ResolveSandbox settles the backend once, before an executor exists. When
// nothing chose a backend (no sandbox.backend in file/env/flag, no exec_mode)
// it flips the default to hisn only if ALL hold: `hisn check` passes, hisn >=
// 0.3.1 is in PATH, and the project's .wakil/sandbox.json (if it asks for
// anything) is approved. Otherwise the backend stays docker and the reason is
// in cfg.StartupBanner(). An explicit backend is returned untouched. The three
// entry points (TUI, `wakil run`, daemon) call this right after LoadConfig and
// hand the RESOLVED config down; NewExecutor itself never probes.
//
// A broken or unapproved .wakil/sandbox.json is a startup error: the config is
// returned unchanged (no backend fallback, no browser) and the error names the
// file, the reason, and `wakil sandbox trust` when unapproved. Callers must
// check the error and exit before printing the banner or starting a session.
func ResolveSandbox(ctx context.Context, cfg config.Config) (config.Config, error) {
	return resolveSandbox(cfg, func() (config.HisnAvailability, error) {
		v, why := hisn.Probe(ctx, cfg.Sandbox.Hisn.Binary, config.MinHisnDefaultVersion)
		return config.HisnAvailability{Version: v, Reason: why}, nil
	})
}

// resolveSandbox is ResolveSandbox with an injectable probe (tests).
func resolveSandbox(cfg config.Config, probe func() (config.HisnAvailability, error)) (config.Config, error) {
	if !cfg.Sandbox.BackendDefaulted || cfg.ExecMode == "direct" {
		return cfg, nil
	}
	avail, err := probe()
	if err != nil {
		return cfg, err
	}
	if avail.Reason == "" {
		// Only now that hisn would be chosen: would the project's own file stop
		// it? A broken or unapproved file is a startup error — it must never
		// widen the backend or turn on the browser through the default path.
		if _, _, err := loadProjectSurface(cfg); err != nil {
			var na *project.ErrNotApproved
			if errors.As(err, &na) {
				return cfg, fmt.Errorf("%s: %w (run `wakil sandbox trust`)", project.RelPath, err)
			}
			return cfg, fmt.Errorf("%s: %w", project.RelPath, err)
		}
		if why := workspaceFitsState(cfg); why != "" {
			return cfg, fmt.Errorf("%s", why)
		}
	}
	return config.ApplyDefaultBackend(cfg, avail, ""), nil
}

// workspaceFitsState reports why the generated profile (kept in Wakil's state
// dir) would be refused by hisn for this workspace: a workspace that contains
// the state dir would make hisn answer E_PROFILE_UNTRUSTED. "" = fine.
func workspaceFitsState(cfg config.Config) string {
	ws := cfg.HostWorkDir
	state := filepath.Join(hisnStateDir(), "hisn")
	if err := hisn.CheckStateOutside(state, ws, cfg.Sandbox.Hisn.HomeRoot); err != nil {
		return "the workspace contains Wakil's state directory (hisn would refuse the profile)"
	}
	return ""
}
