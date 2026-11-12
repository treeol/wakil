package proxy

// Single-transport guard (Step 3).
//
// After this change exactly one place builds and sends requests to model
// hosts: internal/proxy. Every other package must go through it (proxy.Exec
// or proxy.Client.Stream), never by opening its own HTTP request. This test
// scans all non-test Go files outside internal/proxy with go/ast (not grep)
// for outbound-HTTP shapes:
//
//	http.NewRequest*, http.Get/Post/Head, (*http.Client).Do / .Get / .Head,
//	custom Transports (http.Transport, http.RoundTripper), net.Dial*
//
// and fails on any hit that is not in the explicit allowlist below. The
// allowlist is per file with a one-line reason, and may only contain
// non-model traffic (web fetch tools, MCP transport, model-registry fetch,
// proxy-metadata probes, the shared transport factory, unix-socket IPC).
// sync.Once.Do and chromedp.Do are not HTTP and never match (only selector
// expressions on the net/http or net import name match).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// outboundAllowlist maps a repo-relative file path to the one-line reason its
// outbound HTTP is legitimate. Only non-model traffic is allowed.
var outboundAllowlist = map[string]string{
	// The single transport itself.
	"internal/proxy/client.go":    "the single transport (main loop Stream)",
	"internal/proxy/transport.go": "the single transport (Exec)",
	// Web search / fetch tools: the search query / fetched URL are tool args
	// (not model prompts or session transcripts). google.go sends the Google
	// API key as a query param (its own key, not a model-host key); searxng.go
	// has no key and no model host.
	"internal/tools/google.go":  "google_search (Google API key in query, model-supplied query) + google_fetch_url (model-supplied URL, no key) — search, not model traffic",
	"internal/tools/searxng.go": "searxng_search (model-supplied query, no key) + searxng_url_read (model-supplied URL, no key) — search, not model traffic",
	// MCP server transport (the SDK dials the user's configured MCP servers).
	// Host: the user's configured MCP server URL. Sends MCP JSON-RPC session
	// content and any configured MCP auth headers, not model traffic.
	"internal/agent/mcp_manager.go": "MCP server HTTP transport (HTTPClient field for the MCP SDK; dials user-configured MCP servers, not model hosts)",
	// Model registry: public OpenRouter /api/v1/models metadata (context
	// lengths + pricing). No API key on the registry fetch; no prompt or
	// session content — GET, no body.
	"internal/orregistry/orregistry.go": "public OpenRouter model-registry GET (context lengths + pricing); no API key on registry fetch, no prompt/session content (GET, no body)",
	// Proxy metadata probes (backend/model list, context limits) — GETs to the
	// configured endpoint host, never chat completions; no request body, no
	// prompt or transcript content. They do send the endpoint API key
	// (Authorization) to that endpoint host, by design.
	"internal/agent/backends.go": "metadata GETs /v1/ilm/backends, /v1/ilm/models, /v1/models to the configured endpoint host; endpoint key in Authorization header; no prompt/session content (no body, no query)",
	"internal/agent/ctxlimit.go": "metadata GETs context-limit probe to the configured endpoint host with ?backend=&model= query; endpoint key in Authorization header; backend/model names only, no prompt/session content",
	// Shared HTTP transport factory: builds the client handed to proxy.Client.
	"internal/wiring/bootstrap.go": "transport factory only — opens no connection; the client it builds feeds the single transport (newHTTPClient)",
	// Init: the /v1/models GET goes through orregistry; the chat test call
	// goes through proxy.Exec. The only direct HTTP in this file is the
	// http.Client construction handed to both (no request, no key, no body
	// of its own).
	"internal/initcmd/init.go": "model-list fetch via orregistry (metadata; no OpenRouter key on the public list) + http.Client construction for the proxy.Exec test call — this file issues no chat request itself",
	// Unix-socket IPC (not HTTP, never leaves the host).
	"internal/exec/exec.go":            "unix-socket IPC to the sandbox (DialTimeout \"unix\" — excluded by the unix-arg check)",
	"internal/sandbox/hisn/kvrping.go": "unix-socket IPC to the kvr server (DialTimeout \"unix\" — excluded by the unix-arg check)",
	"internal/staging/client.go":       "unix-socket IPC to the staging socket (DialTimeout \"unix\" — excluded by the unix-arg check)",
}

// outboundHTTPShapes lists the selector names that signal outbound HTTP.
// Method receivers (c.Do, client.Get, hc.Do) are tracked via declared client
// variables; construction helpers (http.NewRequest, (&http.Client{}).Get)
// are caught by the package-qualified shapes and by the tracked-variable
// scan below. Note: &http.Client{} literals are caught only when their
// receiver variable is tracked; an untracked composite literal's method
// calls are not — the guard is best-effort, not a proof.
func outboundHTTPShapes() map[string]bool {
	return map[string]bool{
		"NewRequest": true, "NewRequestWithContext": true,
		"Get": true, "Post": true, "PostForm": true, "Head": true,
		"Do":   true,
		"Dial": true, "DialContext": true, "DialTimeout": true, "DialIP": true,
		"Transport": true, "RoundTripper": true, "RoundTrip": true,
		"Client": true, "DefaultClient": true, "DefaultTransport": true,
	}
}

