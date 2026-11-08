package hisn

import (
	"fmt"
	"io/fs"
	"syscall"
	"testing"
)

// statLine builds a /proc/<pid>/stat line with the given state letter and
// start time (field 22), padded with zero fields in between.
func statLine(pid int, state byte, start string) []byte {
	// Fields after "(comm)": state(3) ppid..(4-21) starttime(22).
	s := fmt.Sprintf("%d (hisn-fake) %c", pid, state)
	for i := 4; i <= 21; i++ {
		s += " 0"
	}
	return []byte(s + " " + start + " 0 0\n")
}

func pathErr(path string, errno syscall.Errno) error {
	return &fs.PathError{Op: "open", Path: path, Err: errno}
}

// fakeProc scripts the two kernel reads procAlive makes. kills holds the result
// of each successive kill(pid, 0) (the last one repeats); files maps a path to
// its content or error.
type fakeProc struct {
	kills []error
	files map[string]any // []byte or error
	calls int
}

func (f *fakeProc) ops() procOps {
	return procOps{
		signal0: func(int) error {
			i := min(f.calls, len(f.kills)-1)
			f.calls++
			return f.kills[i]
		},
		readFile: func(path string) ([]byte, error) {
			switch v := f.files[path].(type) {
			case []byte:
				return v, nil
			case error:
				return nil, v
			}
			return nil, pathErr(path, syscall.ENOENT)
		},
	}
}

// The reap window: kill(0) says the pid exists, then init reaps the zombie
// before /proc/<pid>/stat is read. The read then fails with ENOENT (the entry
// is gone at open) or ESRCH (the open succeeded on a task reaped before the
// read; seen on a real kernel). With a working procfs either, plus a second
// kill(0) answering ESRCH, proves the process is gone.
func TestProcAliveReapedBetweenKillAndStat(t *testing.T) {
	const pid = 4242
	for _, errno := range []syscall.Errno{syscall.ENOENT, syscall.ESRCH} {
		for _, start := range []string{"777", ""} {
			t.Run(fmt.Sprintf("%v/start=%q", errno, start), func(t *testing.T) {
				f := &fakeProc{
					kills: []error{nil, syscall.ESRCH},
					files: map[string]any{
						"/proc/self/stat": statLine(1, 'S', "1"),
						"/proc/4242/stat": pathErr("/proc/4242/stat", errno),
					},
				}
				if procOps.alive(f.ops(), pid, start) {
					t.Fatal("a pid reaped between kill(0) and the stat read must be reported gone")
				}
				if f.calls != 2 {
					t.Fatalf("kill(0) called %d times, want 2 (the second one is the proof)", f.calls)
				}
			})
		}
	}
}

// Cases that must keep the old "cannot prove it is gone" answer.
func TestProcAliveUnknowableStaysAlive(t *testing.T) {
	const pid = 4242
	self := statLine(1, 'S', "1")
	tests := []struct {
		name  string
		kills []error
		files map[string]any
	}{
		{
			name:  "stat unreadable for a reason other than ENOENT",
			kills: []error{nil},
			files: map[string]any{
				"/proc/self/stat": self,
				"/proc/4242/stat": pathErr("/proc/4242/stat", syscall.EACCES),
			},
		},
		{
			name:  "no procfs at all",
			kills: []error{nil, syscall.ESRCH},
			files: map[string]any{
				"/proc/self/stat": pathErr("/proc/self/stat", syscall.ENOENT),
				"/proc/4242/stat": pathErr("/proc/4242/stat", syscall.ENOENT),
			},
		},
		{
			name:  "entry missing but the pid still answers kill(0)",
			kills: []error{nil, nil},
			files: map[string]any{
				"/proc/self/stat": self,
				"/proc/4242/stat": pathErr("/proc/4242/stat", syscall.ENOENT),
			},
		},
		{
			name:  "read ESRCH but the pid still answers kill(0)",
			kills: []error{nil, nil},
			files: map[string]any{
				"/proc/self/stat": self,
				"/proc/4242/stat": pathErr("/proc/4242/stat", syscall.ESRCH),
			},
		},
		{
			name:  "entry missing and the second kill(0) is EPERM, not ESRCH",
			kills: []error{nil, syscall.EPERM},
			files: map[string]any{
				"/proc/self/stat": self,
				"/proc/4242/stat": pathErr("/proc/4242/stat", syscall.ENOENT),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeProc{kills: tc.kills, files: tc.files}
			if !procOps.alive(f.ops(), pid, "777") {
				t.Fatal("an unprovable gone must read as alive (old behavior)")
			}
		})
	}
}

// The rest of the contract is unchanged: a readable zombie is gone, a start
// time mismatch is gone, a match is alive, and kill(0) ESRCH is gone.
func TestProcAliveReadableCases(t *testing.T) {
	const pid = 4242
	path := "/proc/4242/stat"
	tests := []struct {
		name  string
		kills []error
		stat  []byte
		start string
		want  bool
	}{
		{"zombie", []error{nil}, statLine(pid, 'Z', "777"), "777", false},
		{"dead (X)", []error{nil}, statLine(pid, 'X', "777"), "777", false},
		{"zombie, no start recorded", []error{nil}, statLine(pid, 'Z', "777"), "", false},
		{"running, same start", []error{nil}, statLine(pid, 'S', "777"), "777", true},
		{"running, no start recorded", []error{nil}, statLine(pid, 'S', "777"), "", true},
		{"running, pid reused (other start)", []error{nil}, statLine(pid, 'S', "888"), "777", false},
		{"kill(0) ESRCH", []error{syscall.ESRCH}, statLine(pid, 'S', "777"), "777", false},
		{"kill(0) EPERM, running, same start", []error{syscall.EPERM}, statLine(pid, 'S', "777"), "777", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeProc{kills: tc.kills, files: map[string]any{
				"/proc/self/stat": statLine(1, 'S', "1"),
				path:              tc.stat,
			}}
			if got := procOps.alive(f.ops(), pid, tc.start); got != tc.want {
				t.Fatalf("alive = %v, want %v", got, tc.want)
			}
		})
	}
	if procAlive(0, "") || procAlive(-1, "") {
		t.Fatal("pid <= 0 is never alive")
	}
}
