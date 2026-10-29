package agent

import (
	"strings"
	"testing"
)

func mustSection(t *testing.T, tmpl *skillTemplate) string {
	t.Helper()
	s, err := serializeTemplateSection(tmpl)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return s
}

func TestSkillTemplateRoundTrip(t *testing.T) {
	cases := map[string]string{
		"trailing newline": "#!/bin/sh\necho @@NAME@@\n",
		"no trailing eol":  "line1\nline2",
		"empty":            "",
		"single newline":   "\n",
		"crlf body":        "a\r\nb\r\n",
		"go templates":     "docker ps --format '{{.ID}}' {{json .Mounts}}\n",
		"marker lookalike": "=== file: other ===\n=== end: other ===\n=== wakil-template v1 ===\n",
		"blank lines":      "\n\nx\n\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			in := &skillTemplate{
				Params: []templateParam{{Name: "NAME", Required: true}},
				Files:  []templateFile{{Path: "f.txt", Body: body}, {Path: "second", Body: "x\n"}},
			}
			value, err := joinSkillTemplate("# prose\nsome text", in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseSkillTemplateValue(value)
			if err != nil {
				t.Fatalf("parse: %v\nvalue:\n%s", err, value)
			}
			if got.Files[0].Body != body {
				t.Errorf("body = %q, want %q", got.Files[0].Body, body)
			}
			if skillProse(value) != "# prose\nsome text" {
				t.Errorf("prose = %q", skillProse(value))
			}
			// Re-serialization is byte-stable.
			if again := mustSection(t, got); again != mustSection(t, in) {
				t.Errorf("section not stable:\n%s\nvs\n%s", again, mustSection(t, in))
			}
		})
	}
}

func TestSkillTemplateNoSection(t *testing.T) {
	tmpl, err := parseSkillTemplateValue("just prose\n=== not the marker ===\n")
	if err != nil || tmpl != nil {
		t.Fatalf("plain skill: tmpl=%v err=%v", tmpl, err)
	}
}

func TestSkillTemplateParseErrors(t *testing.T) {
	m := tmplSectionMarker + "\n"
	cases := map[string]string{
		"bad json":         m + "{not json}\n",
		"unknown field":    m + `{"files":[{"path":"a"}],"bogus":1}` + "\n=== file: a ===\nx\n=== end: a ===\n",
		"secret field":     m + `{"params":[{"name":"P","secret":true}],"files":[{"path":"a"}]}` + "\n=== file: a ===\n@@P@@\n=== end: a ===\n",
		"missing end":      m + `{"files":[{"path":"a"}]}` + "\n=== file: a ===\nx\n",
		"body count":       m + `{"files":[{"path":"a"},{"path":"b"}]}` + "\n=== file: a ===\nx\n=== end: a ===\n",
		"undeclared token": m + `{"files":[{"path":"a"}]}` + "\n=== file: a ===\n@@NOPE@@\n=== end: a ===\n",
		"traversal":        m + `{"files":[{"path":"../a"}]}` + "\n=== file: ../a ===\nx\n=== end: ../a ===\n",
		"absolute":         m + `{"files":[{"path":"/etc/a"}]}` + "\n=== file: /etc/a ===\nx\n=== end: /etc/a ===\n",
		"reserved dir":     m + `{"files":[{"path":".wakil/plan.md"}]}` + "\n=== file: .wakil/plan.md ===\nx\n=== end: .wakil/plan.md ===\n",
		"bad mode":         m + `{"files":[{"path":"a","mode":"0000"}]}` + "\n=== file: a ===\nx\n=== end: a ===\n",
		"required+default": m + `{"params":[{"name":"P","required":true,"default":"x"}],"files":[{"path":"a"}]}` + "\n=== file: a ===\n@@P@@\n=== end: a ===\n",
		"garbage between":  m + `{"files":[{"path":"a"}]}` + "\n=== file: a ===\nx\n=== end: a ===\nstray\n",
		"no files":         m + `{"files":[]}` + "\n",
		"dup param":        m + `{"params":[{"name":"P"},{"name":"P"}],"files":[{"path":"a"}]}` + "\n=== file: a ===\nx\n=== end: a ===\n",
		"lowercase param":  m + `{"params":[{"name":"p"}],"files":[{"path":"a"}]}` + "\n=== file: a ===\nx\n=== end: a ===\n",
	}
	for name, section := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSkillTemplateValue("prose\n\n" + section); err == nil {
				t.Errorf("expected parse error for %s", name)
			}
		})
	}
}

func TestSkillTemplateSerializeRejectsOwnEndMarker(t *testing.T) {
	_, err := serializeTemplateSection(&skillTemplate{Files: []templateFile{{Path: "a", Body: "x\n=== end: a ===\n"}}})
	if err == nil {
		t.Fatal("body containing its own end marker must be rejected")
	}
}

