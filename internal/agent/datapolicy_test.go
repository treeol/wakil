package agent

// datapolicy_test.go covers B3 (correction_capture gate) and B5 (mid-session
// cache-reset line on /zdr and /region changes).

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// correctionTestApp builds a parent App with a real memory store and a
// counting confirmer, wired for correction capture.
func correctionTestApp(t *testing.T, capture bool) (*App, *int) {
	t.Helper()
	confirmed := new(int)
	store, err := memory.Open(filepath.Join(t.TempDir(), "memory.db"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		Cfg:         config.Config{CorrectionCapture: capture},
		MemoryStore: store,
		Confirm: func(_, _, _ string, _ bool) bool {
			*confirmed++
			return true
		},
	}
	return app, confirmed
}

// TestCorrectionCapture_OffSkipsDetection: with correction_capture off, even
// an explicit correction message produces no prompt and no suspension of
// auto mode.
func TestCorrectionCapture_OffSkipsDetection(t *testing.T) {
	app, confirmed := correctionTestApp(t, false)
	app.SetAutoApprove(true)
	app.detectAndProposeCorrection(context.Background(), "no, use tabs not spaces")
	if *confirmed != 0 {
		t.Errorf("Confirm called %d times with capture off, want 0", *confirmed)
	}
	// Correction capture off: detectAndProposeCorrection returned before
	// any detection ran, so auto mode is untouched.
	if !app.Consent().AutoApprove {
		t.Error("auto mode must be untouched when capture is off")
	}
}

// TestCorrectionCapture_OnPrompts: with correction_capture on, an explicit
// correction message produces a confirmation prompt.
func TestCorrectionCapture_OnPrompts(t *testing.T) {
	app, confirmed := correctionTestApp(t, true)
	app.detectAndProposeCorrection(context.Background(), "no, use tabs not spaces")
	if *confirmed == 0 {
		t.Error("Confirm must be called for a correction when capture is on")
	}
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
