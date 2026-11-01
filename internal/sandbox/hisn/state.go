package hisn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// record is the owner record of one hisn session Wakil created. It lives at
// <state>/sessions/<id>.json and binds the session id to the Wakil process
// that created it AND to the specific session instance (token, carried as the
// session hostname), so a session Wakil did not create can never be adopted or
// killed.
type record struct {
	ID       string `json:"id"`
	Pid      int    `json:"pid"`
	PidStart string `json:"pid_start"` // /proc start time of Pid; "" if unknown
	Token    string `json:"token"`     // random; becomes the session hostname
	Cwd      string `json:"cwd"`
	Started  bool   `json:"started"`
}

func (b *Backend) sessionsDir() string         { return filepath.Join(b.opts.StateDir, "sessions") }
func (b *Backend) recordPath(id string) string { return filepath.Join(b.sessionsDir(), id+".json") }
func (b *Backend) lockPath() string            { return filepath.Join(b.opts.StateDir, "lock") }

// withLock runs fn holding the exclusive state lock. The lock is an flock on a
// dedicated file, so it serialises every Wakil process on this host.
func (b *Backend) withLock(fn func() error) error {
	if err := os.MkdirAll(b.sessionsDir(), 0o700); err != nil {
		return fmt.Errorf("hisn state dir: %w", err)
	}
	f, err := os.OpenFile(b.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("hisn state lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("hisn state lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// createRecord writes the record with O_EXCL: it fails if one already exists.
func (b *Backend) createRecord(r record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(b.recordPath(r.ID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("owner record exists: %w", errSessionInUse)
		}
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(b.recordPath(r.ID))
	}
	return werr
}

// rewriteRecord atomically replaces an existing record.
func (b *Backend) rewriteRecord(r record) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := b.recordPath(r.ID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.recordPath(r.ID))
}

func (b *Backend) readRecord(id string) (record, error) {
	var r record
	data, err := os.ReadFile(b.recordPath(id))
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("corrupt owner record %s: %w", id, err)
	}
	return r, nil
}

func (b *Backend) removeRecord(id string) {
	_ = os.Remove(b.recordPath(id))
	_ = os.Remove(filepath.Join(b.sessionsDir(), id+".toml"))
}

func (b *Backend) listRecordIDs() []string {
	ents, err := os.ReadDir(b.sessionsDir())
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() {
			ids = append(ids, n)
		}
	}
	return ids
}

// procStart returns the kernel start time (clock ticks since boot) of pid from
// /proc, or "" when it cannot be read (non-Linux, process gone).
func procStart(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	// Fields after the comm: state is field 3, starttime is field 22.
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

// ownerAlive reports whether the process that wrote r is still the same
// process (pid exists AND its start time is unchanged).
func ownerAlive(r record) bool {
	if r.Pid <= 0 {
		return false
	}
	if err := syscall.Kill(r.Pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if r.PidStart == "" {
		return true // cannot prove it is gone
	}
	cur := procStart(r.Pid)
	return cur == "" || cur == r.PidStart
}

var errSessionInUse = errors.New("session id already in use")
