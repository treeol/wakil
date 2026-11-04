package hisn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/treeol/wakil/internal/sandbox"
)

// KVRService names the in-sandbox staging service in .wakil/sandbox.json
// (hisn.services) and in Wakil's own default.
const KVRService = "kvr"

// KVROptions configures the in-sandbox kvr (staging) service. Zero = off.
type KVROptions struct {
	// Enabled is false when staging is switched off (kvr_disabled).
	Enabled bool
	// Binary is the kvr-server executable on the HOST (sandbox.hisn.kvr_binary
	// or the one found in PATH). The sandbox sees it at the same path, so it
	// must live under a directory hisn exposes (/usr, /opt, /nix, ...).
	Binary                          string
	MaxEntries                      int
	SweepIntervalSecs, SnapshotSecs int
}

// kvrDirName is the directory (under the workspace) holding the socket and
// the snapshot. It is the ONLY place the host client may dial.
const kvrDirName = ".wakil"

// visibleRoots are the host trees every hisn session binds read-only (DESIGN
// §4.1: the toolchain dirs, when they exist) — a binary has to live under one
// of them (or under a project mounts_ro) to be runnable inside.
var visibleRoots = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/opt", "/nix"}

// binaryVisible reports whether the sandbox can run bin: its canonical
// (symlink-resolved) path lies under a toolchain root or a project ro/rw mount.
func binaryVisible(bin string, extra []string) (string, bool) {
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return bin, false
	}
	for _, r := range append(append([]string(nil), visibleRoots...), extra...) {
		if sandbox.Within(r, real) {
			return real, true
		}
	}
	return real, false
}

// kvrPlan is what Wakil decided about the staging service.
type kvrPlan struct {
	Socket  string  // host == sandbox path of the socket ("" = staging off)
	Service Service // the service to add (zero when the project declared its own)
	Add     bool    // add Service to the surface
	Note    string  // one line for the banner / /sandbox
}

// planKVR decides how staging runs. There is NO fallback to a host-side kvr:
// without a runnable in-sandbox binary, staging stays off with the reason.
func planKVR(cwd string, s Surface, o KVROptions, profileMode bool, extraVisible []string) (kvrPlan, error) {
	off := func(why string) (kvrPlan, error) { return kvrPlan{Note: "off (" + why + ")"}, nil }
	if !o.Enabled {
		return off("kvr_disabled")
	}
	if !profileMode {
		return off("needs hisn >= " + ProfileMinVersion + " (in-sandbox services)")
	}
	if s.Profile != "" {
		return off("a named hisn profile is used as is; declare a kvr service in it and Wakil cannot reach its socket")
	}
	dir := filepath.Join(cwd, kvrDirName)
	// The project may declare its own kvr service.
	for _, v := range s.Services {
		if v.Name != KVRService {
			continue
		}
		if v.Socket == "" {
			return kvrPlan{}, errors.New(`hisn.services[kvr]: "socket" is required (the staging client dials it)`)
		}
		sock := filepath.Clean(v.Socket)
		if filepath.Dir(sock) != dir {
			return kvrPlan{}, fmt.Errorf("hisn.services[kvr]: socket %s must be directly under %s (the host client only dials there)", sock, dir)
		}
		if v.ReadySocket != "" && filepath.Clean(v.ReadySocket) != sock {
			return kvrPlan{}, fmt.Errorf("hisn.services[kvr]: ready_socket must equal socket (%s)", sock)
		}
		return kvrPlan{Socket: sock, Note: "on (project-declared service, socket " + sock + ")"}, nil
	}
	if o.Binary == "" {
		return off("no kvr-server binary: set sandbox.hisn.kvr_binary or put kvr-server in PATH")
	}
	real, ok := binaryVisible(o.Binary, extraVisible)
	if !ok {
		return off(fmt.Sprintf("kvr binary %s is not visible inside the sandbox: install it under /usr or /opt, or list its directory in hisn.mounts_ro", real))
	}
	sock := filepath.Join(dir, "kvr.sock")
	args := []string{"/usr/bin/env",
		"KVR_SOCKET_PATH=" + sock,
		"KVR_SNAPSHOT_PATH=" + filepath.Join(dir, "staging.kvr"),
		"KVR_SNAPSHOT_ON_SHUTDOWN=true",
		"KVR_MAX_ENTRIES=" + strconv.Itoa(o.MaxEntries),
		"KVR_SWEEP_INTERVAL_SECS=" + strconv.Itoa(o.SweepIntervalSecs),
		"KVR_SNAPSHOT_INTERVAL_SECS=" + strconv.Itoa(o.SnapshotSecs),
		real,
	}
	// No `ready`: hisn fails the WHOLE session closed on a readiness timeout, a
	// broken kvr must not be able to do that. Wakil PINGs the socket itself.
	return kvrPlan{
		Socket:  sock,
		Service: Service{Name: KVRService, Exec: args, Socket: sock, StopSignal: "TERM", StopGrace: 5},
		Add:     true,
		Note:    "on (in-sandbox service, socket " + sock + ")",
	}, nil
}

