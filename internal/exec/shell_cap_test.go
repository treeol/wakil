package exec

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 200 KB of normal output, ~12x the timeout tail: the default cap (1 MB) must
// pass it through untouched, with no truncation notice.
const bigOutputCmd = "head -c 204800 /dev/zero | tr '\\0' 'x'"

// A command that never ends and floods output until its context expires.
const floodCmd = "while :; do head -c 65536 /dev/zero | tr '\\0' 'y'; done"

func TestShellResult(t *testing.T) {
	big := strings.Repeat("a", 3*TimeoutOutputTail)
	tests := []struct {
		name      string
		out       string
		omitted   int64
		timedOut  bool
		wantLen   int // length of the output after the notice line; -1 = no notice expected
		wantTotal string
	}{
		{"small normal", "hello\n", 0, false, -1, ""},
		{"big normal not cut", big, 0, false, -1, ""},
		{"capped normal keeps cap notice", big, 100, false, len(big), "[truncated, 49252 bytes total — showing last 49152]"},
		{"timeout cuts to tail", big, 0, true, TimeoutOutputTail, "[truncated, 49152 bytes total — showing last 16384]"},
		{"timeout small unchanged", "tiny", 0, true, -1, ""},
		{"timeout counts earlier omission", big, 1000, true, TimeoutOutputTail, "[truncated, 50152 bytes total — showing last 16384]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shellResult(tc.out, tc.omitted, tc.timedOut)
			if tc.wantLen < 0 {
				if strings.Contains(got, "[truncated") {
					t.Fatalf("unexpected notice: %.80q", got)
				}
				return
			}
			notice, body, ok := strings.Cut(got, "\n")
			if !ok || notice != tc.wantTotal {
				t.Fatalf("notice = %q, want %q", notice, tc.wantTotal)
			}
			if len(body) != tc.wantLen {
				t.Fatalf("body len = %d, want %d", len(body), tc.wantLen)
			}
		})
	}
}

func TestShellResultTimeoutDoesNotSplitRune(t *testing.T) {
	out := strings.Repeat("é", TimeoutOutputTail) // 2 bytes per rune
	got := shellResult(out, 0, true)
	_, body, _ := strings.Cut(got, "\n")
	if strings.ContainsRune(body, '\uFFFD') {
		t.Fatalf("tail starts mid-rune: %q…", body[:8])
	}
}

func TestDirectRunShellCapBehaviour(t *testing.T) {
	e, err := NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Run("200KB normal output is not truncated", func(t *testing.T) {
		out, err := e.RunShell(context.Background(), bigOutputCmd)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 204800 || strings.Contains(out, "[truncated") {
			t.Fatalf("normal output cut: len=%d notice=%v", len(out), strings.Contains(out, "[truncated"))
		}
	})
	t.Run("timed-out output is the last 16KB with notice", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
		defer cancel()
		out, err := e.RunShell(ctx, floodCmd)
		if err == nil {
			t.Fatal("expected the timed-out command to fail")
		}
		notice, body, ok := strings.Cut(out, "\n")
		if !ok || !strings.HasPrefix(notice, "[truncated, ") || !strings.HasSuffix(notice, "showing last 16384]") {
			t.Fatalf("notice = %q", notice)
		}
		if len(body) != TimeoutOutputTail {
			t.Fatalf("body len = %d, want %d", len(body), TimeoutOutputTail)
		}
	})
	t.Run("an explicit cap still applies to normal output", func(t *testing.T) {
		small := &DirectExecutor{root: e.root, ShellOutputCap: 1000}
		out, err := small.RunShell(context.Background(), bigOutputCmd)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out, "[truncated, 204800 bytes total — showing last 1000]\n") {
			t.Fatalf("cap/notice wrong: %.80q", out)
		}
	})
}

func TestBackendRunShellCapBehaviour(t *testing.T) {
	sb := &shBackend{root: t.TempDir()}
	ex := NewBackendExecutor(sb, BackendOptions{})
	defer ex.Close()
	_ = ex.BindChat(context.Background(), "c")
	t.Run("200KB normal output is not truncated", func(t *testing.T) {
		out, err := ex.RunShell(context.Background(), bigOutputCmd)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 204800 || strings.Contains(out, "[truncated") {
			t.Fatalf("normal output cut: len=%d", len(out))
		}
	})
	t.Run("timed-out output is the last 16KB with notice", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
		defer cancel()
		out, err := ex.RunShell(ctx, floodCmd)
		if err == nil {
			t.Fatal("expected the timed-out command to fail")
		}
		notice, body, ok := strings.Cut(out, "\n")
		if !ok || !strings.HasPrefix(notice, "[truncated, ") || !strings.HasSuffix(notice, "showing last 16384]") {
			t.Fatalf("notice = %q", notice)
		}
		if len(body) != TimeoutOutputTail {
			t.Fatalf("body len = %d, want %d", len(body), TimeoutOutputTail)
		}
	})
}
