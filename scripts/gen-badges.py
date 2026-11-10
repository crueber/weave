#!/usr/bin/env python3
"""Generates the flat 'at a glance' README badges into docs/badges/*.svg (no gradients, no external fonts).
Run: python3 scripts/gen-badges.py   (edit BADGES below, then commit the SVGs)"""
import os

LABEL_BG = "#4b4a45"   # neutral label half
ACCENT = "#3e4fd6"     # product accent (matches the app's light-theme accent)
WARN = "#a85a00"       # amber: a hard requirement (>= 4.5:1 with white text)
BADGES = [  # (file, label, value, value colour)
    ("auth", "sign-in", "OpenID Connect required", WARN),
    ("database", "database", "SQLite, built in", ACCENT),
    ("deploy", "deploy", "one container, one replica", ACCENT),
    ("cli", "CLI", "Linux · macOS · Windows", ACCENT),
    ("agents", "agents", "CLI + Claude Code skill", ACCENT),
    ("stack", "built with", "Go + HTMX", ACCENT),
]
CHAR = 6.6   # approx px per char at 11px Verdana/DejaVu (slightly generous so text never clips)
PAD = 10

def width(t):
    return int(len(t) * CHAR + 2 * PAD)

def svg(label, value, color):
    lw, vw = width(label), width(value)
    w, h = lw + vw, 22
    esc = lambda s: s.replace("&", "&amp;").replace("<", "&lt;")
    return f'''<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" role="img" aria-label="{esc(label)}: {esc(value)}">
  <title>{esc(label)}: {esc(value)}</title>
  <clipPath id="r"><rect width="{w}" height="{h}" rx="4"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="{lw}" height="{h}" fill="{LABEL_BG}"/>
    <rect x="{lw}" width="{vw}" height="{h}" fill="{color}"/>
  </g>
  <g fill="#fff" font-family="Verdana,'DejaVu Sans',sans-serif" font-size="11" text-anchor="middle">
    <text x="{lw/2:.1f}" y="15">{esc(label)}</text>
    <text x="{lw + vw/2:.1f}" y="15" font-weight="700">{esc(value)}</text>
  </g>
</svg>
'''

out = os.path.join(os.path.dirname(__file__), "..", "docs", "badges")
os.makedirs(out, exist_ok=True)
for f, l, v, c in BADGES:
    open(os.path.join(out, f + ".svg"), "w").write(svg(l, v, c))
print("wrote", len(BADGES), "badges")
