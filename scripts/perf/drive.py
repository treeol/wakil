#!/usr/bin/env python3
"""Drive wakil's TUI through a pty (used by scripts/perf/run.sh).

  drive.py [--wait SECS] STEP... -- COMMAND...

STEP is text typed into the prompt ("\\r" = Enter). The special step
"@resume" types "/resume <id>" for the OLDEST saved session of this run (taken
from $XDG_DATA_HOME/wakil/sessions). After the steps the driver quits with
ctrl+d twice and reports how long exit took. The timeline file is written by
wakil itself (WAKIL_TRACE=1)."""
import os, pty, sys, time, select, struct, fcntl, termios, glob, json

def main():
    argv = sys.argv[1:]
    wait = float(os.environ.get("STEP_WAIT", "6"))
    steps = []
    while argv and argv[0] != "--":
        steps.append(argv.pop(0))
    if argv: argv.pop(0)
    cmd = argv
    pid, fd = pty.fork()
    if pid == 0:
        os.execvp(cmd[0], cmd)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 120, 0, 0))
    buf = b""
    t0 = time.time()
    def answer(d):
        # A real terminal answers these queries within milliseconds; without an
        # answer termenv waits its full 5 s timeout before the program starts.
        if b"\x1b]11;?" in d: os.write(fd, b"\x1b]11;rgb:1e1e/1e1e/1e1e\x1b\\")
        if b"\x1b]10;?" in d: os.write(fd, b"\x1b]10;rgb:d4d4/d4d4/d4d4\x1b\\")
        if b"\x1b[6n" in d: os.write(fd, b"\x1b[1;1R")
        if b"\x1b[c" in d: os.write(fd, b"\x1b[?62;c")
    def pump(until, timeout):
        nonlocal buf
        end = time.time() + timeout
        while time.time() < end:
            r, _, _ = select.select([fd], [], [], 0.05)
            if r:
                try: d = os.read(fd, 65536)
                except OSError: return False
                if not d: return False
                buf += d
                answer(d)
            if until(buf): return True
        return False
    ok = pump(lambda b: b"/help" in b or b"\xe2\x9d\xaf" in b or b"ctrl+" in b, 60)
    print("prompt-seen: %s after %.2fs" % (ok, time.time()-t0), flush=True)
    time.sleep(0.6)
    sess_dir = os.path.join(os.environ.get("XDG_DATA_HOME", ""), "wakil", "sessions")
    for s in steps:
        if s == "@resume":
            files = sorted(glob.glob(os.path.join(sess_dir, "*.json")), key=os.path.getmtime)
            if not files:
                print("no saved session to resume", flush=True); continue
            sid = os.path.basename(files[0])[:-5]
            s = "/resume %s\r" % sid
        mark, t1 = len(buf), time.time()
        os.write(fd, s.encode().replace(b"\\r", b"\r"))
        pump(lambda b: False, wait)
        print("step %-34r waited %.1fs, +%d bytes" % (s[:34], time.time()-t1, len(buf)-mark), flush=True)
    t2 = time.time()
    os.write(fd, b"\x04"); time.sleep(0.4); os.write(fd, b"\x04")
    pump(lambda b: False, 20)
    try:
        _, st = os.waitpid(pid, 0)
        print("exited rc=%s %.2fs after the quit keys" % (os.waitstatus_to_exitcode(st), time.time()-t2), flush=True)
    except ChildProcessError:
        pass
    txt = buf.decode("utf-8", "replace").replace("\r", "\n")
    for line in txt.split("\n")[-8:]:
        if "timeline:" in line: print(line.strip()[-120:])
main()
