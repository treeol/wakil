package tui

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Sprites: PNGs cropped and downscaled by scripts/gen_splash_logo.py, embedded
// at build time and rendered at runtime as braille truecolor art. Rendering
// at runtime lets them scale to whatever space is available (terminal zoom
// changes the pane size in cells) and keeps their alpha, so the terminal's own
// background shows through.
//
//go:embed splash/logo.png
var splashLogoPNG []byte

var (
	logoSprite = &splashSprite{data: splashLogoPNG} // start-up splash
)

// The logo has one fixed size: splashCols cells wide, splashRows() tall (40 x
// 20 for the embedded 384x392 image). It does not scale with the terminal; a
// pane too small for it gets the wordmark instead (see renderSplash).
const splashCols = 40

// splashSprite is one embedded image plus a cache of its renders.
type splashSprite struct {
	data []byte

	once sync.Once
	img  *image.RGBA // nil if decoding failed

	mu    sync.Mutex
	cache map[[2]int]string // {cols, cell rows} -> ANSI
}

func (s *splashSprite) image() *image.RGBA {
	s.once.Do(func() {
		src, err := png.Decode(bytes.NewReader(s.data))
		if err != nil {
			return
		}
		rgba := image.NewRGBA(src.Bounds())
		draw.Draw(rgba, rgba.Bounds(), src, src.Bounds().Min, draw.Src)
		s.img = rgba
	})
	return s.img
}

// brailleRows is the height in cells of the sprite drawn cols cells wide as
// braille (each cell is 2x4 dots, so a cell is about as tall as two wide).
// 0 if undecodable.
func (s *splashSprite) brailleRows(cols int) int {
	img := s.image()
	if img == nil || cols < 1 {
		return 0
	}
	b := img.Bounds()
	return max(1, int(math.Round(float64(cols*b.Dy())/float64(b.Dx())/2)))
}

// pickSplashLogo returns the logo at its fixed size when it fits a vpW x vpH
// pane, or "" when the terminal lacks truecolor or the pane is narrower or
// shorter than the logo — the caller then falls back to the wordmark.
func pickSplashLogo(vpW, vpH int, truecolor bool) string {
	if !truecolor || logoSprite.image() == nil {
		return ""
	}
	rows := logoSprite.brailleRows(splashCols)
	if vpW < splashCols || vpH < rows {
		return ""
	}
	return logoSprite.renderBraille(splashCols, rows)
}

// renderBraille returns the sprite as braille art cols x cellRows cells, each
// cell coloured with the average colour of its opaque pixels. Braille dots are
// much finer than half-blocks, which keeps thin shapes like a feather's barbs
// legible at a few rows tall. Results are cached.
func (s *splashSprite) renderBraille(cols, cellRows int) string {
	img := s.image()
	if img == nil || cols < 1 || cellRows < 1 {
		return ""
	}
	key := [2]int{cols, cellRows}
	s.mu.Lock()
	defer s.mu.Unlock()
	if out, ok := s.cache[key]; ok {
		return out
	}
	out := renderBrailleSprite(img, cols, cellRows)
	if s.cache == nil {
		s.cache = map[[2]int]string{}
	}
	s.cache[key] = out
	return out
}

// brailleBit maps a dot's (column, row) within a cell to its braille bit.
var brailleBit = [2][4]rune{{0x01, 0x02, 0x04, 0x40}, {0x08, 0x10, 0x20, 0x80}}

// brailleOnCoverage is the opaque fraction above which a dot is drawn. Below
// 50% so thin strokes survive the downscale.
const brailleOnCoverage = 0.4

func renderBrailleSprite(src *image.RGBA, cols, cellRows int) string {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dotsW, dotsH := cols*2, cellRows*4

	var out strings.Builder
	for cy := 0; cy < cellRows; cy++ {
		for cx := 0; cx < cols; cx++ {
			var mask rune
			var sr, sg, sb, sn int // colour sum over drawn dots
			for dx := 0; dx < 2; dx++ {
				for dy := 0; dy < 4; dy++ {
					X, Y := cx*2+dx, cy*4+dy
					x0, x1 := X*sw/dotsW, (X+1)*sw/dotsW
					y0, y1 := Y*sh/dotsH, (Y+1)*sh/dotsH
					var r, g, bl, a int
					for sy := y0; sy < y1; sy++ {
						off := sy*src.Stride + x0*4
						for sx := x0; sx < x1; sx++ {
							r += int(src.Pix[off])
							g += int(src.Pix[off+1])
							bl += int(src.Pix[off+2])
							a += int(src.Pix[off+3])
							off += 4
						}
					}
					n := (y1 - y0) * (x1 - x0)
					if n == 0 || a == 0 || float64(a) < brailleOnCoverage*float64(n)*255 {
						continue
					}
					mask |= brailleBit[dx][dy]
					sr += min(255, r*255/a)
					sg += min(255, g*255/a)
					sb += min(255, bl*255/a)
					sn++
				}
			}
			if mask == 0 {
				out.WriteByte(' ')
				continue
			}
			fmt.Fprintf(&out, "\x1b[38;2;%d;%d;%dm%c\x1b[0m", sr/sn, sg/sn, sb/sn, 0x2800+mask)
		}
		if cy+1 < cellRows {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

// splashTruecolor reports whether the terminal can show the sprites. A var so
// tests can force it (the test profile is not truecolor).
var splashTruecolor = func() bool {
	return lipgloss.ColorProfile() == termenv.TrueColor
}
