package hisn

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/treeol/wakil/internal/sandbox"
)

// ProfileMinVersion is the first hisn release with profiles (hisn-profile/v1,
// [[services]], [[forward]], [[host_services]], risk labels). Older hisn keeps
// the bare --policy path; everything that needs the 0.3 surface is then a hard
// error, never silently dropped.
const ProfileMinVersion = "0.3.1"

// homePersistFor returns the [home] persist path for a session: the root when
// the mode is persist and hisn supports [home] (>= ProfileHomeMinVersion), or
// "" (no table) for tmpfs. A tmpfs request on hisn < 0.3.2 is an error (D10):
// the old home mount cannot be removed without losing what the user asked
// for, so it never falls back silently.
func homePersistFor(mode, root string, ver [3]int) (string, error) {
	switch mode {
	case "", HomePersist:
		if root == "" {
			return "", nil
		}
		want, _ := ParseVersion(ProfileHomeMinVersion)
		if !VersionAtLeast(ver, want) {
			return "", nil // hisn < 0.3.2: the 0.4.x home mount stays (caller)
		}
		return root, nil
	case HomeTmpfs:
		want, _ := ParseVersion(ProfileHomeMinVersion)
		if !VersionAtLeast(ver, want) {
			return "", fmt.Errorf("sandbox.hisn.home = \"tmpfs\" needs hisn >= %s (found %d.%d.%d): upgrade hisn or set home to \"persist\"", ProfileHomeMinVersion, ver[0], ver[1], ver[2])
		}
		return "", nil
	default:
		return "", fmt.Errorf("sandbox.hisn.home %q: want \"persist\" or \"tmpfs\"", mode)
	}
}

// ProfileHomeMinVersion is the first hisn release whose profile accepts the
// [home] table (persist = "<abs path>"). Older hisn rejects the key outright.
const ProfileHomeMinVersion = "0.3.2"

// Home modes for sandbox.hisn.home and the project override.
const (
	HomePersist = "persist" // host dir as the session $HOME (default)
	HomeTmpfs   = "tmpfs"   // no persistent home: a fresh tmpfs $HOME
)

// ---- Surface: what a project asks for beyond the base policy ----

// Service is one in-sandbox [[services]] entry. The JSON form is the schema of
// .wakil/sandbox.json (hisn.services[]).
type Service struct {
	Name         string   `json:"name"`
	Exec         []string `json:"exec"`
	Socket       string   `json:"socket,omitempty"`
	ReadySocket  string   `json:"ready_socket,omitempty"`
	ReadyTimeout uint64   `json:"ready_timeout,omitempty"`
	StopSignal   string   `json:"stop_signal,omitempty"`
	StopGrace    uint64   `json:"stop_grace,omitempty"`
}

// Forward is one [[forward]] entry: an existing host socket exposed inside.
type Forward struct {
	Name   string `json:"name"`
	Socket string `json:"socket"`
	Expose string `json:"expose"`
	// Risk is hisn's explicit label for a socket it does not recognise.
	Risk string `json:"risk,omitempty"`
}

// HostService is one [[host_services]] entry: a host process whose socket is
// forwarded inside. It runs on the HOST as the user (label host-exec at least).
type HostService struct {
	Name         string   `json:"name"`
	Exec         []string `json:"exec"`
	Socket       string   `json:"socket"`
	Expose       string   `json:"expose"`
	ReadySocket  string   `json:"ready_socket,omitempty"`
	ReadyTimeout uint64   `json:"ready_timeout,omitempty"`
	StopSignal   string   `json:"stop_signal,omitempty"`
	StopGrace    uint64   `json:"stop_grace,omitempty"`
	Risk         string   `json:"risk,omitempty"`
}

// Surface is the per-project sandbox request. With Profile set it names a
// hisn profile (built-in or $HISN_PROFILES) used INSTEAD of generation; Wakil
// then only adds grants (MountsRO/MountsRW).
type Surface struct {
	Services     []Service
	Forward      []Forward
	HostServices []HostService
	EnvSet       map[string]string
	MountsRO     []string
	MountsRW     []string
	// Mask lists paths hidden inside the sandbox (generated profiles only).
	Mask []string
	// RiskAck is the user's acknowledgement string, copied verbatim into every
	// [[forward]]/[[host_services]] entry. Wakil never invents it.
	RiskAck string
	// Profile, when non-empty, is the name of a hisn profile to use as is.
	Profile string
	// Home is the project's home override ("persist" | "tmpfs"); "" means the
	// global sandbox.hisn.home decides (Options.Home).
	Home string
	// Rootfs names a hisn rootfs (store name) or an absolute path. Empty =
	// the host toolchain. Name form is the project-level value; path form is
	// global only (the project loader rejects it).
	Rootfs string
}

