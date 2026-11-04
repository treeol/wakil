package wiring

import (
	"context"
	"errors"
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
func ResolveSandbox(ctx context.Context, cfg config.Config) config.Config {
	return resolveSandbox(cfg, func() config.HisnAvailability {
		v, why := hisn.Probe(ctx, cfg.Sandbox.Hisn.Binary, config.MinHisnDefaultVersion)
		return config.HisnAvailability{Version: v, Reason: why}
	})
}

// resolveSandbox is ResolveSandbox with an injectable probe (tests).
func resolveSandbox(cfg config.Config, probe func() config.HisnAvailability) config.Config {
	if !cfg.Sandbox.BackendDefaulted || cfg.ExecMode == "direct" {
		return cfg
	}
	avail := probe()
	extra := ""
	if avail.Reason == "" {
		// Only now that hisn would be chosen: would the project's own file stop
		// it? An unapproved file must never turn a working docker setup into a
		// hard error through the default alone.
		if _, _, err := loadProjectSurface(cfg); err != nil {
			var na *project.ErrNotApproved
			if errors.As(err, &na) {
				extra = project.RelPath + " is not approved (run `wakil sandbox trust`)"
			} else {
				extra = err.Error()
			}
		} else if why := workspaceFitsState(cfg); why != "" {
			extra = why
		}
	}
	return config.ApplyDefaultBackend(cfg, avail, extra)
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
