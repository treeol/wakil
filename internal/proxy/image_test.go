package proxy

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestMarshalWireMessages_TextOnlyByteIdentical verifies the golden no-op
// guarantee: text-only messages with no cache_control markers produce
// byte-identical JSON to json.Marshal of []Message.
func TestMarshalWireMessages_TextOnlyByteIdentical(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: strPtr("you are a helpful assistant")},
		{Role: "user", Content: strPtr("hello")},
		{Role: "assistant", Content: strPtr("hi there")},
	}

	wire, err := marshalWireMessages(msgs, nil)
	if err != nil {
		t.Fatalf("marshalWireMessages: %v", err)
	}
	wireBytes, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal(wire): %v", err)
	}

	directBytes, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("json.Marshal(msgs): %v", err)
	}

	if string(wireBytes) != string(directBytes) {
		t.Errorf("text-only wire differs from direct marshal:\nwire:   %s\ndirect: %s", wireBytes, directBytes)
	}
}

// TestMarshalWireMessages_NullContentByteIdentical verifies that null-content
// messages (tool-call turns) serialize byte-identically with no markers.
func TestMarshalWireMessages_NullContentByteIdentical(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", Content: nil, ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", Function: FunctionCall{Name: "read_file", Arguments: `{"path":"foo.go"}`}},
		}},
		{Role: "tool", Content: strPtr("file contents"), ToolCallID: "call_1"},
	}

	wire, err := marshalWireMessages(msgs, nil)
	if err != nil {
		t.Fatalf("marshalWireMessages: %v", err)
	}
	wireBytes, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal(wire): %v", err)
	}

	directBytes, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("json.Marshal(msgs): %v", err)
	}

	if string(wireBytes) != string(directBytes) {
		t.Errorf("null-content wire differs from direct marshal:\nwire:   %s\ndirect: %s", wireBytes, directBytes)
	}
}

// TestMarshalWireMessages_WithImages verifies that messages with images
// produce the correct OpenAI-compatible content parts array.
func TestMarshalWireMessages_WithImages(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: strPtr("what's in this image?"), Images: []ImagePart{
			{Path: "screenshot.png", DataURL: "data:image/png;base64,iVBORw0KGgo=", MIME: "image/png", Size: 10},
		}},
	}

	wire, err := marshalWireMessages(msgs, nil)
	if err != nil {
		t.Fatalf("marshalWireMessages: %v", err)
	}
	wireBytes, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal(wire): %v", err)
	}

	// Parse back and verify structure.
	var parsed []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(wireBytes, &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("expected 1 message, got %d", len(parsed))
	}

	// Content should be an array of 2 parts: text + image_url.
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(parsed[0].Content, &parts); err != nil {
		t.Fatalf("content is not an array: %v\ncontent: %s", err, parsed[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(parts))
	}

	// Part 0: text.
	var p0Type, p0Text string
	json.Unmarshal(parts[0]["type"], &p0Type)
	json.Unmarshal(parts[0]["text"], &p0Text)
	if p0Type != "text" || p0Text != "what's in this image?" {
		t.Errorf("part 0: type=%q text=%q", p0Type, p0Text)
	}

	// Part 1: image_url.
	var p1Type string
	json.Unmarshal(parts[1]["type"], &p1Type)
	if p1Type != "image_url" {
		t.Errorf("part 1: type=%q, want image_url", p1Type)
	}
	var imgURL struct {
		URL string `json:"url"`
	}
	json.Unmarshal(parts[1]["image_url"], &imgURL)
	if imgURL.URL != "data:image/png;base64,iVBORw0KGgo=" {
		t.Errorf("image_url url=%q", imgURL.URL)
	}
}