// Empty reports whether the surface asks for nothing.
func (s Surface) Empty() bool {
	return len(s.Services) == 0 && len(s.Forward) == 0 && len(s.HostServices) == 0 &&
		len(s.EnvSet) == 0 && len(s.MountsRO) == 0 && len(s.MountsRW) == 0 &&
		len(s.Mask) == 0 && s.Profile == "" && s.Home == "" && s.Rootfs == ""
}

// needs03 names the requested features that exist only in hisn >= 0.3.1.
func (s Surface) needs03() []string {
	var f []string
	if s.Profile != "" {
		f = append(f, "hisn.profile")
	}
	if len(s.Services) > 0 {
		f = append(f, "hisn.services")
	}
	if len(s.Forward) > 0 {
		f = append(f, "hisn.forward")
	}
	if len(s.HostServices) > 0 {
		f = append(f, "hisn.host_services")
	}
	if s.Rootfs != "" {
		f = append(f, "hisn.rootfs")
	}
	return f
}

var (
	profileNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	envSetNameRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	entryNameRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// Normalize validates the surface and resolves relative service paths against
// cwd (hisn does not: it stats ready sockets with the path as written). It
// returns a copy; s is left untouched.
func (s Surface) Normalize(cwd string) (Surface, error) {
	out := s
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(cwd, p)
	}
	if out.Profile != "" {
		if !profileNameRe.MatchString(out.Profile) {
			return out, fmt.Errorf("hisn.profile %q: want a profile NAME ([A-Za-z0-9._-]), not a path", out.Profile)
		}
		if len(out.Services)+len(out.Forward)+len(out.HostServices)+len(out.EnvSet) > 0 || len(out.Mask) > 0 {
			return out, fmt.Errorf("hisn.profile %q is used as is: Wakil only adds grants (hisn.mounts_ro/hisn.mounts_rw); remove hisn.services, hisn.forward, hisn.host_services and hisn.env_set", out.Profile)
		}
		// A named profile is never generated, so Wakil's [rootfs]/[home]/[ssh]
		// fragments cannot be added to it. Refuse, never drop silently.
		if out.Rootfs != "" {
			return out, fmt.Errorf("hisn.rootfs cannot be combined with hisn.profile %q: a named profile is used as is and cannot carry the rootfs; remove one of them", out.Profile)
		}
		if out.Home != "" && out.Home != HomePersist {
			return out, fmt.Errorf("sandbox.hisn.home %q cannot be combined with hisn.profile %q: a named profile is used as is; remove one of them", out.Home, out.Profile)
		}
	}
	seen := map[string]bool{}
	dup := func(kind, name string) error {
		if !entryNameRe.MatchString(name) {
			return fmt.Errorf("hisn.%s: invalid name %q (want [A-Za-z0-9._-]{1,64})", kind, name)
		}
		if seen[kind+"/"+name] {
			return fmt.Errorf("hisn.%s: duplicate name %q", kind, name)
		}
		seen[kind+"/"+name] = true
		return nil
	}
	out.Services = append([]Service(nil), s.Services...)
	for i := range out.Services {
		v := &out.Services[i]
		if err := dup("services", v.Name); err != nil {
			return out, err
		}
		if len(v.Exec) == 0 || v.Exec[0] == "" {
			return out, fmt.Errorf("hisn.services[%s]: exec must not be empty", v.Name)
		}
		if v.Socket != "" {
			v.Socket = abs(v.Socket)
		}
		if v.ReadySocket != "" {
			v.ReadySocket = abs(v.ReadySocket)
		}
	}
	out.Forward = append([]Forward(nil), s.Forward...)
	for i := range out.Forward {
		v := &out.Forward[i]
		if err := dup("forward", v.Name); err != nil {
			return out, err
		}
		if !filepath.IsAbs(v.Socket) || !filepath.IsAbs(v.Expose) {
			return out, fmt.Errorf("hisn.forward[%s]: socket and expose must be absolute paths", v.Name)
		}
	}
	out.HostServices = append([]HostService(nil), s.HostServices...)
	for i := range out.HostServices {
		v := &out.HostServices[i]
		if err := dup("host_services", v.Name); err != nil {
			return out, err
		}
		if len(v.Exec) == 0 || v.Exec[0] == "" {
			return out, fmt.Errorf("hisn.host_services[%s]: exec must not be empty", v.Name)
		}
		if !filepath.IsAbs(v.Socket) || !filepath.IsAbs(v.Expose) {
			return out, fmt.Errorf("hisn.host_services[%s]: socket and expose must be absolute paths", v.Name)
		}
		if v.ReadySocket != "" && !filepath.IsAbs(v.ReadySocket) {
			return out, fmt.Errorf("hisn.host_services[%s]: ready_socket must be an absolute path", v.Name)
		}
	}
	for k := range s.EnvSet {
		if !envSetNameRe.MatchString(k) || k == "HOME" {
			return out, fmt.Errorf("hisn.env_set: invalid or forbidden name %q", k)
		}
	}
	for _, list := range []struct {
		key string
		v   []string
	}{{"mounts_ro", s.MountsRO}, {"mounts_rw", s.MountsRW}} {
		for _, p := range list.v {
			if !filepath.IsAbs(p) || filepath.Clean(p) == "/" {
				return out, fmt.Errorf("hisn.%s: %q must be an absolute path other than /", list.key, p)
			}
		}
	}
	out.MountsRO = cleanPaths(s.MountsRO)
	out.MountsRW = cleanPaths(s.MountsRW)
	return out, nil
}