func TestResolveAndRender(t *testing.T) {
	tmpl := &skillTemplate{
		Params: []templateParam{
			{Name: "STACK", Required: true, Pattern: "[a-z0-9-]+"},
			{Name: "PORT", Default: "8080"},
			{Name: "OPT"},
		},
		Files: []templateFile{
			{Path: "@@STACK@@/deploy.sh", Mode: "0755", Body: "stack=@@STACK@@ port=@@PORT@@ opt=[@@OPT@@] keep={{.ID}} @@STACK@@@@PORT@@\n"},
		},
	}
	if err := tmpl.validate(); err != nil {
		t.Fatal(err)
	}

	t.Run("missing required", func(t *testing.T) {
		_, err := resolveTemplateParams(tmpl, nil)
		if err == nil || !strings.Contains(err.Error(), "STACK") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown param", func(t *testing.T) {
		_, err := resolveTemplateParams(tmpl, map[string]string{"STACK": "a", "X": "1"})
		if err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("pattern is full match", func(t *testing.T) {
		if _, err := resolveTemplateParams(tmpl, map[string]string{"STACK": "ok-1; rm -rf /"}); err == nil {
			t.Fatal("partial pattern match must be rejected")
		}
	})
	t.Run("newline rejected", func(t *testing.T) {
		if _, err := resolveTemplateParams(tmpl, map[string]string{"STACK": "a", "OPT": "x\ny"}); err == nil {
			t.Fatal("multi-line value must be rejected")
		}
	})
	t.Run("render single pass", func(t *testing.T) {
		values, err := resolveTemplateParams(tmpl, map[string]string{"STACK": "shop", "OPT": "@@PORT@@"})
		if err != nil {
			t.Fatal(err)
		}
		files, err := renderTemplate(tmpl, values)
		if err != nil {
			t.Fatal(err)
		}
		want := "stack=shop port=8080 opt=[@@PORT@@] keep={{.ID}} shop8080\n"
		if files[0].Path != "shop/deploy.sh" || files[0].Content != want || files[0].Mode != "0755" {
			t.Errorf("rendered = %+v, want content %q", files[0], want)
		}
	})
	t.Run("path value traversal rejected", func(t *testing.T) {
		loose := &skillTemplate{Params: []templateParam{{Name: "DIR", Required: true}}, Files: []templateFile{{Path: "@@DIR@@/x", Body: ""}}}
		values, _ := resolveTemplateParams(loose, map[string]string{"DIR": "../../etc"})
		if _, err := renderTemplate(loose, values); err == nil {
			t.Fatal("rendered path with .. must be rejected")
		}
		values, _ = resolveTemplateParams(loose, map[string]string{"DIR": ".wakil"})
		if _, err := renderTemplate(loose, values); err == nil {
			t.Fatal("rendered path into .wakil/ must be rejected")
		}
	})
	t.Run("file vs dir conflict", func(t *testing.T) {
		c := &skillTemplate{Files: []templateFile{{Path: "a", Body: ""}, {Path: "a/b", Body: ""}}}
		if _, err := renderTemplate(c, map[string]string{}); err == nil {
			t.Fatal("a and a/b must conflict")
		}
	})
}

func TestParameterizeSinglePass(t *testing.T) {
	params := []captureParam{{Name: "STACK_NAME", Value: "shop-prod"}, {Name: "SHORT", Value: "shop"}, {Name: "STACK", Value: "STACK"}}
	counts := map[string]int{}
	got := parameterize("shop-prod shop STACK shop-prodx", params, counts)
	want := "@@STACK_NAME@@ @@SHORT@@ @@STACK@@ @@STACK_NAME@@x"
	if got != want {
		t.Errorf("parameterize = %q, want %q", got, want)
	}
	if counts["STACK_NAME"] != 2 || counts["SHORT"] != 1 || counts["STACK"] != 1 {
		t.Errorf("counts = %v", counts)
	}
}

func TestSummarizeSkillTemplate(t *testing.T) {
	tmpl := &skillTemplate{
		Params: []templateParam{{Name: "A", Required: true, Description: "the a"}, {Name: "B", Default: "x"}},
		Files:  []templateFile{{Path: "deploy.sh", Mode: "0755", Body: "1\n2\n"}},
		Verify: []string{"bash -n deploy.sh"},
	}
	s := summarizeSkillTemplate(tmpl)
	for _, want := range []string{"A (required) — the a", `B (default "x")`, "deploy.sh (0755, 4 bytes, 2 lines)", "bash -n deploy.sh", "apply_skill_template"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "1\n2") {
		t.Error("summary must not include bodies")
	}
}

func TestSkillTemplateParseRejectsNonCanonical(t *testing.T) {
	m := tmplSectionMarker + "\n"
	cases := map[string]string{
		"trailing bracket":                m + `{"files":[{"path":"a"}]}]` + "\n=== file: a ===\nx\n=== end: a ===\n",
		"trailing object":                 m + `{"files":[{"path":"a"}]} {}` + "\n=== file: a ===\nx\n=== end: a ===\n",
		"empty without no_eol":            m + `{"files":[{"path":"a"}]}` + "\n=== file: a ===\n=== end: a ===\n",
		"no_eol with trailing empty line": m + `{"files":[{"path":"a","no_eol":true}]}` + "\n=== file: a ===\nx\n\n=== end: a ===\n",
		"tab in path":                     m + `{"files":[{"path":"a\tb"}]}` + "\n=== file: a\tb ===\nx\n=== end: a\tb ===\n",
		"esc in path":                     m + `{"files":[{"path":"a\u001bb"}]}` + "\n=== file: a\x1bb ===\nx\n=== end: a\x1bb ===\n",
	}
	for name, section := range cases {
		if _, err := parseSkillTemplateValue("prose\n\n" + section); err == nil {
			t.Errorf("%s: expected parse error", name)
		}
	}
}