// ---- host-side socket safety (Mashūra R1 B2 / N8 / N10) ----

// GuardKVRSocket refuses a socket path the host client must not dial: the
// directory (.wakil) and the socket may not be symlinks (a link to docker.sock
// or an ssh-agent would make the host speak to it), the socket must be a unix
// socket owned by the current user, and it must sit directly in <cwd>/.wakil.
func GuardKVRSocket(cwd, sock string) error {
	dir := filepath.Join(cwd, kvrDirName)
	if filepath.Dir(filepath.Clean(sock)) != dir {
		return fmt.Errorf("kvr socket %s is not directly under %s", sock, dir)
	}
	di, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("kvr dir: %w", err)
	}
	if !di.IsDir() {
		return fmt.Errorf("%s is not a plain directory (symlink?)", dir)
	}
	si, err := os.Lstat(sock)
	if err != nil {
		return fmt.Errorf("kvr socket: %w", err)
	}
	if si.Mode()&os.ModeSymlink != 0 || si.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a plain unix socket (symlink or other file type)", sock)
	}
	return checkOwner(si)
}

// PrepareKVRDir creates <cwd>/.wakil (plain directory, never through a
// symlink) and removes a stale socket file left by a previous session — hisn
// does not (WAKIL-INTEGRATION §5). A stale path that is not a socket is
// refused, never unlinked.
func PrepareKVRDir(cwd, sock string) error {
	dir := filepath.Join(cwd, kvrDirName)
	if fi, err := os.Lstat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a plain directory", dir)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	} else {
		return err
	}
	fi, err := os.Lstat(sock)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.Mode()&os.ModeSymlink != 0 || fi.Mode()&os.ModeSocket == 0:
		return fmt.Errorf("%s exists and is not a unix socket; not removing it", sock)
	}
	if err := os.Remove(sock); err != nil {
		return fmt.Errorf("removing stale kvr socket: %w", err)
	}
	return nil
}

// planKVR decides, once, whether the staging service runs and adds it to the
// surface so the risk check sees exactly the profile that will start.
func (b *Backend) planKVR() error {
	p, err := planKVR(b.opts.HostCwd, b.opts.Surface, b.opts.KVR, b.profileMode(), b.opts.Surface.MountsRO)
	if err != nil {
		return err
	}
	b.kvrNote, b.kvrSocket = p.Note, p.Socket
	if p.Add {
		b.opts.Surface.Services = append(append([]Service(nil), b.opts.Surface.Services...), p.Service)
	}
	return nil
}

// KVRSocket returns the host path of the staging socket once the session is
// running and the service answered a PING, else "". The path is the same
// inside and outside the sandbox (cwd is bound at its identical path).
func (b *Backend) KVRSocket() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.cur == nil || b.cur.kvr == "" {
		return ""
	}
	return b.cur.kvr
}

// awaitKVR runs after the session is ready (the socket directory was prepared
// BEFORE the service bound, see prepareLaunch): it waits for the service to
// answer a PING. A broken kvr never fails the session: staging is
// switched off for this session with a visible note.
func (b *Backend) awaitKVR(l *live) {
	if b.kvrSocket == "" {
		return
	}
	if err := GuardKVRSocket(b.opts.HostCwd, b.kvrSocket); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The service never bound its socket (broken binary, crashed).
			b.setKVRNote("off (service did not answer: socket never appeared)")
		} else {
			b.setKVRNote("off (" + err.Error() + ")")
		}
		return
	}
	if err := PingKVR(b.kvrSocket, kvrWait); err != nil {
		b.setKVRNote("off (service did not answer: " + err.Error() + ")")
		return
	}
	l.kvr = b.kvrSocket
}

func (b *Backend) setKVRNote(n string) {
	fmt.Fprintf(errWriter, "hisn: staging (kvr) %s\n", n)
	b.kvrNote = n
}

var _ sandbox.KVRProvider = (*Backend)(nil)