func cleanPaths(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, p := range in {
		out[i] = filepath.Clean(p)
	}
	return out
}

// ---- hisn-profile/v1 TOML ----

type profileFile struct {
	Version      string            `toml:"version"`
	Policy       policyFile        `toml:"policy"`
	Services     []serviceTOML     `toml:"services,omitempty"`
	Forward      []forwardTOML     `toml:"forward,omitempty"`
	HostServices []hostServiceTOML `toml:"host_services,omitempty"`
	Home         *homeTOML         `toml:"home,omitempty"`
	Rootfs       *rootfsTOML       `toml:"rootfs,omitempty"`
}

// rootfsTOML is hisn 0.3.2's [rootfs] table: exactly one of name (a store
// rootfs) or path (an absolute directory). Never emitted empty.
type rootfsTOML struct {
	Name string `toml:"name,omitempty"`
	Path string `toml:"path,omitempty"`
}

// rootfsFor returns the [rootfs] table for a Rootfs value, or nil when unset.
// A path value must be absolute; the caller validates the form.
func rootfsFor(r string) *rootfsTOML {
	switch {
	case r == "":
		return nil
	case filepath.IsAbs(r):
		return &rootfsTOML{Path: filepath.Clean(r)}
	default:
		return &rootfsTOML{Name: r}
	}
}

// rootfsNameRe is the store-name form accepted for a rootfs.
var rootfsNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidateRootfs checks a rootfs value: "" | a store name | an absolute path.
// allowPath is false for the project file (path form is global only).
func ValidateRootfs(r string, allowPath bool) error {
	switch {
	case r == "":
		return nil
	case filepath.IsAbs(r):
		if !allowPath {
			return errors.New("the rootfs path form is global only (sandbox.hisn.rootfs); the project file takes a store name")
		}
		return nil
	case rootfsNameRe.MatchString(r):
		return nil
	default:
		return fmt.Errorf("rootfs %q: want a store name ([A-Za-z0-9._-]{1,64}) or an absolute path", r)
	}
}

// homeTOML is hisn 0.3.2's [home] table: the host directory bind-mounted rw as
// the session $HOME (label home-persist, Data, medium).
type homeTOML struct {
	Persist string `toml:"persist"`
}

type readyTOML struct {
	Socket  string `toml:"socket,omitempty"`
	Timeout uint64 `toml:"timeout,omitempty"`
}

type stopTOML struct {
	Signal string `toml:"signal"`
	Grace  uint64 `toml:"grace"`
}

