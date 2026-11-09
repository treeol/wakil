// Package initcmd implements `wakil init`: it asks (or reads flags), tests the
// endpoint with one tool call and writes a minimal config file. The API key is
// never printed, logged or taken from a flag: it enters only through hidden TTY
// input or an environment variable.
package initcmd

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/orregistry"
	"github.com/treeol/wakil/internal/sandbox"
)

// Provider names accepted by --provider.
const (
	ProviderOpenRouter = "openrouter"
	ProviderOpenAI     = "openai-compatible"
)

const (
	defaultOpenRouterBase = "https://openrouter.ai/api"
	defaultOllamaBase     = "http://localhost:11434"
	maxListed             = 15
	freeNote              = "free: may train on prompts; blocked if your OpenRouter privacy settings forbid that; 50 requests/day without credits"
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// maxEnvNameLen: a name longer than this is treated as a pasted key, not a
// variable name (real names are short; API keys are 32+ characters).
const maxEnvNameLen = 31

// validEnvName reports whether s looks like an environment variable NAME. A
// long alphanumeric string passes the character test but is far more likely a
// key typed at the wrong prompt, so it is refused (and never echoed or saved).
func validEnvName(s string) bool { return envNameRE.MatchString(s) && len(s) <= maxEnvNameLen }

// Options are the non-interactive answers (the flags). There is deliberately
// no field that carries the key itself.
type Options struct {
	Provider string
	BaseURL  string
	Model    string
	KeyEnv   string
	Approval string
	Sandbox  string
	SkipTest bool
	Force    bool
	Yes      bool
}

// ParseArgs parses the flags of `wakil init`.
func ParseArgs(args []string, errOut io.Writer) (Options, error) {
	var o Options
	fs := flag.NewFlagSet("wakil init", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&o.Provider, "provider", "", "openrouter | openai-compatible")
	fs.StringVar(&o.BaseURL, "base-url", "", "endpoint base URL (openai-compatible; Ollama default "+defaultOllamaBase+")")
	fs.StringVar(&o.Model, "model", "", "model id")
	fs.StringVar(&o.KeyEnv, "key-env", "", "NAME of the environment variable that holds the API key (the key itself is never a flag)")
	fs.StringVar(&o.Approval, "approval", "", "ask (default) | auto")
	fs.StringVar(&o.Sandbox, "sandbox", "", "hisn | docker | direct (default: automatic)")
	fs.BoolVar(&o.SkipTest, "skip-test", false, "do not make the test call")
	fs.BoolVar(&o.Force, "force", false, "replace an existing config (a 0600 backup is made first)")
	fs.BoolVar(&o.Yes, "yes", false, "accept defaults; save even when the test call fails (with a warning)")
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: wakil init [flags]\n\nInteractive on a terminal; otherwise give --provider (and --model, --key-env ...).")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return o, nil
}

// Env is everything init touches outside itself, so tests can fake it.
type Env struct {
	In          io.Reader
	Out         io.Writer
	Interactive bool // stdin and stdout are terminals
	// ReadSecret reads one line without echo. It must return an error when echo
	// cannot be turned off; init then refuses to read the key.
	ReadSecret func(prompt string) (string, error)
	Getenv     func(string) string
	HTTP       *http.Client
	Now        func() time.Time
	ConfigPath string
	// OpenRouterBase overrides https://openrouter.ai/api (tests).
	OpenRouterBase string
	// ProbeSandbox runs the existing backend probe: reason is "" when hisn is
	// usable.
	ProbeSandbox func(ctx context.Context) (version, reason string)
	// DockerAvailable reports whether the docker CLI is installed.
	DockerAvailable func() bool
	// InContainer reports whether Wakil itself runs in a container.
	InContainer func() bool
}

func (e *Env) fill() {
	if e.Out == nil {
		e.Out = io.Discard
	}
	if e.In == nil {
		e.In = strings.NewReader("")
	}
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.HTTP == nil {
		e.HTTP = &http.Client{Timeout: 100 * time.Second}
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.OpenRouterBase == "" {
		e.OpenRouterBase = defaultOpenRouterBase
	}
	if e.ProbeSandbox == nil {
		e.ProbeSandbox = func(context.Context) (string, string) { return "", "not probed" }
	}
	if e.DockerAvailable == nil {
		e.DockerAvailable = func() bool { return false }
	}
	if e.InContainer == nil {
		e.InContainer = sandbox.InContainer
	}
	if e.ReadSecret == nil {
		e.ReadSecret = func(string) (string, error) { return "", errors.New("hidden input is not available") }
	}
}

type session struct {
	o  Options
	e  Env
	in *bufio.Reader
}

func (s *session) printf(format string, a ...any) { fmt.Fprintf(s.e.Out, format, a...) }

// line prompts and reads one line; def is returned for an empty answer.
func (s *session) line(prompt, def string) (string, error) {
	if def != "" {
		s.printf("%s [%s]: ", prompt, def)
	} else {
		s.printf("%s: ", prompt)
	}
	txt, err := s.in.ReadString('\n')
	if err != nil && txt == "" {
		return "", errors.New("input closed")
	}
	txt = strings.TrimSpace(txt)
	if txt == "" {
		return def, nil
	}
	return txt, nil
}

// choose asks for one of opts (matched by exact text or 1-based number).
func (s *session) choose(prompt string, opts []string, def string) (string, error) {
	for {
		ans, err := s.line(prompt, def)
		if err != nil {
			return "", err
		}
		for i, o := range opts {
			if strings.EqualFold(ans, o) || ans == strconv.Itoa(i+1) {
				return o, nil
			}
		}
		s.printf("  please answer one of: %s\n", strings.Join(opts, ", "))
	}
}

// Run executes init and returns the process exit code.
func Run(ctx context.Context, o Options, e Env) int {
	e.fill()
	s := &session{o: o, e: e, in: bufio.NewReader(e.In)}
	if err := s.run(ctx); err != nil {
		fmt.Fprintf(e.Out, "wakil init: %v\n", err)
		return 1
	}
	return 0
}

func (s *session) run(ctx context.Context) error {
	e := &s.e
	if e.ConfigPath == "" {
		return errors.New("cannot determine the config path (HOME is not set)")
	}
	if _, err := os.Lstat(e.ConfigPath); err == nil && !s.o.Force {
		return fmt.Errorf("%s already exists: not touching it (use --force to replace it; a backup is made first)", e.ConfigPath)
	}
	if !e.Interactive && s.o.Provider == "" {
		return errors.New("not a terminal: pass --provider openrouter|openai-compatible (see `wakil init -h`)")
	}

	var p plan
	// 1. provider and endpoint
	provider := s.o.Provider
	if provider == "" {
		s.printf("Wakil setup. Nothing is written until the last step.\n\n")
		s.printf("Provider:\n  1) openrouter         hosted models (needs an OpenRouter key)\n  2) openai-compatible  Ollama, llama.cpp, vLLM or any OpenAI-style URL\n")
		var err error
		if provider, err = s.choose("Provider", []string{ProviderOpenRouter, ProviderOpenAI}, ProviderOpenRouter); err != nil {
			return err
		}
	}
	switch provider {
	case ProviderOpenRouter:
		p.EndpointName = "openrouter"
		p.BaseURL = e.OpenRouterBase
	case ProviderOpenAI:
		p.EndpointName = "local"
		base := s.o.BaseURL
		if base == "" {
			if !e.Interactive {
				base = defaultOllamaBase
			} else {
				var err error
				if base, err = s.line("Base URL (Ollama default shown)", defaultOllamaBase); err != nil {
					return err
				}
			}
		}
		base, err := normalizeBaseURL(base)
		if err != nil {
			return err
		}
		p.BaseURL = base
	default:
		return fmt.Errorf("unknown provider %q (want %s or %s)", provider, ProviderOpenRouter, ProviderOpenAI)
	}

	// 2. credentials
	key, err := s.credentials(provider, &p)
	if err != nil {
		return err
	}
	authHeader := ""
	if key != "" {
		authHeader = "Bearer " + key
	}

	// 3-4. model and test call (repeatable)
	if err := s.modelAndTest(ctx, provider, &p, authHeader); err != nil {
		return err
	}

	// 5. approval
	if err := s.approval(&p); err != nil {
		return err
	}

	// 6. sandbox
	if err := s.sandbox(ctx, &p); err != nil {
		return err
	}

	// write
	data, err := renderConfig(p)
	if err != nil {
		return err
	}
	backup, err := writeConfig(e.ConfigPath, data, s.o.Force, e.Now(), p.KeyHeader != "")
	if err != nil {
		return err
	}
	s.printf("\n")
	if backup != "" {
		s.printf("Backed up the previous config to %s (mode 0600).\n", backup)
	}
	s.printf("Wrote %s (mode 0600).\n", e.ConfigPath)
	s.printf("  endpoint: %s -> %s, model %s\n", p.EndpointName, p.BaseURL, p.Model)
	switch {
	case p.KeyEnv != "":
		s.printf("  key: read from $%s at run time (not stored in the file)\n", p.KeyEnv)
		s.printf("  note: %s must also be set in the environment when you run wakil\n", p.KeyEnv)
	case p.KeyHeader != "":
		s.printf("  key: stored in the file as auth_header (mode 0600; `wakil config show` shows only that it is set)\n")
	default:
		s.printf("  key: none\n")
	}
	s.printf("  approval: %s\n", p.Approval)
	if p.Backend != "" {
		s.printf("  sandbox.backend: %s\n", p.Backend)
	} else {
		s.printf("  sandbox: automatic\n")
	}
	s.printf("Check it any time with `wakil config show`.\n")
	return nil
}

func normalizeBaseURL(u string) (string, error) {
	u = strings.TrimSpace(u)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf("base URL %q must start with http:// or https://", u)
	}
	u = strings.TrimRight(u, "/")
	// Wakil appends /v1/chat/completions itself.
	u = strings.TrimSuffix(u, "/v1")
	return u, nil
}

// credentials fills p's key fields and returns the key value (kept only in
// memory, for the list and test calls).
func (s *session) credentials(provider string, p *plan) (string, error) {
	e := &s.e
	if s.o.KeyEnv != "" || !e.Interactive {
		if s.o.KeyEnv == "" {
			return "", nil // local endpoints may have no key
		}
		if !validEnvName(s.o.KeyEnv) {
			return "", errors.New("--key-env takes the NAME of an environment variable, not a key")
		}
		p.KeyEnv = s.o.KeyEnv
		v := strings.TrimSpace(e.Getenv(p.KeyEnv))
		if v == "" {
			s.printf("warning: $%s is not set in this shell: the test call runs without a key\n", p.KeyEnv)
		}
		return v, nil
	}
	defMode := "paste"
	if provider == ProviderOpenAI {
		defMode = "none"
	} else if e.Getenv("OPENROUTER_API_KEY") != "" {
		defMode = "env"
	}
	s.printf("\nAPI key:\n  1) paste   type or paste it now (hidden); stored in the config file (mode 0600)\n  2) env     name an environment variable that holds it; the file stores only the name\n  3) none    the endpoint needs no key (typical for local servers)\n")
	mode, err := s.choose("Key", []string{"paste", "env", "none"}, defMode)
	if err != nil {
		return "", err
	}
	switch mode {
	case "none":
		return "", nil
	case "env":
		def := ""
		if provider == ProviderOpenRouter {
			def = "OPENROUTER_API_KEY"
		}
		for {
			name, err := s.line("Environment variable NAME", def)
			if err != nil {
				return "", err
			}
			if validEnvName(name) {
				p.KeyEnv = name
				v := strings.TrimSpace(e.Getenv(name))
				if v == "" {
					s.printf("  $%s is not set in this shell; the test call will run without a key.\n", name)
				}
				return v, nil
			}
			// Never echo what was typed: it may be the key itself.
			s.printf("  that is not a variable name (short; letters, digits, underscore). Enter the NAME, not the key.\n")
		}
	}
	key, err := e.ReadSecret("Key (input hidden): ")
	if err != nil {
		return "", fmt.Errorf("cannot read the key without echo (%w); set it in an environment variable and use --key-env NAME instead", err)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("no key entered")
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return "", errors.New("the key contains whitespace; not saved")
	}
	p.KeyHeader = "Bearer " + key
	return key, nil
}

func (s *session) modelAndTest(ctx context.Context, provider string, p *plan, authHeader string) error {
	e := &s.e
	def := ""
	if provider == ProviderOpenRouter {
		m, err := config.QuickstartModel()
		if err != nil {
			return err
		}
		def = m
	}
	var list []orregistry.Model
	listAuth := authHeader
	if provider == ProviderOpenRouter {
		listAuth = "" // the OpenRouter list is public
	}
	if s.o.Model == "" || e.Interactive {
		l, err := orregistry.ListModels(ctx, e.HTTP, p.BaseURL+"/v1/models", listAuth)
		if err != nil {
			s.printf("could not list models (%s)\n", oneLine(scrub(err.Error(), strings.TrimPrefix(authHeader, "Bearer ")), 200))
		} else {
			list = l
		}
	}
	model := s.o.Model
	for {
		if model == "" {
			if !e.Interactive {
				if def == "" {
					return errors.New("pass --model: no default model for this provider")
				}
				model = def
			} else {
				var err error
				if model, err = s.pickModel(list, def); err != nil {
					return err
				}
			}
		}
		p.Model = model
		if s.o.SkipTest {
			s.printf("Skipping the test call (--skip-test).\n")
			return nil
		}
		s.printf("\nTest call: one chat request with a trivial tool to %s ...\n", model)
		ok, msg := testCall(ctx, e.HTTP, p.BaseURL, model, authHeader)
		if ok {
			s.printf("  ok: the model called the tool.\n")
			return nil
		}
		s.printf("  FAILED: %s\n", msg)
		if !e.Interactive {
			if s.o.Yes {
				s.printf("warning: saving anyway (--yes). Fix the problem above before relying on this config.\n")
				return nil
			}
			return errors.New("test call failed; fix the problem, or use --skip-test / --yes to save anyway")
		}
		choice, err := s.choose("[p]ick another model, [s]ave anyway or [a]bort", []string{"pick", "save", "abort"}, "pick")
		if err != nil {
			return err
		}
		switch choice {
		case "save":
			return nil
		case "abort":
			return errors.New("aborted; nothing was written")
		}
		model = ""
	}
}

// orderModels puts tool-capable models first (then unknown, then known
// non-tool), keeping the server's order inside each group and the default
// model on top.
func orderModels(list []orregistry.Model, def string) []orregistry.Model {
	rank := func(m orregistry.Model) int {
		switch {
		case m.ID == def:
			return 0
		case m.Tools:
			return 1
		case !m.ToolsKnown:
			return 2
		}
		return 3
	}
	out := append([]orregistry.Model(nil), list...)
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	if len(out) > maxListed {
		out = out[:maxListed]
	}
	return out
}

func price(p orregistry.Pricing) string {
	if !p.Valid {
		return ""
	}
	f := func(v float64) string {
		if v == 0 {
			return "0"
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return fmt.Sprintf("$%s/$%s per 1M in/out", f(roundTo(p.Prompt)), f(roundTo(p.Completion)))
}

func roundTo(v float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 4, 64), 64)
	return r
}

func (s *session) pickModel(list []orregistry.Model, def string) (string, error) {
	shown := orderModels(list, def)
	hasDef := false
	for _, m := range shown {
		if m.ID == def {
			hasDef = true
		}
	}
	if len(shown) > 0 {
		s.printf("\nModels (%d shown of %d; tool-capable first):\n", len(shown), len(list))
		for i, m := range shown {
			var parts []string
			if m.ContextLength > 0 {
				parts = append(parts, fmt.Sprintf("%dk ctx", m.ContextLength/1000))
			}
			if pr := price(m.Pricing); pr != "" {
				parts = append(parts, pr)
			}
			if m.Tools {
				parts = append(parts, "tools")
			}
			s.printf("  %2d) %s  %s\n", i+1, m.ID, strings.Join(parts, ", "))
			if m.Free() {
				s.printf("      %s\n", freeNote)
			}
		}
	}
	dflt := ""
	switch {
	case def != "" && hasDef:
		dflt = "1"
	case def != "":
		dflt = def
		s.printf("  (default %s is not in the current list; type another id to change it)\n", def)
	case len(shown) > 0:
		dflt = "1"
	}
	for {
		ans, err := s.line("Model number or id", dflt)
		if err != nil {
			return "", err
		}
		if ans == "" {
			s.printf("  enter a number or a model id\n")
			continue
		}
		if n, err := strconv.Atoi(ans); err == nil {
			if n >= 1 && n <= len(shown) {
				return shown[n-1].ID, nil
			}
			s.printf("  no entry %d\n", n)
			continue
		}
		return ans, nil
	}
}

func (s *session) approval(p *plan) error {
	switch s.o.Approval {
	case "ask", "auto":
		p.Approval = s.o.Approval
		return nil
	case "":
	default:
		return fmt.Errorf("--approval takes ask or auto, not %q", s.o.Approval)
	}
	if !s.e.Interactive {
		p.Approval = "ask"
		return nil
	}
	s.printf("\nApproval mode:\n  ask   confirm each tool call (default)\n  auto  run tool calls without asking (same as --auto)\n")
	a, err := s.choose("Approval", []string{"ask", "auto"}, "ask")
	if err != nil {
		return err
	}
	p.Approval = a
	return nil
}

func (s *session) sandbox(ctx context.Context, p *plan) error {
	e := &s.e
	ver, reason := e.ProbeSandbox(ctx)
	usable := reason == ""
	automatic := "docker"
	if usable {
		automatic = "hisn"
	}
	s.printf("\nSandbox probe: ")
	if usable {
		s.printf("hisn %s is usable (automatic choice: hisn)\n", ver)
	} else {
		s.printf("hisn is not usable: %s\n", reason)
		if sandbox.IsUsernsFailure(reason) && e.InContainer() {
			s.printf("  %s\n", sandbox.ContainerUsernsHint)
		}
	}
	chosen := s.o.Sandbox
	switch chosen {
	case "", "hisn", "docker", "direct":
	default:
		return fmt.Errorf("--sandbox takes hisn, docker or direct, not %q", chosen)
	}
	if chosen == "" && !usable && e.Interactive {
		dock := e.DockerAvailable()
		s.printf("Choose how tools run:\n")
		var opts []string
		if dock {
			s.printf("  docker  tools run in a container; needs a running Docker daemon\n")
			opts = append(opts, "docker")
		} else {
			s.printf("  docker  (the docker CLI was not found)\n")
		}
		s.printf("  direct  tools run on this machine with NO sandbox; only for a machine or container you can lose\n")
		opts = append(opts, "direct", "skip")
		s.printf("  skip    decide later (wakil will say what is missing at start)\n")
		def := "skip"
		if dock {
			def = "docker"
		}
		c, err := s.choose("Sandbox", opts, def)
		if err != nil {
			return err
		}
		if c != "skip" {
			chosen = c
		}
	}
	if chosen != "" && chosen != automatic {
		p.Backend = chosen
	}
	if chosen == "hisn" && !usable {
		s.printf("warning: sandbox.backend hisn is written but the probe failed; wakil will not start until hisn works\n")
	}
	return nil
}
