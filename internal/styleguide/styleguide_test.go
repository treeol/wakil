package styleguide

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEmbeddedCatalogAndReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.md")
	if err := os.WriteFile(path, []byte("MY-GO-RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewCatalog(true, map[string]string{"go": path, "rs": filepath.Join(dir, "rs.md")})
	if err == nil {
		t.Fatal("missing custom guide should fail")
	}
	if err := os.WriteFile(filepath.Join(dir, "rs.md"), []byte("RUST-RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = NewCatalog(true, map[string]string{"go": path, "rs": filepath.Join(dir, "rs.md")})
	if err != nil {
		t.Fatal(err)
	}
	selection, omitted := c.Select([]string{"go"}, []string{"rust"})
	if len(omitted) != 0 || !strings.Contains(selection.Text(), "MY-GO-RULES") || !strings.Contains(selection.Text(), "RUST-RULES") {
		t.Fatalf("unexpected selection: %#v %v", selection.Sources(), omitted)
	}
	if strings.Contains(selection.Text(), "gofmt") {
		t.Fatal("replacement must not concatenate the embedded guide")
	}
	if got := c.LanguageForPath("MAIN.RS"); got != "rust" {
		t.Fatalf("LanguageForPath = %q, want rust", got)
	}
	if got := c.NamedLanguages("Use the rust and rs guides"); !reflect.DeepEqual(got, []string{"rust"}) {
		t.Fatalf("Rust aliases = %v, want one canonical guide", got)
	}
	if id, enable, ok := c.GuideDirective("Use the rust guide"); !ok || !enable || id != "rust" {
		t.Fatalf("Rust directive = %q %v %v", id, enable, ok)
	}
	if got := c.LanguageForPath("thing.py"); got != "python" {
		t.Fatalf("LanguageForPath = %q, want python", got)
	}
}

func TestCanonicalCustomLanguageKeys(t *testing.T) {
	dir := t.TempDir()
	for _, key := range []string{"rs", "rust"} {
		path := filepath.Join(dir, key+".md")
		if err := os.WriteFile(path, []byte("RUST-RULES"), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := NewCatalog(true, map[string]string{key: path})
		if err != nil {
			t.Fatal(err)
		}
		for _, alias := range []string{"rs", "rust"} {
			if got := c.Resolve(alias); got != "rust" {
				t.Errorf("Resolve(%q) with key %q = %q", alias, key, got)
			}
		}
		if got := c.NamedLanguages("Use the rust guide"); !reflect.DeepEqual(got, []string{"rust"}) {
			t.Errorf("Rust name = %v", got)
		}
		if got := c.LanguageForPath("main.rs"); got != "rust" {
			t.Errorf("Rust extension = %q", got)
		}
	}
	other := filepath.Join(dir, "other.md")
	if err := os.WriteFile(other, []byte("RUST-RULES"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCatalog(true, map[string]string{"rs": other, "rust": other}); err == nil {
		t.Fatal("duplicate canonical language should fail")
	}
}

func TestSelectionExplicitPrecedesPathsAndIsBounded(t *testing.T) {
	c, err := NewCatalog(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, omitted := c.Select([]string{"python", "go", "typescript"}, []string{"javascript"})
	if got, want := s.IDs(), []string{"python", "go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(omitted, []string{"typescript"}) {
		t.Fatalf("omitted = %v", omitted)
	}
	if len(s.Text()) > MaxMessageBytes {
		t.Fatalf("message too large: %d", len(s.Text()))
	}
	s2, _ := c.Select([]string{"python"}, []string{"go", "javascript"})
	if got, want := s2.IDs(), []string{"python", "go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs = %v, want %v", got, want)
	}
}

func TestNamedLanguagesDoNotMatchGoProse(t *testing.T) {
	c, err := NewCatalog(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.NamedLanguages("please go ahead and compare syntax"); len(got) != 0 {
		t.Fatalf("false explicit matches: %v", got)
	}
	if got := c.NamedLanguages("Implement this in Python and Golang."); !reflect.DeepEqual(got, []string{"python", "go"}) {
		t.Fatalf("NamedLanguages = %v", got)
	}
}

func TestGuideDirective(t *testing.T) {
	c, err := NewCatalog(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if id, enable, ok := c.GuideDirective("Do not use the Python guide"); !ok || enable || id != "python" {
		t.Fatalf("disable directive = %q %v %v", id, enable, ok)
	}
	if id, enable, ok := c.GuideDirective("Use the Golang guide"); !ok || !enable || id != "go" {
		t.Fatalf("enable directive = %q %v %v", id, enable, ok)
	}
	if got := c.NamedLanguages("Implement this in Go"); !reflect.DeepEqual(got, []string{"go"}) {
		t.Fatalf("explicit Go request = %v", got)
	}
}

func TestCatalogValidation(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"empty.md": " \n", "large.md": strings.Repeat("x", MaxMessageBytes),
		"invalid.md": string([]byte{0xff}),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		id, file string
	}{
		{"go", "empty.md"}, {"go", "large.md"}, {"go", "invalid.md"},
		{"golang", "empty.md"}, {"c#", "empty.md"}, {"GO", "empty.md"},
	} {
		t.Run(tc.id+"/"+tc.file, func(t *testing.T) {
			if _, err := NewCatalog(true, map[string]string{tc.id: filepath.Join(dir, tc.file)}); err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
	if _, err := NewCatalog(true, map[string]string{"go": dir}); err == nil {
		t.Fatal("directory must fail")
	}
}

func TestDisabledDoesNotReadPaths(t *testing.T) {
	c, err := NewCatalog(false, map[string]string{"go": "/does/not/exist"})
	if err != nil || c == nil {
		t.Fatalf("disabled NewCatalog = %v, %v", c, err)
	}
	if got := c.LanguageForPath("main.go"); got != "" {
		t.Fatalf("disabled catalog matched %q", got)
	}
}

func TestEmbeddedGuidesFitTogether(t *testing.T) {
	c, err := NewCatalog(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for id := range c.guides {
		ids = append(ids, id)
		if s, omitted := c.Select([]string{id}, nil); len(omitted) != 0 || s.Bytes() == 0 {
			t.Fatalf("embedded guide %s does not fit", id)
		}
	}
	for _, a := range ids {
		for _, b := range ids {
			if a == b {
				continue
			}
			s, _ := c.Select([]string{a, b}, nil)
			if len(s.IDs()) != 2 || s.Bytes() > MaxMessageBytes {
				t.Fatalf("embedded pair %s/%s: %v bytes=%d", a, b, s.IDs(), s.Bytes())
			}
		}
	}
}

func TestRenderedSizeBoundaries(t *testing.T) {
	base := renderMessage([]Guide{{ID: "go", Source: "user", Content: ""}})
	content := strings.Repeat("é", (MaxMessageBytes-len(base))/2)
	if got := len(renderMessage([]Guide{{ID: "go", Source: "user", Content: content}})); got == MaxMessageBytes-1 {
		content += "x"
	} else if got > MaxMessageBytes {
		content = content[:len(content)-2] + "x"
	}
	if got := len(renderMessage([]Guide{{ID: "go", Source: "user", Content: content}})); got != MaxMessageBytes {
		t.Fatalf("test setup is not at exact boundary: %d", got)
	}
	path := filepath.Join(t.TempDir(), "go.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewCatalog(true, map[string]string{"go": path})
	if err != nil {
		t.Fatal(err)
	}
	s, omitted := c.Select([]string{"go"}, nil)
	if len(omitted) != 0 || s.Bytes() != len(renderMessage([]Guide{{ID: "go", Source: "user", Content: content}})) || s.Bytes() > MaxMessageBytes {
		t.Fatalf("boundary mismatch: %d, omitted=%v", s.Bytes(), omitted)
	}
	c.guides["python"] = Guide{ID: "python", Source: "user", Content: content}
	if s, omitted := c.Select([]string{"go", "python"}, nil); len(s.IDs()) != 1 || !reflect.DeepEqual(omitted, []string{"python"}) || s.Bytes() > MaxMessageBytes {
		t.Fatalf("combined bound failed: ids=%v omitted=%v bytes=%d", s.IDs(), omitted, s.Bytes())
	}
}
