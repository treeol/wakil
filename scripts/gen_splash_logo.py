#!/usr/bin/env python3
"""Regenerate the TUI sprite sources (internal/tui/splash/*.png).

Dev-time only (needs Pillow). Crops each asset to its content and downscales it.
The TUI embeds the results and renders them at runtime, at whatever size fits,
as braille truecolor art with a transparent background.

    logo.png <- assets/wakil-logo.png            (start-up splash)

    python3 scripts/gen_splash_logo.py
"""
from PIL import Image

JOBS = (
    ("assets/wakil-logo.png", "internal/tui/splash/logo.png", 384),
)

for src, dst, width in JOBS:
    im = Image.open(src).convert("RGBA")
    im = im.crop(im.getchannel("A").getbbox())
    w, h = im.size
    im = im.resize((width, round(width * h / w)), Image.LANCZOS)
    im.save(dst, optimize=True)
    print(dst, im.size)