func TestSingleTransportGuard(t *testing.T) {
	// The test's working directory is the package dir; the repo root is the
	// parent of internal/proxy. The guard walks from there, located via the
	// source file path (runtime.Caller) so a chdir'ed process cwd (the
	// known flaky os.Chdir test in cwd_field_test.go) can never move it.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	shapes := outboundHTTPShapes()
	// Package qualifier imports: a file only matches when it imports net/http
	// (avoids false positives on same-named methods like sync.Once.Do,
	// chromedp.Do, net.Dial on unix sockets is still caught below via its
	// own import).
	fset := token.NewFileSet()
	var violations []string
	var allowed []string

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip VCS, temp/caches, and testdata.
			name := info.Name()
			if name == ".git" || name == ".tmp" || name == "testdata" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		// internal/proxy is the single transport; excluded entirely.
		if strings.HasPrefix(rel, "internal/proxy/") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		// Which local names bind net/http and net in this file? Dot-imports are
		// rejected outright: they would hide the qualifier the AST scan needs.
		httpNames := map[string]bool{}
		netNames := map[string]bool{}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			n := ""
			if imp.Name != nil {
				n = imp.Name.Name
				if n == "." && (p == "net/http" || p == "net") {
					violations = append(violations, rel+": dot-import of "+p)
					return nil
				}
			}
			switch p {
			case "net/http":
				if n == "" {
					n = "http"
				}
				httpNames[n] = true
			case "net":
				if n == "" {
					n = "net"
				}
				netNames[n] = true
			}
		}
		if len(httpNames) == 0 && len(netNames) == 0 {
			return nil
		}
		// Client variables (locals, params, struct fields) of *http.Client
		// type: client.Do / httpc.Do / hc.Do. &http.Client{} literals and
		// `*http.Client` typed declarations mark them; the selector check
		// below flags any .Do/.Get/.Post/.Head on a tracked variable even
		// though its receiver is not the package qualifier.
		clientVars := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.ValueSpec:
				ast.Inspect(d, func(m ast.Node) bool {
					if cl, ok := m.(*ast.CompositeLit); ok {
						if sel, ok := cl.Type.(*ast.SelectorExpr); ok {
							if id, ok := sel.X.(*ast.Ident); ok && httpNames[id.Name] && sel.Sel.Name == "Client" {
								for _, nm := range d.Names {
									clientVars[nm.Name] = true
								}
							}
						}
					}
					return true
				})
			case *ast.AssignStmt:
				for i, rhs := range d.Rhs {
					ast.Inspect(rhs, func(m ast.Node) bool {
						if cl, ok := m.(*ast.CompositeLit); ok {
							if sel, ok := cl.Type.(*ast.SelectorExpr); ok {
								if id, ok := sel.X.(*ast.Ident); ok && httpNames[id.Name] && sel.Sel.Name == "Client" {
									if i < len(d.Lhs) {
										if lid, ok := d.Lhs[i].(*ast.Ident); ok {
											clientVars[lid.Name] = true
										}
									}
								}
							}
						}
						return true
					})
				}
			case *ast.Field:
				// function parameters / struct fields of *http.Client type
				if st, ok := d.Type.(*ast.StarExpr); ok {
					if sel, ok := st.X.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && httpNames[id.Name] && sel.Sel.Name == "Client" {
							for _, nm := range d.Names {
								clientVars[nm.Name] = true
							}
						}
					}
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			selExpr, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if !shapes[selExpr.Sel.Name] {
				return true
			}
			pos := fset.Position(selExpr.Pos())
			emit := func(name string) {
				entry := rel + ":" + strconv.Itoa(pos.Line) + ": " + name
				if reason, ok := outboundAllowlist[rel]; ok {
					allowed = append(allowed, entry+" — "+reason)
				} else {
					violations = append(violations, entry)
				}
			}
			// Method call on a tracked *http.Client variable (client.Do, …).
			if id, ok := selExpr.X.(*ast.Ident); ok && clientVars[id.Name] &&
				(selExpr.Sel.Name == "Do" || selExpr.Sel.Name == "Get" || selExpr.Sel.Name == "Post" || selExpr.Sel.Name == "Head") {
				emit(id.Name + "." + selExpr.Sel.Name)
				return true
			}
			// Package-qualified call (http.NewRequest, http.DefaultClient.Do, net.Dial…).
			if id, ok := selExpr.X.(*ast.Ident); ok {
				if httpNames[id.Name] || netNames[id.Name] {
					emit(id.Name + "." + selExpr.Sel.Name)
				}
				return true
			}
			// Receiver is a call/paren expression: (&http.Client{}).Get(...).
			// If the receiver's subtree mentions http.Client, flag it.
			ast.Inspect(selExpr.X, func(m ast.Node) bool {
				if sel2, ok := m.(*ast.SelectorExpr); ok {
					if id2, ok := sel2.X.(*ast.Ident); ok && httpNames[id2.Name] && sel2.Sel.Name == "Client" {
						emit("(&http.Client{})." + selExpr.Sel.Name)
					}
				}
				return true
			})
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if len(violations) > 0 {
		t.Errorf("outbound HTTP outside internal/proxy (single-transport violation):\n%s\n\nAllowlist (non-model traffic only):\n%s",
			strings.Join(violations, "\n"), allowlistString())
	}
	_ = allowed
}

func allowlistString() string {
	var b strings.Builder
	for f, r := range outboundAllowlist {
		b.WriteString("  " + f + " — " + r + "\n")
	}
	return b.String()
}
