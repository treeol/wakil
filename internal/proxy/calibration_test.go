package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- estimatePromptTokens / hasImages ---

func TestEstimatePromptTokens(t *testing.T) {
	cases := []struct {
		name         string
		payloadBytes int
		hasImages    bool
		learned      float64
		want         int64
	}{
		{"zero bytes", 0, false, 0.25, 0},
		{"negative bytes", -5, false, 0.25, 0},
		{"no learned ratio falls back to ~4 bytes/token", 4000, false, 0, 1000},
		{"image turn ignores learned ratio", 4000, true, 0.5, 1000},
		{"learned ratio applies", 4000, false, 0.5, 2000},
		{"learned ratio below 1 token clamps to 1", 1, false, 0.1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := estimatePromptTokens(c.payloadBytes, c.hasImages, c.learned); got != c.want {
				t.Errorf("estimatePromptTokens(%d, hasImages=%v, learned=%v) = %d, want %d",
					c.payloadBytes, c.hasImages, c.learned, got, c.want)
			}
		})
	}
}

func TestHasImages(t *testing.T) {
	if hasImages(nil) {
		t.Error("nil messages must not report images")
	}
	if hasImages([]Message{{Role: "user", Content: strPtr("hi")}}) {
		t.Error("text-only message must not report images")
	}
	img := []Message{{Role: "user", Content: strPtr("what"), Images: []ImagePart{{DataURL: "data:image/png;base64,AAAA"}}}}
	if !hasImages(img) {
		t.Error("message with an image must report images")
	}
}

// --- calibration lifecycle ---

func TestCalibrationKeyIsolatesIdentity(t *testing.T) {
	base := calibrationKey("http://a", "openai", "m1", "b1")
	same := calibrationKey("http://a", "openai", "m1", "b1")
	if base != same {
		t.Fatalf("same identity produced different keys: %q vs %q", base, same)
	}
	for _, other := range []string{
		calibrationKey("http://b", "openai", "m1", "b1"),    // base URL
		calibrationKey("http://a", "ilm-proxy", "m1", "b1"), // kind
		calibrationKey("http://a", "openai", "m2", "b1"),    // model
		calibrationKey("http://a", "openai", "m1", "b2"),    // backend
	} {
		if other == base {
			t.Errorf("distinct identity collided on key %q", base)
		}
	}
}

func TestLearnCalibrationGuards(t *testing.T) {
	c := &Client{BaseURL: "http://a", Kind: "openai", Model: "m", Backend: "b"}

	// Estimated usage (Exact=false) teaches nothing.
	c.learnCalibration(UsageStat{InputTok: 1000, Exact: false}, 4000, nil, false)
	if c.calibratedTokensPerByte() != 0 {
		t.Errorf("Exact=false must not be learned; ratio = %v", c.calibratedTokensPerByte())
	}

	// Zero input tokens teaches nothing.
	c.learnCalibration(UsageStat{InputTok: 0, Exact: true}, 4000, nil, false)
	if c.calibratedTokensPerByte() != 0 {
		t.Errorf("zero InputTok must not be learned; ratio = %v", c.calibratedTokensPerByte())
	}

	// Image turn teaches nothing.
	img := []Message{{Role: "user", Content: strPtr("x"), Images: []ImagePart{{DataURL: "data:image/png;base64,AAAA"}}}}
	c.learnCalibration(UsageStat{InputTok: 1000, Exact: true}, 4000, img, false)
	if c.calibratedTokensPerByte() != 0 {
		t.Errorf("image turn must not be learned; ratio = %v", c.calibratedTokensPerByte())
	}

	// Trimmed request teaches nothing.
	c.learnCalibration(UsageStat{InputTok: 1000, Exact: true}, 4000, nil, true)
	if c.calibratedTokensPerByte() != 0 {
		t.Errorf("trimmed request must not be learned; ratio = %v", c.calibratedTokensPerByte())
	}
}

