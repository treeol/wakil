package tui

import (
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"
)

// finishSoundCmds lists the players tried to chime when a turn finishes, in
// preference order. canberra-gtk-play plays a named freedesktop event sound;
// paplay/pw-play play the sound file directly. The first one found wins.
var finishSoundCmds = [][]string{
	{"canberra-gtk-play", "-i", "complete"},
	{"paplay", "/usr/share/sounds/freedesktop/stereo/complete.oga"},
	{"pw-play", "/usr/share/sounds/freedesktop/stereo/complete.oga"},
}

// playFinishSound returns a command that plays a short completion chime via the
// first available native player, falling back to the terminal bell when none is
// installed (e.g. a bare remote shell). Failures are ignored — a missing sound
// must never disrupt the turn. Run (not Start) reaps the child so it can't
// linger as a zombie; it runs in Bubble Tea's own Cmd goroutine, so blocking
// for the ~1s chime doesn't stall the UI.
func playFinishSound() tea.Cmd {
	return func() tea.Msg {
		for _, c := range finishSoundCmds {
			if p, err := exec.LookPath(c[0]); err == nil {
				cmd := exec.Command(p, c[1:]...)
				_ = cmd.Run()
				return nil
			}
		}
		_, _ = os.Stdout.WriteString("\a")
		return nil
	}
}
