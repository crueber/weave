#!/usr/bin/env python3
"""Converts the PNGs from scripts/site/shots.mjs into the website's WebP images.

  python3 scripts/site/webp.py SHOTS_DIR site/img

Each screenshot becomes NAME-1280.webp and NAME-640.webp (phone shots: NAME.webp at their own width).
Metadata is dropped (Pillow writes no EXIF/XMP unless asked). Optional env: Q (default 80).
"""
import glob, os, sys
from PIL import Image

src, out = sys.argv[1], sys.argv[2]
q = int(os.environ.get("Q", 80))
os.makedirs(out, exist_ok=True)
total = 0
for f in sorted(glob.glob(os.path.join(src, "*.png"))):
    name = os.path.splitext(os.path.basename(f))[0]
    im = Image.open(f).convert("RGB")
    targets = [(name, im.width)] if im.width <= 640 else [(name + "-1280", 1280), (name + "-640", 640)]
    for base, w in targets:
        r = im if im.width == w else im.resize((w, round(im.height * w / im.width)), Image.LANCZOS)
        p = os.path.join(out, base + ".webp")
        r.save(p, "WEBP", quality=q, method=6)
        total += os.path.getsize(p)
        print(f"{p}  {r.width}x{r.height}  {os.path.getsize(p) // 1024} KB")
print(f"total {total // 1024} KB")