type serviceTOML struct {
	Name    string     `toml:"name"`
	Exec    []string   `toml:"exec"`
	Socket  string     `toml:"socket,omitempty"`
	Ready   *readyTOML `toml:"ready,omitempty"`
	Stop    *stopTOML  `toml:"stop,omitempty"`
	Restart string     `toml:"restart,omitempty"`
}

type forwardTOML struct {
	Name    string `toml:"name"`
	Socket  string `toml:"socket"`
	Expose  string `toml:"expose"`
	Risk    string `toml:"risk,omitempty"`
	RiskAck string `toml:"risk_ack,omitempty"`
}

type hostServiceTOML struct {
	Name    string     `toml:"name"`
	Exec    []string   `toml:"exec"`
	Socket  string     `toml:"socket"`
	Expose  string     `toml:"expose"`
	Ready   *readyTOML `toml:"ready,omitempty"`
	Stop    *stopTOML  `toml:"stop,omitempty"`
	Risk    string     `toml:"risk,omitempty"`
	RiskAck string     `toml:"risk_ack,omitempty"`
}

func readyFor(sock string, timeout uint64) *readyTOML {
	if sock == "" && timeout == 0 {
		return nil
	}
	return &readyTOML{Socket: sock, Timeout: timeout}
}

func stopFor(sig string, grace uint64) *stopTOML {
	if sig == "" && grace == 0 {
		return nil
	}
	if sig == "" {
		sig = "TERM"
	}
	if grace == 0 {
		grace = 5
	}
	return &stopTOML{Signal: sig, Grace: grace}
}

// profileFor builds the hisn-profile/v1 envelope: [policy] is the session
// policy (unchanged content), the 0.3 tables come from the surface. risk_ack
// is only ever copied from s.RiskAck.
func profileFor(pol policyFile, s Surface, homePersist string) profileFile {
	pf := profileFile{Version: "hisn-profile/v1", Policy: pol}
	if homePersist != "" {
		pf.Home = &homeTOML{Persist: homePersist}
	}
	pf.Rootfs = rootfsFor(s.Rootfs)
	for _, v := range s.Services {
		t := serviceTOML{Name: v.Name, Exec: v.Exec, Socket: v.Socket,
			Ready: readyFor(v.ReadySocket, v.ReadyTimeout), Stop: stopFor(v.StopSignal, v.StopGrace), Restart: "none"}
		if t.Ready != nil && t.Ready.Socket == "" {
			t.Ready.Socket = v.Socket
		}
		pf.Services = append(pf.Services, t)
	}
	for _, v := range s.Forward {
		pf.Forward = append(pf.Forward, forwardTOML{Name: v.Name, Socket: v.Socket, Expose: v.Expose, Risk: v.Risk, RiskAck: s.RiskAck})
	}
	for _, v := range s.HostServices {
		pf.HostServices = append(pf.HostServices, hostServiceTOML{Name: v.Name, Exec: v.Exec, Socket: v.Socket, Expose: v.Expose,
			Ready: readyFor(v.ReadySocket, v.ReadyTimeout), Stop: stopFor(v.StopSignal, v.StopGrace), Risk: v.Risk, RiskAck: s.RiskAck})
	}
	return pf
}

// buildProfile renders the profile for one session.
//
// homePersist is the [home] persist path ("" = no [home] table); see
// homePersistFor for how the caller decides it.
func buildProfile(p policyParams, s Surface, homePersist string) ([]byte, error) {
	p.EnvSet = s.EnvSet
	p.MountsRO, p.MountsRW, p.Mask = s.MountsRO, s.MountsRW, s.Mask
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(profileFor(policyFor(p), s, homePersist)); err != nil {
		return nil, fmt.Errorf("encoding hisn profile: %w", err)
	}
	return buf.Bytes(), nil
}

// profileArgs is the `hisn run --detach` command line in profile mode. The
// network comes from the profile's [policy], not from a CLI override.
func profileArgs(id, profile, cwd string) []string {
	return []string{"run", "--detach", "-v", "--session-id", id,
		"--profile", profile, "--cwd", cwd, "--ready-fd", "3"}
}

// namedArgs is the command line for a named hisn profile: Wakil adds nothing
// but initial grants.
func namedArgs(id, name, cwd string, ro, rw []string) []string {
	a := profileArgs(id, name, cwd)
	for _, p := range ro {
		a = append(a, "--grant", p+":ro")
	}
	for _, p := range rw {
		a = append(a, "--grant", p+":rw")
	}
	return a
}