// TestMarshalWireMessages_ImagesWithCacheControl verifies that image messages
// with cache_control markers get the breakpoint on the text part.
func TestMarshalWireMessages_ImagesWithCacheControl(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: strPtr("describe this"), Images: []ImagePart{
			{Path: "photo.jpg", DataURL: "data:image/jpeg;base64,/9j/4AAQ=", MIME: "image/jpeg", Size: 20},
		}},
	}
	marked := map[int]bool{0: true}

	wire, err := marshalWireMessages(msgs, marked)
	if err != nil {
		t.Fatalf("marshalWireMessages: %v", err)
	}
	wireBytes, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal(wire): %v", err)
	}

	var parsed []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	json.Unmarshal(wireBytes, &parsed)

	var parts []map[string]json.RawMessage
	json.Unmarshal(parsed[0].Content, &parts)

	// Text part should have cache_control.
	var cc struct {
		Type string `json:"type"`
	}
	json.Unmarshal(parts[0]["cache_control"], &cc)
	if cc.Type != "ephemeral" {
		t.Errorf("text part cache_control type=%q, want ephemeral", cc.Type)
	}

	// Image part should NOT have cache_control.
	if _, has := parts[1]["cache_control"]; has {
		t.Errorf("image part should not have cache_control")
	}
}

// TestMarshalWireMessages_ImagesOnlyNoText verifies that a message with images
// but nil text content produces only image_url parts.
func TestMarshalWireMessages_ImagesOnlyNoText(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: nil, Images: []ImagePart{
			{Path: "img.png", DataURL: "data:image/png;base64,iVBOR=", MIME: "image/png", Size: 5},
		}},
	}

	wire, err := marshalWireMessages(msgs, nil)
	if err != nil {
		t.Fatalf("marshalWireMessages: %v", err)
	}
	wireBytes, _ := json.Marshal(wire)

	var parsed []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	json.Unmarshal(wireBytes, &parsed)

	var parts []map[string]json.RawMessage
	json.Unmarshal(parsed[0].Content, &parts)

	if len(parts) != 1 {
		t.Fatalf("expected 1 part (image only), got %d", len(parts))
	}
	var pType string
	json.Unmarshal(parts[0]["type"], &pType)
	if pType != "image_url" {
		t.Errorf("part type=%q, want image_url", pType)
	}
}

// TestMarshalWireMessages_MultipleImages verifies multiple images produce
// multiple image_url parts.
func TestMarshalWireMessages_MultipleImages(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: strPtr("compare these"), Images: []ImagePart{
			{Path: "a.png", DataURL: "data:image/png;base64,AAAA", MIME: "image/png", Size: 3},
			{Path: "b.png", DataURL: "data:image/png;base64,BBBB", MIME: "image/png", Size: 3},
			{Path: "c.png", DataURL: "data:image/png;base64,CCCC", MIME: "image/png", Size: 3},
		}},
	}

	wire, err := marshalWireMessages(msgs, nil)
	if err != nil {
		t.Fatalf("marshalWireMessages: %v", err)
	}
	wireBytes, _ := json.Marshal(wire)

	var parsed []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	json.Unmarshal(wireBytes, &parsed)

	var parts []map[string]json.RawMessage
	json.Unmarshal(parsed[0].Content, &parts)

	// 1 text + 3 images = 4 parts.
	if len(parts) != 4 {
		t.Fatalf("expected 4 parts (1 text + 3 images), got %d", len(parts))
	}
}

