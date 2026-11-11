#!/usr/bin/env python3
"""WCAG AA contrast check for the Weave design tokens in internal/web/static/weave.css.
Usage: python3 scripts/check-contrast.py   (exit 1 if any required pair is below its minimum)."""
import re, sys, pathlib

css = (pathlib.Path(__file__).resolve().parent.parent / "internal/web/static/weave.css").read_text()


def block(selector_start):
    i = css.index(selector_start)
    j = css.index("}", i)
    return css[i:j]


def tokens(b):
    return dict(re.findall(r"(--[\w-]+):\s*(#[0-9a-fA-F]{6})", b))


light = tokens(block(":root{"))
dark = tokens(block(':root[data-theme="dark"]{'))


def lum(h):
    h = h.lstrip("#")
    c = [int(h[i:i + 2], 16) / 255 for i in (0, 2, 4)]
    c = [x / 12.92 if x <= 0.03928 else ((x + 0.055) / 1.055) ** 2.4 for x in c]
    return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]


def ratio(a, b):
    la, lb = sorted((lum(a), lum(b)), reverse=True)
    return (la + 0.05) / (lb + 0.05)


# (foreground, background, minimum ratio)
pairs = []
for fg in ("--fg", "--fg-2", "--muted"):
    for bg in ("--bg", "--surface", "--surface-2", "--surface-3", "--list-bg", "--tile-bg"):
        pairs.append((fg, bg, 4.5))
for fg, bg in (("--accent", "--surface"), ("--accent", "--bg"), ("--accent", "--accent-soft"), ("--accent-fg", "--accent"), ("--accent-fg", "--accent-hover"),
               ("--danger", "--surface"), ("--danger", "--danger-soft"), ("--success", "--surface"), ("--success", "--success-soft"),
               ("--warning", "--surface"), ("--warning", "--warning-soft"), ("--fg", "--accent-soft")):
    pairs.append((fg, bg, 4.5))
for n in "red orange yellow green teal blue indigo purple pink gray".split():
    pairs.append(("--label-fg", "--label-" + n, 4.5))
pairs += [("--border-strong", "--surface", 3.0), ("--focus-ring", "--surface", 3.0), ("--focus-ring", "--bg", 3.0), ("--drop-indicator", "--list-bg", 3.0)]

bad = 0
for name, t in (("light", light), ("dark", dark)):
    for fg, bg, mn in pairs:
        r = ratio(t[fg], t[bg])
        if r < mn:
            bad += 1
            print("FAIL %-5s %-14s on %-14s %.2f < %.1f" % (name, fg, bg, r, mn))

# ---- board / column / card colours (boards_colors.css): tinted surfaces must keep --fg / --fg-2 / --muted readable ----
ROOT = pathlib.Path(__file__).resolve().parent.parent
colors_css = (ROOT / "internal/web/static/boards_colors.css").read_text()
tint = {}
blocks = {}
for name, sel in (("light", ":root{"), ("dark", ':root[data-theme="dark"]{'), ("media", ':root:not([data-theme="light"]){')):
    i = colors_css.index(sel)
    blk = colors_css[i:colors_css.index("}", i)]
    blocks[name] = {k: float(v) / 100 for k, v in re.findall(r"--tint-(board|list|fill):(\d+)%", blk)}
tint = {"light": blocks["light"], "dark": blocks["dark"]}
cbad0 = 0
if blocks["media"] != blocks["dark"]:
    print("FAIL prefers-color-scheme tint block differs from the [data-theme=dark] block: %s vs %s" % (blocks["media"], blocks["dark"]))
    cbad0 += 1
for name in ("light", "dark"):
    if set(tint[name]) != {"board", "list", "fill"}:
        print("FAIL %s: missing --tint-board/list/fill" % name)
        cbad0 += 1
# design caps (docs/DESIGN.md): faint board canvas and column tint, so nothing reads as a large saturated area
for name, caps in (("light", {"board": .10, "list": .08}), ("dark", {"board": .08, "list": .08})):
    for k, mx in caps.items():
        if tint[name].get(k, 1) > mx:
            print("FAIL %s --tint-%s %.0f%% exceeds the %.0f%% cap" % (name, k, tint[name][k] * 100, mx * 100))
            cbad0 += 1


