package agent

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// TestAutoCounselResultHeader pins Group 5: the result of an automatic
// mashura__debug round (which the model did not call) must begin with one
// line stating that Wakil started it automatically.
func TestAutoCounselResultHeader(t *testing.T) {
	want := "[automatic mashura round — Wakil started this round automatically; you did not call it]\n\n"
	if autoCounselResultHeader != want {
		t.Fatalf("autoCounselResultHeader = %q, want %q", autoCounselResultHeader, want)
	}
	if !strings.HasPrefix(autoCounselResultHeader+`REPORT BODY`, want) {
		t.Fatal("header must prefix the result verbatim")
	}
	// The header is one line: exactly one \n before the report starts.
	lines := strings.SplitN(strings.TrimSuffix(want, "\n"), "\n", 2)
	if len(lines) != 2 || lines[1] != "" {
		t.Fatalf("header must be one line plus blank line, got %q", want)
	}
	_ = proxy.Message{}
	_ = config.Config{}
}
