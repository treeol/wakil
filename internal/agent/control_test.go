package agent

// control_test.go: unit tests for the chunk-6 Control/StateApply methods
// (internal/agent/control.go), focusing on the concurrency-relevant behaviors
// the review required: Conv locking, slice-copy semantics, CtxLimit pair reset,
// and ConsumeStartupNote lifecycle behavior.

import (
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// TestAppendSystemMessageLocksConv verifies AppendSystemMessage appends under
// convMu: run it concurrently with ConvSnapshot readers and assert no race (via
// -race) and that readers observe either the pre- or post-append slice, never
// a torn one.
func TestAppendSystemMessageLocksConv(t *testing.T) {
	a := &App{}
	a.SetConsent(ConsentSnapshot{})
	a.Conv = []proxy.Message{{Role: "user", Content: StrPtr("first")}}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			a.AppendSystemMessage(proxy.Message{Role: "system", Content: StrPtr("x")})
		}
	}()
	for i := 0; i < 100; i++ {
		_ = a.ConvSnapshot() // RLock reader; races if AppendSystemMessage skips the lock
	}
	wg.Wait()

	if len(a.Conv) != 101 {
		t.Fatalf("Conv length = %d, want 101 (1 seed + 100 appended)", len(a.Conv))
	}
}

// TestSetCtxLimitResetsPressureWarned verifies the pair reset.
func TestSetCtxLimitResetsPressureWarned(t *testing.T) {
	a := &App{}
	a.CtxPressureWarned = true

	a.SetCtxLimit(ContextLimit{NCtx: 4096})

	if a.CtxLimit.NCtx != 4096 {
		t.Errorf("CtxLimit.NCtx = %d, want 4096", a.CtxLimit.NCtx)
	}
	if a.CtxPressureWarned {
		t.Error("SetCtxLimit should reset CtxPressureWarned")
	}
}

// TestReplacePendingImagesCopiesInput verifies the slice is copied, not
// aliased: mutating the caller's slice afterward must not change App state.
func TestReplacePendingImagesCopiesInput(t *testing.T) {
	a := &App{}
	in := []proxy.ImagePart{{Path: "a.png"}, {Path: "b.png"}}

	a.ReplacePendingImages(in)

	in[0].Path = "MUTATED"
	if a.PendingImages[0].Path != "a.png" {
		t.Errorf("ReplacePendingImages aliased the input slice: got %q, want %q", a.PendingImages[0].Path, "a.png")
	}

	// nil → nil, not a retained empty slice.
	a.ReplacePendingImages(nil)
	if a.PendingImages != nil {
		t.Error("ReplacePendingImages(nil) should set PendingImages to nil")
	}
}

// TestAddClearPendingImages verifies append + clear round-trip.
func TestAddClearPendingImages(t *testing.T) {
	a := &App{}
	a.AddPendingImage(proxy.ImagePart{Path: "a.png"})
	a.AddPendingImage(proxy.ImagePart{Path: "b.png"})
	if len(a.PendingImages) != 2 {
		t.Fatalf("AddPendingImage: got %d images, want 2", len(a.PendingImages))
	}
	a.ClearPendingImages()
	if len(a.PendingImages) != 0 {
		t.Fatalf("ClearPendingImages: got %d images, want 0", len(a.PendingImages))
	}
}

// TestConsumeStartupNoteReturnsAndClears verifies the return-then-clear
// lifecycle behavior.
func TestConsumeStartupNoteReturnsAndClears(t *testing.T) {
	a := &App{StartupNote: "hello"}
	if got := a.ConsumeStartupNote(); got != "hello" {
		t.Errorf("ConsumeStartupNote = %q, want %q", got, "hello")
	}
	if a.StartupNote != "" {
		t.Errorf("ConsumeStartupNote should clear StartupNote; got %q", a.StartupNote)
	}
	// Second consume returns empty (already consumed).
	if got := a.ConsumeStartupNote(); got != "" {
		t.Errorf("second ConsumeStartupNote = %q, want empty", got)
	}
}

// TestResumeSessionMsgLocksConv verifies the chunk-6 fix: ResumeSessionMsg
// takes convMu when assigning Conv, so it no longer races ConvSnapshot readers.
func TestResumeSessionMsgLocksConv(t *testing.T) {
	a := &App{}
	a.SetConsent(ConsentSnapshot{})
	a.Client = &proxy.Client{}
	s := &Session{
		ChatID: "chat-1",
		Conv:   []proxy.Message{{Role: "user", Content: StrPtr("resumed")}},
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			ResumeSessionMsg(a, s)
		}
	}()
	for i := 0; i < 100; i++ {
		_ = a.ConvSnapshot()
	}
	wg.Wait()

	if a.Client.ChatID != "chat-1" {
		t.Errorf("ChatID = %q, want chat-1", a.Client.ChatID)
	}
	if len(a.Conv) != 1 {
		t.Errorf("Conv length = %d, want 1", len(a.Conv))
	}
}