def lin(c):
    return c / 12.92 if c <= 0.04045 else ((c + 0.055) / 1.055) ** 2.4


def to_oklab(h):
    h = h.lstrip("#")
    r, g, b = [lin(int(h[i:i + 2], 16) / 255) for i in (0, 2, 4)]
    l = (0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b) ** (1 / 3)
    m = (0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b) ** (1 / 3)
    s_ = (0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b) ** (1 / 3)
    return (0.2104542553 * l + 0.7936177850 * m - 0.0040720468 * s_, 1.9779984951 * l - 2.4285922050 * m + 0.4505937099 * s_, 0.0259040371 * l + 0.7827717662 * m - 0.8086757660 * s_)


def from_oklab(L, a, b):
    l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3
    m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3
    s_ = (L - 0.0894841775 * a - 1.2914855480 * b) ** 3
    rgb = (4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s_, -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s_, -0.0041960863 * l - 0.7034186147 * m + 1.7076147010 * s_)
    out = []
    for c in rgb:
        c = max(0.0, min(1.0, c))
        c = 12.92 * c if c <= 0.0031308 else 1.055 * c ** (1 / 2.4) - 0.055
        out.append(round(c * 255))
    return "#%02x%02x%02x" % tuple(out)


def mix(color, pct, base):
    a, b = to_oklab(color), to_oklab(base)
    return from_oklab(*[x * pct + y * (1 - pct) for x, y in zip(a, b)])


# every colour a user can pick: the Weave presets plus the Planka palette names (used by labels and old lists)
palette = set(re.findall(r'\{"[a-z]+", "[A-Za-z ]+", "(#[0-9a-f]{6})"\}', (ROOT / "internal/domain/boards/colors.go").read_text()))
ui_go = (ROOT / "internal/domain/boards/ui.go").read_text()
palette |= set(re.findall(r'"[a-z-]+": "(#[0-9a-f]{6})"', ui_go[:ui_go.index("func init()")]))
cbad = cbad0
for name, t in (("light", light), ("dark", dark)):
    k = tint[name]
    for c in sorted(palette):
        col_bg = mix(c, k["list"], t["--list-bg"])
        col_hov = mix(c, k["list"], t["--list-bg-hover"])
        fill = mix(c, k["fill"], t["--tile-bg"])
        board = mix(c, k["board"], t["--bg"])
        # board canvas tint: header, filter bar and the "Add list" tile sit directly on it
        for fgn in ("--fg", "--fg-2", "--muted"):
            r = ratio(t[fgn], board)
            if r < 4.5:
                cbad += 1
                print("FAIL %-5s %-7s on board tint of %s (%s) %.2f < 4.5" % (name, fgn, c, board, r))
        # --muted is remapped to --fg-2 inside coloured columns and filled tiles (see boards_colors.css)
        for fgn, bgs, mn in (("--fg", (col_bg, col_hov, fill), 4.5), ("--fg-2", (col_bg, col_hov, fill), 4.5)):
            for bg in bgs:
                r = ratio(t[fgn], bg)
                if r < mn:
                    cbad += 1
                    print("FAIL %-5s %-7s on tint of %s (%s) %.2f < %.1f" % (name, fgn, c, bg, r, mn))
    # meta chips on a filled card sit on --surface
    for fgn in ("--danger", "--warning", "--success", "--fg"):
        r = ratio(t[fgn], t["--surface"])
        if r < 4.5:
            cbad += 1
            print("FAIL %-5s %s on --surface %.2f" % (name, fgn, r))
print("colours: %d colours x 2 themes (board canvas + pill-mode columns + filled tiles; fill-mode columns are neutral --list-bg), %d failures" % (len(palette), cbad))
bad += cbad
print("contrast: %d pairs x 2 themes, %d failures" % (len(pairs), bad))
sys.exit(1 if bad else 0)
