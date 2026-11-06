package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/treeol/wakil/internal/config"
)

// legacyNoticeFlag is written into the project's store directory once the
// first-start notice has been printed (0.5.0, D4). Its presence suppresses the
// notice for that project forever.
const legacyNoticeFlag = "legacy-notice-shown"

// LegacyNotice returns the one first-start line for the current project (D4),
// or "" when none applies: no legacy store with entries, the project is the
// legacy path itself, or the notice was already shown for this project. When it
// returns a line it also records the flag, so the caller prints it at most once.
func LegacyNotice(cfg config.Config) string {
	k := ResolveWorkspaceKeys(cfg).WithLegacy(cfg)
	if k.MemoryDBPath == "" || k.LegacyKey == "" || k.LegacyKey == k.MemoryKey {
		return ""
	}
	if k.LegacyEntries <= 0 {
		return ""
	}
	flag := filepath.Join(filepath.Dir(k.MemoryDBPath), legacyNoticeFlag)
	if _, err := os.Stat(flag); err == nil {
		return ""
	}
	line := fmt.Sprintf("memory now keyed per project; %d legacy entries readable, run `wakil state migrate --from-legacy` to copy attributable ones", k.LegacyEntries)
	if err := os.MkdirAll(filepath.Dir(flag), 0o700); err == nil {
		_ = os.WriteFile(flag, []byte(line+"\n"), 0o600)
	}
	return line
}
