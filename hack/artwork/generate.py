"""Generate the Ichnos logo set (CNCF artwork layout) and README banners as SVG.

Usage: python3 hack/artwork/generate.py [output-directory]

The output directory defaults to the repository root.
"""
import argparse
import os
from pathlib import Path
from typing import NamedTuple

TEAL = "#0E9F96"
AMBER = "#F59E0B"
NAVY = "#0B1F33"
WHITE = "#FFFFFF"
BLACK = "#000000"

# Three-toed track (an "ichnite") in a 120x120 box. The middle toe is the
# accent: the one step Ichnos points at.
ICON_W = ICON_H = 120


def icon(toe, accent, heel, x=0.0, y=0.0, s=1.0, opacity=None):
    op = f' opacity="{opacity}"' if opacity is not None else ""
    return f"""<g transform="translate({x:.2f} {y:.2f}) scale({s})"{op}>
  <g fill="none" stroke-linecap="round" stroke-width="15">
    <line x1="44" y1="63" x2="25" y2="35" stroke="{toe}"/>
    <line x1="60" y1="55" x2="60" y2="17" stroke="{accent}"/>
    <line x1="76" y1="63" x2="95" y2="35" stroke="{toe}"/>
  </g>
  <ellipse cx="60" cy="88" rx="18" ry="15" fill="{heel}"/>
</g>"""


# Monoline lowercase "ichnos", drawn as strokes so no font is needed.
# Box: x 0..285, y 12..100 (baseline 95, x-height top 55, ascender 15).
WORD_W = 285


def wordmark(color, x=0.0, y=0.0, s=1.0):
    return f"""<g transform="translate({x:.2f} {y:.2f}) scale({s})" fill="none" stroke="{color}" stroke-width="10" stroke-linecap="round" stroke-linejoin="round">
  <line x1="8" y1="57" x2="8" y2="95"/>
  <circle cx="8" cy="37" r="6.5" fill="{color}" stroke="none"/>
  <path d="M61.32 61.14 A20 20 0 1 0 61.32 88.86"/>
  <line x1="82" y1="17" x2="82" y2="95"/>
  <path d="M82 72.5 A17.5 17.5 0 0 1 117 72.5 V95"/>
  <line x1="138" y1="57" x2="138" y2="95"/>
  <path d="M138 72.5 A17.5 17.5 0 0 1 173 72.5 V95"/>
  <circle cx="214" cy="75" r="20"/>
  <path d="M276.26 60 A13 10 0 1 0 265 75 A13 10 0 1 1 253.74 90"/>
</g>"""


def svg(w, h, body, title):
    return f"""<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w} {h}" width="{w}" height="{h}" role="img" aria-label="{title}">
<title>{title}</title>
{body}
</svg>
"""


class Variant(NamedTuple):
    name: str
    toe: str
    accent: str
    heel: str
    word: str


VARIANTS = [
    Variant("color", TEAL, AMBER, TEAL, NAVY),
    Variant("black", BLACK, BLACK, BLACK, BLACK),
    Variant("white", WHITE, WHITE, WHITE, WHITE),
]


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write(text)


def logos(out):
    for v in VARIANTS:
        body = icon(v.toe, v.accent, v.heel, x=4, y=4)
        write(f"{out}/artwork/icon/{v.name}/ichnos-icon-{v.name}.svg",
              svg(128, 128, body, "Ichnos"))
        body = icon(v.toe, v.accent, v.heel, x=4, y=4) + wordmark(v.word, x=150, y=4)
        write(f"{out}/artwork/horizontal/{v.name}/ichnos-horizontal-{v.name}.svg",
              svg(150 + WORD_W + 8, 128, body, "Ichnos"))
        w = WORD_W + 16
        body = icon(v.toe, v.accent, v.heel, x=(w - ICON_W) / 2, y=4) + wordmark(v.word, x=8, y=118)
        write(f"{out}/artwork/stacked/{v.name}/ichnos-stacked-{v.name}.svg",
              svg(w, 226, body, "Ichnos"))
    # On dark backgrounds the color variant keeps the brand colors and turns the wordmark white.
    body = icon(TEAL, AMBER, TEAL, x=4, y=4) + wordmark(WHITE, x=150, y=4)
    write(f"{out}/artwork/horizontal/color-dark/ichnos-horizontal-color-dark.svg",
          svg(150 + WORD_W + 8, 128, body, "Ichnos"))


def trail(toe, accent, heel, opacity):
    """A faint walking trail of tracks across the banner, heading to the logo."""
    steps = [(44, 128, -18), (110, 92, 10), (178, 118, -14), (246, 74, 12),
             (316, 100, -10)]
    parts = []
    for i, (x, y, rot) in enumerate(steps):
        parts.append(f'<g transform="rotate({rot} {x + 18} {y + 18})">'
                     + icon(toe, accent if i == len(steps) - 1 else toe, heel,
                            x=x, y=y, s=0.3, opacity=opacity) + "</g>")
    return "\n".join(parts)


def banner(out, mode):
    W, H = 1280, 320
    if mode == "light":
        bg1, bg2, line, word, tag, sub = "#F2FBFA", "#FFFFFF", "#D7EEEC", NAVY, "#1F2D3A", "#57606A"
        trail_op = 0.16
    else:
        bg1, bg2, line, word, tag, sub = "#0F2230", "#0B1720", "#1C3A47", WHITE, "#E6EDF3", "#9BA7B3"
        trail_op = 0.22
    font = "-apple-system, BlinkMacSystemFont, 'Segoe UI', Helvetica, Arial, sans-serif"
    body = f"""<defs>
  <linearGradient id="bg" x1="0" y1="0" x2="1" y2="1">
    <stop offset="0" stop-color="{bg1}"/>
    <stop offset="1" stop-color="{bg2}"/>
  </linearGradient>
</defs>
<rect x="0.5" y="0.5" width="{W - 1}" height="{H - 1}" rx="24" fill="url(#bg)" stroke="{line}"/>
{trail(TEAL, AMBER, TEAL, trail_op)}
{icon(TEAL, AMBER, TEAL, x=424.5, y=40, s=1.0)}
{wordmark(word, x=570.5, y=40, s=1.0)}
<text x="640" y="232" text-anchor="middle" font-family="{font}" font-size="28" font-weight="600" fill="{tag}">Explains why your container build cache missed, on every pull request.</text>
<text x="640" y="272" text-anchor="middle" font-family="{font}" font-size="19" fill="{sub}">Open source · CI-agnostic · Built on BuildKit build records</text>"""
    write(f"{out}/docs/images/banner-{mode}.svg", svg(W, H, body, "Ichnos: explains why your container build cache missed"))


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("output", nargs="?", default=Path(__file__).resolve().parents[2],
                        help="directory to write artwork/ and docs/images/ into (default: the repository root)")
    out = parser.parse_args().output
    logos(out)
    banner(out, "light")
    banner(out, "dark")


if __name__ == "__main__":
    main()