func TestLearnCalibrationCumulative(t *testing.T) {
	c := &Client{BaseURL: "http://a", Kind: "openai", Model: "m", Backend: "b"}

	// Two samples: 4000B→1000tok (0.25) and 8000B→4000tok (0.5). Cumulative
	// mean over (4000+8000)B and (1000+4000)tok = 5000/12000 ≈ 0.4166…
	c.learnCalibration(UsageStat{InputTok: 1000, Exact: true}, 4000, nil, false)
	c.learnCalibration(UsageStat{InputTok: 4000, Exact: true}, 8000, nil, false)

	want := 5000.0 / 12000.0
	got := c.calibratedTokensPerByte()
	if got < want-1e-9 || got > want+1e-9 {
		t.Errorf("cumulative ratio = %v, want %v", got, want)
	}
}

func TestCalibratedTokensPerByteKeyed(t *testing.T) {
	c := &Client{BaseURL: "http://a", Kind: "openai", Model: "m", Backend: "b"}
	c.learnCalibration(UsageStat{InputTok: 1000, Exact: true}, 4000, nil, false)

	if c.calibratedTokensPerByte() == 0 {
		t.Fatal("learned ratio should be nonzero for the matching identity")
	}

	// Switching model yields a different key → no ratio.
	c.Model = "other-model"
	if c.calibratedTokensPerByte() != 0 {
		t.Errorf("model switch must not reuse ratio; got %v", c.calibratedTokensPerByte())
	}
}

// TestStreamLearnsCalibrationForNextTurn drives two full Stream calls: the
// first reports an authoritative prompt_tokens (1000) over a small payload,
// which must be learned; the second reports NO usage, so its provisional
// estimate must use the learned ratio rather than the flat ~4 bytes/token
// guess. We assert the second call's LastUsage is Exact=false (provisional) and
// that the learned ratio would estimate the same payload differently from the
// flat fallback.
func TestStreamLearnsCalibrationForNextTurn(t *testing.T) {
	var muCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		muCount++
		w.Header().Set("Content-Type", "text/event-stream")
		if muCount == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":20}}\n\n")
		} else {
			// No usage chunk → provisional estimate stays in LastUsage.
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok2\"},\"finish_reason\":null}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m", HTTP: http.DefaultClient}

	// First call: authoritative usage learned.
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream 1: %v", err)
	}
	ratio := c.calibratedTokensPerByte()
	if ratio <= 0 {
		t.Fatal("authoritative usage should have been learned as a calibration ratio")
	}
	if !c.LastUsage().Exact {
		t.Fatal("usage chunk present — Exact should be true after call 1")
	}

	// Second call: no usage → provisional estimate, not Exact, and it must use
	// the learned ratio (which, for this payload, differs from the flat guess).
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream 2: %v", err)
	}
	u2 := c.LastUsage()
	if u2.Exact {
		t.Fatal("second call reported no usage — Exact must be false (provisional)")
	}
	if u2.InputTok <= 0 {
		t.Fatal("second call must leave a positive provisional InputTok")
	}
	flat := ApproxTokens(len([]byte("hi")))
	if ratio > 0 && u2.InputTok == flat {
		t.Errorf("second-call provisional estimate %d equals the flat fallback %d — learned ratio not applied", u2.InputTok, flat)
	}
}

// TestCalibrationKeyUsesConfiguredModelForOpenAI: the canonical identity for an
// OpenAI-kind endpoint uses ConfiguredModel (the wire model), not c.Model.
func TestCalibrationKeyUsesConfiguredModelForOpenAI(t *testing.T) {
	c := &Client{BaseURL: "http://a", Kind: KindOpenAI, ConfiguredModel: "wire-model", Model: "session-alias"}
	key := c.calibrationKeyFor()

	// Same effective identity (ConfiguredModel governs) → same key even if Model differs.
	c2 := &Client{BaseURL: "http://a", Kind: KindOpenAI, ConfiguredModel: "wire-model", Model: "different-alias"}
	if c2.calibrationKeyFor() != key {
		t.Error("OpenAI identity must be keyed on ConfiguredModel, not c.Model")
	}

	// A different wire model → different key.
	c3 := &Client{BaseURL: "http://a", Kind: KindOpenAI, ConfiguredModel: "other", Model: "session-alias"}
	if c3.calibrationKeyFor() == key {
		t.Error("different ConfiguredModel must produce a different key")
	}
}
