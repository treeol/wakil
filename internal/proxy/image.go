package proxy

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

// ImagePart represents one image attached to a user message. It is carried
// alongside Message (via Message.Images) and serialized at wire-encoding time
// as an OpenAI-compatible image_url content part:
//
//	{"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}
//
// ImagePart itself is never JSON-serialized directly — marshalWireMessages
// converts it to a contentPart at serialization time. The DataURL field holds
// the full "data:image/<mime>;base64,<encoded>" URI ready for the wire.
type ImagePart struct {
	// Path is the original file path the user provided (for display only;
	// not sent to the model).
	Path string
	// DataURL is the complete data URI: "data:image/png;base64,iVBOR...".
	DataURL string
	// MIME is the detected MIME type, e.g. "image/png".
	MIME string
	// Size is the raw file size in bytes (before base64 encoding).
	Size int
}

// Placeholder returns a human-readable label for the TUI transcript, e.g.
// "[image: screenshot.png · 480×300 · 24 KB]". Dimensions are not available
// without decoding the image, so only filename and size are shown in v1.
func (img ImagePart) Placeholder() string {
	name := filepath.Base(img.Path)
	if name == "" || name == "." {
		name = "image"
	}
	return fmt.Sprintf("[image: %s · %s]", name, humanSize(img.Size))
}

// humanSize formats a byte count for human display.
func humanSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// DetectMIME sniffs the image format from magic bytes. Returns the MIME type
// and true if recognized; ("", false) otherwise. Exported so the TUI's
// clipboard reader can pre-validate clipboard data before loading.
func DetectMIME(data []byte) (string, bool) {
	return detectMIMEByContent(data)
}

// detectMIMEByContent sniffs the image format from magic bytes. Returns the
// MIME type and true if recognized; ("", false) otherwise.
func detectMIMEByContent(data []byte) (string, bool) {
	if len(data) < 12 {
		return "", false
	}
	// PNG: \x89PNG\r\n\x1a\n
	if data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G' {
		return "image/png", true
	}
	// JPEG: \xff\xd8\xff
	if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "image/jpeg", true
	}
	// GIF: GIF87a or GIF89a
	if string(data[0:6]) == "GIF87a" || string(data[0:6]) == "GIF89a" {
		return "image/gif", true
	}
	// WebP: RIFF....WEBP
	if string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp", true
	}
	return "", false
}

// maxImageBytes caps the raw file size accepted by LoadImage. 20 MB is
// generous for screenshots/photos while preventing memory exhaustion from
// accidentally attaching a huge file. Base64 encoding inflates this ~33%
// on the wire, so the effective request-body contribution is ~27 MB max.
const maxImageBytes = 20 * 1024 * 1024 // 20 MB

// LoadImage reads an image file from path, detects its MIME type, and returns
// an ImagePart with the base64-encoded data URL ready for the wire. The path
// must point to a readable file of a supported image format (png, jpeg, gif,
// webp). Returns an error if the file cannot be read, is too large, or the
// format is not recognized.
//
// MIME detection is content-authoritative: magic bytes are checked and the
// file extension is ignored. A file named .png that doesn't contain PNG
// magic bytes is rejected. This prevents a renamed non-image file from being
// accepted based on its extension alone.
func LoadImage(path string) (ImagePart, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ImagePart{}, fmt.Errorf("read image %s: %w", path, err)
	}

	if len(data) > maxImageBytes {
		return ImagePart{}, fmt.Errorf("image %s is %s (limit %s)", path, humanSize(len(data)), humanSize(maxImageBytes))
	}

	// Content-authoritative MIME detection: sniff magic bytes first.
	// If sniffing fails, the file is rejected regardless of extension —
	// a file named .png that doesn't contain PNG magic bytes is not a PNG.
	mime, ok := detectMIMEByContent(data)
	if !ok {
		return ImagePart{}, fmt.Errorf("unsupported image format: %s (expected png, jpeg, gif, or webp)", path)
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	dataURL := "data:" + mime + ";base64," + encoded

	return ImagePart{
		Path:    path,
		DataURL: dataURL,
		MIME:    mime,
		Size:    len(data),
	}, nil
}

// LoadImageFromBytes creates an ImagePart from raw image bytes, using the
// provided label as the display path. It applies the same MIME sniffing and
// size limit as LoadImage but reads from memory instead of disk. Used by the
// TUI's clipboard-paste path to attach images without a temp file.
func LoadImageFromBytes(data []byte, label string) (ImagePart, error) {
	if len(data) > maxImageBytes {
		return ImagePart{}, fmt.Errorf("image is %s (limit %s)", humanSize(len(data)), humanSize(maxImageBytes))
	}
	mime, ok := detectMIMEByContent(data)
	if !ok {
		return ImagePart{}, fmt.Errorf("unsupported image format (expected png, jpeg, gif, or webp)")
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	return ImagePart{
		Path:    label,
		DataURL: "data:" + mime + ";base64," + encoded,
		MIME:    mime,
		Size:    len(data),
	}, nil
}