// profilePath is where the generated profile of chat id lives: Wakil's state
// dir, NEVER the project (hisn rejects a profile under the session cwd, a rw
// grant or the home with E_PROFILE_UNTRUSTED: the sandbox could rewrite it).
func (b *Backend) profilePath(id string) string {
	return filepath.Join(b.opts.StateDir, id+".profile.toml")
}

// procCwd is the neutral working directory of the supervisor process (hisn
// also checks the profile against ITS process cwd).
func (b *Backend) procCwd() string { return filepath.Join(b.opts.StateDir, "proc-cwd") }

// ErrProfileUntrusted marks a generated profile hisn would refuse with
// E_PROFILE_UNTRUSTED (the state directory lies under the workspace, the home
// or an rw mount).
var ErrProfileUntrusted = errors.New("E_PROFILE_UNTRUSTED")

// checkProfileTrust reproduces hisn's E_PROFILE_UNTRUSTED rule up front, so the
// error can say what to move instead of a bare hisn code.
func checkProfileTrust(profile, cwd, home string, rw []string) error {
	// The state dir may not exist yet (first run): resolve its deepest
	// EXISTING ancestor and re-join the rest, so the comparison is on real
	// paths either way.
	pdir := resolveDeepest(filepath.Dir(profile))
	p := filepath.Join(pdir, filepath.Base(profile))
	check := func(what, root string) error {
		if root == "" {
			return nil
		}
		r, err := filepath.EvalSymlinks(root)
		if err != nil {
			r = filepath.Clean(root)
		}
		if sandbox.Within(r, p) {
			return fmt.Errorf("%w: hisn would reject the generated profile: it lies under the %s %s, where the sandbox could rewrite it; the profile lives in Wakil's state directory, so move the workspace/home outside %s", ErrProfileUntrusted, what, r, pdir)
		}
		return nil
	}
	if err := check("session cwd", cwd); err != nil {
		return err
	}
	if err := check("persistent home (sandbox.hisn.home_root)", home); err != nil {
		return err
	}
	for _, m := range rw {
		if err := check("rw mount (hisn.mounts_rw)", m); err != nil {
			return err
		}
	}
	return nil
}

// featureError names every requested 0.3 feature and the version it needs.
func featureError(features []string, got [3]int) error {
	return fmt.Errorf("%s need hisn >= %s (found %d.%d.%d): upgrade hisn or remove them from .wakil/sandbox.json",
		strings.Join(features, ", "), ProfileMinVersion, got[0], got[1], got[2])
}

// CheckStateOutside reports (as an E_PROFILE_UNTRUSTED error) whether the
// profile directory state would lie under the workspace or an explicit home:
// the check the backend repeats per session, available before any backend
// exists (default-backend resolution).
func CheckStateOutside(state, cwd, home string) error {
	return checkProfileTrust(filepath.Join(state, "x.profile.toml"), cwd, home, nil)
}

// resolveDeepest resolves symlinks in the deepest existing ancestor of p and
// re-joins the missing tail.
func resolveDeepest(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for dir := p; ; dir = filepath.Dir(dir) {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(dir) == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

// ErrStateReachable means a writable surface (a grantable prefix, an rw mount
// or an rw grant) would contain Wakil's state directory. That directory holds
// the user's approval of .wakil/sandbox.json, generated profiles and owner
// records: it is the trust root and must never be writable from the sandbox.
var ErrStateReachable = errors.New("would make Wakil's state directory writable from the sandbox")

// guardStateNotWritable refuses an rw path (or grantable prefix) that is, lies
// under, or CONTAINS the state directory (symlinks resolved on both sides).
func (b *Backend) guardStateNotWritable(what, path string) error {
	return StateGuard(b.opts.StateDir, what, path)
}

// StateGuard is guardStateNotWritable for a given state directory (also used
// by config validation, before any backend exists).
func StateGuard(stateDir, what, path string) error {
	if stateDir == "" || path == "" {
		return nil
	}
	st := resolveDeepest(stateDir)
	p := resolveDeepest(path)
	if sandbox.Within(p, st) || sandbox.Within(st, p) {
		return fmt.Errorf("%w: %s %s overlaps %s (the user's approvals live there)", ErrStateReachable, what, path, stateDir)
	}
	return nil
}
