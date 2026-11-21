#!/usr/bin/env python3
"""WCAG AA contrast check for the website tokens in site/style.css (same method as scripts/check-contrast.py).
Usage: python3 scripts/site/check-contrast.py   (exit 1 if any pair the site uses is below its minimum).
Also checks that the dark block and the prefers-color-scheme block carry identical values."""
import re, sys, pathlib

css = (pathlib.Path(__file__).resolve().parents[2] / "site/style.css").read_text()


def block(start):
    i = css.index(start)
    return css[i:css.index("}", i)]


def tokens(b):
    return dict(re.findall(r"(--[\w-]+):\s*(#[0-9a-fA-F]{6})", b))


light = tokens(block(":root{"))
dark = tokens(block(':root[data-theme="dark"]{'))
dark_media = tokens(block(':root:not([data-theme="light"]){'))


def lum(h):
    h = h.lstrip("#")
    c = [int(h[i:i + 2], 16) / 255 for i in (0, 2, 4)]
    c = [x / 12.92 if x <= 0.03928 else ((x + 0.055) / 1.055) ** 2.4 for x in c]
    return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]


def ratio(a, b):
    la, lb = sorted((lum(a), lum(b)), reverse=True)
    return (la + 0.05) / (lb + 0.05)


# (foreground, background, minimum) for every text/background pair the site actually renders
pairs = []
for fg in ("--fg", "--fg-2", "--muted"):
    for bg in ("--bg", "--surface", "--surface-2", "--band"):
        pairs.append((fg, bg, 4.5))
pairs += [("--fg", "--surface-3", 4.5),            # inline code, button hover
          ("--accent", "--bg", 4.5), ("--accent", "--surface", 4.5), ("--accent", "--band", 4.5),  # links, eyebrows
          ("--accent", "--surface-2", 3.0),        # pressed theme icon (graphic)
          ("--accent-fg", "--accent", 4.5), ("--accent-fg", "--accent-hover", 4.5),              # primary button, tabs, steps
          ("--accent-hover", "--bg", 4.5), ("--accent-hover", "--surface", 4.5),                 # link hover
          ("--code-fg", "--code-bg", 4.5), ("--code-muted", "--code-bg", 4.5),                   # code blocks
          ("--border-strong", "--surface", 3.0), ("--border-strong", "--bg", 3.0),               # button outline (non-text)
          ("--focus-ring", "--bg", 3.0), ("--focus-ring", "--surface", 3.0), ("--focus-ring", "--band", 3.0)]

fails = 0
if dark != dark_media:
    print("FAIL dark tokens differ between [data-theme=dark] and the prefers-color-scheme block:",
          sorted(set(dark.items()) ^ set(dark_media.items())))
    fails += 1
for name, t in (("light", light), ("dark", dark)):
    for fg, bg, need in pairs:
        r = ratio(t[fg], t[bg])
        ok = r >= need
        fails += not ok
        print(f"{'ok  ' if ok else 'FAIL'} {name:5} {fg:15} on {bg:13} {r:5.2f} (>= {need})")
print(f"{fails} failure(s)")
sys.exit(1 if fails else 0)
