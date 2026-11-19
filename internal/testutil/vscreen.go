package testutil

import (
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// VScreen is a minimal terminal emulator for the byte stream Bubble Tea's
// standard renderer writes in the alternate screen: cursor addressing, \r \n
// (scrolling at the bottom), erase-line, erase-below, erase-screen and
// printable text. SGR and mode sequences are parsed and ignored. Every call to
// Write is one renderer flush; after it the grid is stored as a frame, so a
// test can look at each screen the user would have seen, not only the model's
// own View() strings.
type VScreen struct {
	mu     sync.Mutex
	w, h   int
	grid   [][]rune
	cx, cy int
	frames []string
	carry  []byte
}

func NewVScreen(w, h int) *VScreen {
	v := &VScreen{w: w, h: h}
	v.clearAll()
	return v
}

func (v *VScreen) clearAll() {
	v.grid = make([][]rune, v.h)
	for i := range v.grid {
		v.grid[i] = []rune(strings.Repeat(" ", v.w))
	}
}

// resize changes the terminal size the way a real terminal does: the grid is
// cropped or padded, content is not reflowed.
func (v *VScreen) Resize(w, h int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	g := make([][]rune, h)
	for y := range g {
		g[y] = []rune(strings.Repeat(" ", w))
		if y < len(v.grid) {
			copy(g[y], v.grid[y])
		}
	}
	v.w, v.h, v.grid = w, h, g
	v.cx, v.cy = min(v.cx, w-1), min(v.cy, h-1)
}

func (v *VScreen) Write(p []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	b := append(v.carry, p...)
	v.carry = nil
	i := 0
	for i < len(b) {
		c := b[i]
		switch {
		case c == 0x1b:
			if i+1 >= len(b) {
				v.carry = append([]byte(nil), b[i:]...)
				i = len(b)
				continue
			}
			switch b[i+1] {
			case '[':
				j := i + 2
				for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
					j++
				}
				if j >= len(b) {
					v.carry = append([]byte(nil), b[i:]...)
					i = len(b)
					continue
				}
				v.csi(string(b[i+2:j]), b[j])
				i = j + 1
			case ']':
				j := i + 2
				for j < len(b) && b[j] != 0x07 && !(b[j] == 0x1b && j+1 < len(b) && b[j+1] == '\\') {
					j++
				}
				if j >= len(b) {
					v.carry = append([]byte(nil), b[i:]...)
					i = len(b)
					continue
				}
				if b[j] == 0x07 {
					i = j + 1
				} else {
					i = j + 2
				}
			default:
				i += 2
			}
		case c == '\r':
			v.cx = 0
			i++
		case c == '\n':
			v.lineFeed()
			i++
		case c < 0x20:
			i++
		default:
			// One UTF-8 rune.
			r, n := decodeRune(b[i:])
			if n == 0 { // incomplete
				v.carry = append([]byte(nil), b[i:]...)
				i = len(b)
				continue
			}
			i += n
			v.put(r)
		}
	}
	v.frames = append(v.frames, v.snapshot())
	return len(p), nil
}

func decodeRune(b []byte) (rune, int) {
	if !utf8.FullRune(b) {
		return 0, 0
	}
	return utf8.DecodeRune(b)
}

func (v *VScreen) put(r rune) {
	if v.cx >= v.w {
		v.cx = 0
		v.lineFeed()
	}
	v.grid[v.cy][v.cx] = r
	v.cx++
}

func (v *VScreen) lineFeed() {
	if v.cy+1 >= v.h {
		copy(v.grid, v.grid[1:])
		v.grid[v.h-1] = []rune(strings.Repeat(" ", v.w))
		return
	}
	v.cy++
}

func (v *VScreen) csi(params string, final byte) {
	if strings.HasPrefix(params, "?") || strings.HasPrefix(params, ">") {
		return // modes
	}
	nums := func(def int) []int {
		var out []int
		for _, s := range strings.Split(params, ";") {
			n, err := strconv.Atoi(s)
			if err != nil {
				n = def
			}
			out = append(out, n)
		}
		return out
	}
	switch final {
	case 'H', 'f':
		n := nums(1)
		row, col := 1, 1
		if len(n) > 0 && n[0] > 0 {
			row = n[0]
		}
		if len(n) > 1 && n[1] > 0 {
			col = n[1]
		}
		v.cy = min(row-1, v.h-1)
		v.cx = min(col-1, v.w-1)
	case 'K':
		for x := v.cx; x < v.w; x++ {
			v.grid[v.cy][x] = ' '
		}
	case 'J':
		switch params {
		case "2", "3":
			v.clearAll()
		default:
			for x := v.cx; x < v.w; x++ {
				v.grid[v.cy][x] = ' '
			}
			for y := v.cy + 1; y < v.h; y++ {
				v.grid[y] = []rune(strings.Repeat(" ", v.w))
			}
		}
	case 'A':
		v.cy = max(0, v.cy-nums(1)[0])
	case 'B':
		v.cy = min(v.h-1, v.cy+nums(1)[0])
	case 'G':
		v.cx = min(v.w-1, max(0, nums(1)[0]-1))
	}
}

func (v *VScreen) snapshot() string {
	rows := make([]string, v.h)
	for y := range v.grid {
		rows[y] = strings.TrimRight(string(v.grid[y]), " ")
	}
	return strings.Join(rows, "\n")
}

func (v *VScreen) Screen() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.snapshot()
}

func (v *VScreen) AllFrames() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.frames...)
}

func (v *VScreen) NFrames() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.frames)
}