// TestLoadImage reads a real PNG file and verifies the data URL is correct.
func TestLoadImage(t *testing.T) {
	// Create a minimal valid PNG file (1x1 pixel, red).
	pngData := makePNG()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.png")
	if err := os.WriteFile(path, pngData, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	img, err := LoadImage(path)
	if err != nil {
		t.Fatalf("LoadImage: %v", err)
	}

	if img.MIME != "image/png" {
		t.Errorf("MIME=%q, want image/png", img.MIME)
	}
	if img.Size != len(pngData) {
		t.Errorf("Size=%d, want %d", img.Size, len(pngData))
	}
	expectedB64 := base64.StdEncoding.EncodeToString(pngData)
	expectedURL := "data:image/png;base64," + expectedB64
	if img.DataURL != expectedURL {
		t.Errorf("DataURL mismatch:\ngot:  %s\nwant: %s", img.DataURL, expectedURL)
	}
	if img.Path != path {
		t.Errorf("Path=%q, want %q", img.Path, path)
	}
}

// TestLoadImage_FakePNGRejectsText verifies that a file named .png but
// containing non-image bytes is rejected (content-authoritative MIME).
func TestLoadImage_FakePNGRejectsText(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "fake.png")
	if err := os.WriteFile(path, []byte("this is not a real PNG file"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadImage(path)
	if err == nil {
		t.Fatal("expected error for .png file with text content, got nil")
	}
}

// TestLoadImage_TooLarge verifies that files exceeding the size limit are rejected.
func TestLoadImage_TooLarge(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "big.png")
	// Create a file just over the limit.
	bigData := make([]byte, maxImageBytes+1)
	if err := os.WriteFile(path, bigData, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadImage(path)
	if err == nil {
		t.Fatal("expected error for oversized image, got nil")
	}
}

// TestLoadImage_NoExtensionSniffsContent verifies that a valid PNG without
// .png extension is accepted via content sniffing.
func TestLoadImage_NoExtensionSniffsContent(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "screenshot") // no extension
	pngData := makePNG()
	if err := os.WriteFile(path, pngData, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	img, err := LoadImage(path)
	if err != nil {
		t.Fatalf("LoadImage: %v", err)
	}
	if img.MIME != "image/png" {
		t.Errorf("MIME=%q, want image/png", img.MIME)
	}
}

// TestLoadImage_UnsupportedFormat verifies that non-image files are rejected.
func TestLoadImage_UnsupportedFormat(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "notimage.txt")
	if err := os.WriteFile(path, []byte("this is not an image"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadImage(path)
	if err == nil {
		t.Fatal("expected error for non-image file, got nil")
	}
}

// TestLoadImage_NotFound verifies that missing files return an error.
func TestLoadImage_NotFound(t *testing.T) {
	_, err := LoadImage("/nonexistent/path/file.png")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// TestImagePartPlaceholder verifies the placeholder text format.
func TestImagePartPlaceholder(t *testing.T) {
	img := ImagePart{Path: "/tmp/screenshots/foo.png", Size: 15360}
	p := img.Placeholder()
	// Should contain the filename and a human-readable size.
	if p == "" {
		t.Error("placeholder is empty")
	}
	if !contains(p, "foo.png") {
		t.Errorf("placeholder should contain filename: %q", p)
	}
	if !contains(p, "KB") {
		t.Errorf("placeholder should contain size unit: %q", p)
	}
}

// TestDetectMIMEByContent verifies magic byte detection for each format.
func TestDetectMIMEByContent(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		mime string
		ok   bool
	}{
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}, "image/png", true},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0, 0, 0, 0, 0}, "image/jpeg", true},
		{"gif87", []byte("GIF87a" + "000000"), "image/gif", true},
		{"gif89", []byte("GIF89a" + "000000"), "image/gif", true},
		{"webp", []byte("RIFF" + "0000" + "WEBP" + "0000"), "image/webp", true},
		{"unknown", []byte("random data 12"), "", false},
		{"tooShort", []byte{0x89, 'P'}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mime, ok := detectMIMEByContent(tc.data)
			if mime != tc.mime || ok != tc.ok {
				t.Errorf("detectMIMEByContent(%v) = %q,%v, want %q,%v", tc.data, mime, ok, tc.mime, tc.ok)
			}
		})
	}
}

// makePNG returns a minimal valid 1×1 red PNG file.
func makePNG() []byte {
	// A minimal 1x1 red PNG, pre-computed.
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, // PNG signature
		0x00, 0x00, 0x00, 0x0D, // IHDR length
		0x49, 0x48, 0x44, 0x52, // "IHDR"
		0x00, 0x00, 0x00, 0x01, // width=1
		0x00, 0x00, 0x00, 0x01, // height=1
		0x08, 0x02, // bit depth=8, color type=RGB
		0x00, 0x00, 0x00, // compression, filter, interlace
		0x90, 0x77, 0x53, 0xDE, // CRC
		0x00, 0x00, 0x00, 0x0C, // IDAT length
		0x49, 0x44, 0x41, 0x54, // "IDAT"
		0x08, 0xD7, 0x63, 0xF8, 0xCF, 0xC0, 0x00, 0x00, // compressed data
		0x01, 0x01, 0x01, 0x00, // CRC tail
		0x18, 0xDD, 0x03, 0x4F, // CRC
		0x00, 0x00, 0x00, 0x00, // IEND length
		0x49, 0x45, 0x4E, 0x44, // "IEND"
		0xAE, 0x42, 0x60, 0x82, // CRC
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
