package agent

// datapolicy_test.go covers B5 (mid-session
// cache-reset line on /zdr and /region changes). The B3 correction-capture
// gate tests were removed in 0.8.4 with the feature.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// testStore opens an in-memory memory store for testing. It lived in
// correction_test.go (removed in 0.8.4 with correction capture) but is used
// by the collapse_*_test.go files, so it moved here.
func testStore(t *testing.T) *memory.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := memory.Open(filepath.Join(dir, "memory.db"), dir)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestDataSwitch_MidSessionCacheNotice: changing /zdr or /region after the
// session has sent a request adds the cache-reset line to the output;
// before any request it does not.
func TestDataSwitch_MidSessionCacheNotice(t *testing.T) {
	notice := "Changing this mid-session resets the prompt cache."

	// Before any request: no notice.
	before := &App{Cfg: config.Config{ZDR: "off", Region: "off"}, Client: &proxy.Client{Model: "m"}}
	handled, _, cmd := handleDataSwitchCommand([]string{"/zdr", "on"}, before, "zdr")
	if !handled || cmd == nil {
		t.Fatal("/zdr on must be handled")
	}
	msg := cmd().(SysNoteMsg)
	if strings.Contains(msg.Text, notice) {
		t.Errorf("pre-send change must not carry the notice: %q", msg.Text)
	}

	// After a request has been sent: notice present.
	sent := &App{Cfg: config.Config{ZDR: "off", Region: "off"}, Client: &proxy.Client{Model: "m"}}
	// Drive the cumulative counter through the same publication path Stream
	// uses (provisional estimate, sent=true).
	sent.Client.SetUsageForRequestForTest(proxy.UsageStat{InputTok: 1}, true)
	for _, tc := range []struct {
		kind string
		args []string
	}{
		{"zdr", []string{"/zdr", "on"}},
		{"region", []string{"/region", "eu"}},
	} {
		handled, _, cmd := handleDataSwitchCommand(tc.args, sent, tc.kind)
		if !handled || cmd == nil {
			t.Fatalf("%s must be handled", tc.args[0])
		}
		msg := cmd().(SysNoteMsg)
		if !strings.Contains(msg.Text, notice) {
			t.Errorf("mid-session %s change must carry the notice: %q", tc.args[0], msg.Text)
		}
	}
	// Re-setting the same value mid-session changes nothing on the wire, so
	// it must not carry the reset line.
	if handled, _, cmd := handleDataSwitchCommand([]string{"/zdr", "on"}, sent, "zdr"); handled && cmd != nil {
		if msg := cmd().(SysNoteMsg); strings.Contains(msg.Text, notice) {
			t.Errorf("unchanged /zdr on must not carry the notice: %q", msg.Text)
		}
	}
	// /zdr reset also carries no reset line (it does not go through the
	// change path).
	if handled, _, cmd := handleDataSwitchCommand([]string{"/zdr", "reset"}, sent, "zdr"); handled && cmd != nil {
		if msg := cmd().(SysNoteMsg); strings.Contains(msg.Text, notice) {
			t.Errorf("/zdr reset must not carry the notice: %q", msg.Text)
		}
	}
}
