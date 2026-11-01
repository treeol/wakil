#!/usr/bin/env python3
"""Render the TUI splash logo (braille truecolor ANSI) to assets/wakil-braille.png.

GitHub cannot show ANSI colour codes, so the README uses a PNG of the exact
output the TUI draws: each braille dot becomes a circle, on a dark rounded panel
(transparent outside the corners) so it stays legible in light and dark themes.
Dev-time only (needs Pillow and Go).

    python3 scripts/gen_readme_logo.py
"""
import os
import re
import subprocess
import tempfile

from PIL import Image, ImageDraw

OUT = "assets/wakil-braille.png"
PITCH = 6      # px between dot centres (final image)
RADIUS = 2.3   # dot radius in px (final image)
SS = 4         # supersampling for smooth circles
PAD_X = 4      # cells of horizontal padding (2 dots = 12px each)
PAD_Y = 2      # cells of vertical padding (4 dots = 24px each)
PANEL = (20, 20, 24, 255)   # dark backing so the logo reads on light themes
PANEL_RADIUS = 36           # corner radius in px (final image)

# braille bit -> (dot column, dot row), inverse of brailleBit in internal/tui/splash.go
BITS = {0x01: (0, 0), 0x02: (0, 1), 0x04: (0, 2), 0x40: (0, 3),
        0x08: (1, 0), 0x10: (1, 1), 0x20: (1, 2), 0x80: (1, 3)}
CELL = re.compile(r"\x1b\[38;2;(\d+);(\d+);(\d+)m(.)\x1b\[0m| ")

with tempfile.TemporaryDirectory() as d:
    dump = os.path.join(d, "logo.ansi")
    subprocess.run(["go", "test", "-count=1", "-run", "TestDumpSplashANSI", "./internal/tui"],
                   env={**os.environ, "WAKIL_DUMP_SPLASH": dump}, check=True,
                   stdout=subprocess.DEVNULL)
    rows = open(dump, encoding="utf-8").read().split("\n")

cells = [[m.groups() for m in CELL.finditer(r)] for r in rows]
ncols, nrows = max(len(r) for r in cells), len(cells)
w, h = (ncols + 2 * PAD_X) * 2 * PITCH, (nrows + 2 * PAD_Y) * 4 * PITCH
im = Image.new("RGBA", (w * SS, h * SS), (0, 0, 0, 0))
dr = ImageDraw.Draw(im)
dr.rounded_rectangle((0, 0, w * SS - 1, h * SS - 1),
                     radius=PANEL_RADIUS * SS, fill=PANEL)
for cy, row in enumerate(cells):
    for cx, g in enumerate(row):
        if g[3] is None:
            continue
        col = (int(g[0]), int(g[1]), int(g[2]), 255)
        mask = ord(g[3]) - 0x2800
        for bit, (dx, dy) in BITS.items():
            if mask & bit:
                x = ((cx + PAD_X) * 2 + dx + 0.5) * PITCH * SS
                y = ((cy + PAD_Y) * 4 + dy + 0.5) * PITCH * SS
                r = RADIUS * SS
                dr.ellipse((x - r, y - r, x + r, y + r), fill=col)
im = im.resize((w, h), Image.LANCZOS)
im.save(OUT, optimize=True)
print(OUT, im.size, f"{ncols}x{nrows} cells")
