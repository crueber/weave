#!/usr/bin/env python3
"""Shrinks the screenshots from shots.mjs in place: 256-colour palette PNGs without dithering, no metadata chunks.

  python3 scripts/demo/pngopt.py docs/screenshots [NAME.png ...]

The UI is flat (no gradients), so a palette keeps it crisp at a third of the size. Only files newer than this run's
screenshots matter, but re-running on already optimised files is harmless (they are already palette images).
"""
import glob, os, sys
from PIL import Image

d = sys.argv[1]
files = [os.path.join(d, f) for f in sys.argv[2:]] or sorted(glob.glob(os.path.join(d, "*.png")))
total = 0
for f in files:
    im = Image.open(f)
    if im.mode != "P":
        im = im.convert("RGB").quantize(256, method=Image.Quantize.MEDIANCUT, dither=Image.Dither.NONE)
    im.save(f, optimize=True)  # Pillow writes no tEXt/eXIf unless asked
    total += os.path.getsize(f)
    print(f"{f}  {im.width}x{im.height}  {os.path.getsize(f) // 1024} KB")
print(f"total {total // 1024} KB")
