#!/usr/bin/env -S uv run --with pillow --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["pillow", "fonttools", "brotli"]
# ///
"""Render src/web/public's icon set, and docs/brand, from one vector mark.

The mark is a lasso seen at an angle: a loop flattened into perspective, the
honda (the small fixed eye a real lasso's running end feeds through) on its low
edge, and the rope trailing off to the right. It is drawn as NEON — a saturated
bloom under a near-white core — because the icon's job is to hold its own on a
home screen beside Moshi and SSHHIP, not to match the app's monochrome chrome.

Everything ships from `mark()`, so the silhouette is defined once:

    docs/icon/lasso.svg     the master, and what the README embeds
    src/web/public/lasso.svg  the SVG favicon modern browsers prefer
    lasso-icon.png    1024   manifest / store art
    favicon-192.png    192
    apple-touch-icon   180
    favicon-32.png      32
    favicon-16.png      16
    favicon.ico              16 + 32 + 48

A neon mark does NOT survive being downscaled: at 16px an 8-unit core stroke is
a quarter of a pixel and averages away to nothing, leaving a colored smudge. So
each size is rendered from its OWN svg with the stroke weights in TIERS — small
sizes get a fatter core and a dimmer bloom — rather than resampled from the big
one. That is the same reason the old pixel-grid build kept a separate 16px
drawing, for the same underlying reason.

Rasterizing needs a renderer that implements SVG filters (feGaussianBlur is the
glow). cairosvg does not; resvg does, and mise pins it — see mise.toml.

    ./docs/icon/build.py
"""

import math
import subprocess
import sys
import tempfile
from pathlib import Path

from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.ttLib import TTFont
from PIL import Image

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
PUBLIC = ROOT / "src" / "web" / "public"
BRAND = ROOT / "docs" / "brand"

# Blue/violet: an electric core over a violet ambient bloom.
HUE, CORE, AMB = "#6f9dff", "#eef3ff", "#7c5cf5"


# --- geometry ---------------------------------------------------------------

def _smooth(pts, close=False):
    """Catmull-Rom through pts -> a cubic bezier path."""
    p = [pts[0]] + list(pts) + [pts[-1]]
    d = f"M {p[1][0]:.1f} {p[1][1]:.1f}"
    for i in range(1, len(p) - 2):
        p0, p1, p2, p3 = p[i - 1], p[i], p[i + 1], p[i + 2]
        c1 = (p1[0] + (p2[0] - p0[0]) / 6, p1[1] + (p2[1] - p0[1]) / 6)
        c2 = (p2[0] - (p3[0] - p1[0]) / 6, p2[1] - (p3[1] - p1[1]) / 6)
        d += f" C {c1[0]:.1f} {c1[1]:.1f} {c2[0]:.1f} {c2[1]:.1f} {p2[0]:.1f} {p2[1]:.1f}"
    return d + (" Z" if close else "")


def _ellipse(cx, cy, rx, ry, rot, n=28):
    a = math.radians(rot)
    ca, sa = math.cos(a), math.sin(a)
    out = []
    for i in range(n + 1):
        t = math.radians(360 * i / n)
        x, y = rx * math.cos(t), ry * math.sin(t)
        out.append((cx + x * ca - y * sa, cy + x * sa + y * ca))
    return out


LOOP = _smooth(_ellipse(244, 188, 174, 108, -18), close=True)
HONDA = _smooth(_ellipse(283, 300, 31, 27, -10), close=True)
TAIL = _smooth([(287, 327), (289, 350), (291, 382), (303, 408),
                (329, 426), (362, 428), (386, 412), (390, 388)])

# core / near-bloom / far-bloom stroke widths, and far-bloom opacity, by size.
# Small renders need a fatter core and a quieter bloom or the mark washes out.
TIERS = ((120, (8, 17, 24, 0.80)),
         (48, (13, 21, 26, 0.62)),
         (24, (19, 26, 30, 0.46)),
         (0, (26, 32, 34, 0.34)))


def _tier(px):
    return next(v for lo, v in TIERS if px >= lo)


def mark(px=512, plate=True):
    """The mark as an SVG string, tuned for rendering at `px` pixels."""
    core_w, near_w, far_w, far_o = _tier(px)
    plate_bg = ('<rect width="512" height="512" rx="114.5" fill="url(#plate)"/>'
                '<ellipse cx="250" cy="228" rx="218" ry="208" fill="url(#amb)"/>')
    return f'''<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" width="{px}" height="{px}" role="img" aria-label="lasso">
<defs>
<radialGradient id="plate" cx="50%" cy="38%" r="82%">
<stop offset="0%" stop-color="#131a1b"/><stop offset="58%" stop-color="#070a0b"/><stop offset="100%" stop-color="#000"/>
</radialGradient>
<radialGradient id="amb" cx="50%" cy="42%" r="56%">
<stop offset="0%" stop-color="{AMB}" stop-opacity=".18"/><stop offset="100%" stop-color="{AMB}" stop-opacity="0"/>
</radialGradient>
<filter id="far" x="-70%" y="-70%" width="240%" height="240%"><feGaussianBlur stdDeviation="24"/></filter>
<filter id="near" x="-40%" y="-40%" width="180%" height="180%"><feGaussianBlur stdDeviation="6"/></filter>
<clipPath id="sq"><rect width="512" height="512" rx="114.5"/></clipPath>
<!-- The rope passes IN FRONT of the loop's far edge. Punching the loop away
     under the tail is what makes the crossing read as depth and not as a
     join, which is the difference between a lasso and a balloon. -->
<mask id="over" maskUnits="userSpaceOnUse" x="0" y="0" width="512" height="512">
<rect width="512" height="512" fill="#fff"/>
<path d="{TAIL}" fill="none" stroke="#000" stroke-width="34" stroke-linecap="round"/>
</mask>
</defs>
<g{' clip-path="url(#sq)"' if plate else ''}>{plate_bg if plate else ''}
<g fill="none" stroke-linecap="round" stroke-linejoin="round">
<g mask="url(#over)">
<g filter="url(#far)" stroke="{HUE}" opacity="{far_o}" stroke-width="{far_w}"><path d="{LOOP}"/></g>
<g filter="url(#near)" stroke="{HUE}" opacity=".95" stroke-width="{near_w}"><path d="{LOOP}"/></g>
<g stroke="{CORE}" stroke-width="{core_w}"><path d="{LOOP}"/></g>
</g>
<g filter="url(#far)" stroke="{HUE}" opacity="{far_o}" stroke-width="{far_w}"><path d="{HONDA}"/><path d="{TAIL}"/></g>
<g filter="url(#near)" stroke="{HUE}" opacity=".95" stroke-width="{near_w}"><path d="{HONDA}"/><path d="{TAIL}"/></g>
<g stroke="{CORE}" stroke-width="{core_w}"><path d="{HONDA}"/><path d="{TAIL}"/></g>
</g></g></svg>'''


FONT = PUBLIC / "fonts" / "JetBrainsMonoNerdFontMono-Regular.woff2"


def text_path(s, size, x, y):
    """The word as OUTLINES, not a <text> element.

    A <text> node renders as whatever font the rasterizer happens to resolve —
    resvg finds no `ui-monospace` at all and drops the glyphs, and GitHub would
    substitute its own. Outlining the string against the font the app already
    ships is what makes the wordmark render identically everywhere.
    """
    font = TTFont(FONT)
    glyphs, cmap = font.getGlyphSet(), font.getBestCmap()
    upem = font["head"].unitsPerEm
    scale = size / upem
    out, pen_x = [], 0.0
    for ch in s:
        name = cmap[ord(ch)]
        pen = SVGPathPen(glyphs)
        glyphs[name].draw(pen)
        d = pen.getCommands()
        if d:
            # y flips: font space is up-positive, SVG user space is down-positive
            out.append(f'<path transform="translate({x + pen_x * scale:.2f} {y}) '
                       f'scale({scale:.5f} {-scale:.5f})" d="{d}"/>')
        pen_x += glyphs[name].width
    return "".join(out)


def wordmark():
    """Horizontal lockup: the mark, then the name. For the README header."""
    word = text_path("lasso", 142, 330, 192)
    core_w, near_w, far_w, far_o = _tier(512)
    return f'''<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 880 280" width="880" height="280" role="img" aria-label="lasso">
<defs>
<radialGradient id="amb" cx="26%" cy="50%" r="60%">
<stop offset="0%" stop-color="{AMB}" stop-opacity=".26"/><stop offset="100%" stop-color="{AMB}" stop-opacity="0"/>
</radialGradient>
<filter id="far" x="-70%" y="-70%" width="240%" height="240%"><feGaussianBlur stdDeviation="24"/></filter>
<filter id="near" x="-40%" y="-40%" width="180%" height="180%"><feGaussianBlur stdDeviation="6"/></filter>
<filter id="tfar" x="-40%" y="-70%" width="180%" height="240%"><feGaussianBlur stdDeviation="13"/></filter>
<mask id="over" maskUnits="userSpaceOnUse" x="-200" y="-200" width="1400" height="800">
<rect x="-200" y="-200" width="1400" height="800" fill="#fff"/>
<g transform="translate(24 8) scale(0.52)"><path d="{TAIL}" fill="none" stroke="#000" stroke-width="34" stroke-linecap="round"/></g>
</mask>
</defs>
<rect width="880" height="280" fill="#000"/>
<ellipse cx="250" cy="140" rx="430" ry="220" fill="url(#amb)"/>
<g transform="translate(24 8) scale(0.52)" fill="none" stroke-linecap="round" stroke-linejoin="round">
<g mask="url(#over)">
<g filter="url(#far)" stroke="{HUE}" opacity="{far_o}" stroke-width="{far_w}"><path d="{LOOP}"/></g>
<g filter="url(#near)" stroke="{HUE}" opacity=".95" stroke-width="{near_w}"><path d="{LOOP}"/></g>
<g stroke="{CORE}" stroke-width="{core_w}"><path d="{LOOP}"/></g>
</g>
<g filter="url(#far)" stroke="{HUE}" opacity="{far_o}" stroke-width="{far_w}"><path d="{HONDA}"/><path d="{TAIL}"/></g>
<g filter="url(#near)" stroke="{HUE}" opacity=".95" stroke-width="{near_w}"><path d="{HONDA}"/><path d="{TAIL}"/></g>
<g stroke="{CORE}" stroke-width="{core_w}"><path d="{HONDA}"/><path d="{TAIL}"/></g>
</g>
<g fill="{HUE}" opacity=".85" filter="url(#tfar)">{word}</g>
<g fill="{CORE}">{word}</g>
</svg>'''


# --- rasterizing ------------------------------------------------------------

def png(svg_text, px, dest):
    """resvg is the renderer because it implements SVG filters; cairosvg
    silently drops feGaussianBlur, which is the entire glow."""
    with tempfile.NamedTemporaryFile("w", suffix=".svg", delete=False) as f:
        f.write(svg_text)
        tmp = f.name
    try:
        subprocess.run(["resvg", "--width", str(px), "--height", str(px),
                        tmp, str(dest)], check=True, capture_output=True)
    except FileNotFoundError:
        sys.exit("resvg not found — `mise install` (it is pinned in mise.toml)")
    except subprocess.CalledProcessError as e:
        sys.exit(f"resvg failed: {e.stderr.decode()}")
    finally:
        Path(tmp).unlink(missing_ok=True)


def flatten_and_shrink(dest):
    """Composite apple-touch-icon onto black and palette-quantize it.

    Apple masks this icon itself and asks for it square and fully opaque, so
    flattening is correct rather than a compromise -- and once it is opaque,
    MEDIANCUT halves the file with no visible change.

    The other renders are NOT quantized. They need their transparent rounded
    corners, and the only alpha-capable method (FASTOCTREE) contours the glow
    into visible bands, which is a worse trade than the bytes it saves.
    """
    im = Image.open(dest).convert("RGBA")
    bg = Image.new("RGBA", im.size, (0, 0, 0, 255))
    im = Image.alpha_composite(bg, im).convert("RGB")
    im.quantize(colors=255, method=Image.Quantize.MEDIANCUT).save(dest, optimize=True)


def main():
    BRAND.mkdir(parents=True, exist_ok=True)
    (HERE / "lasso.svg").write_text(mark(512))
    (PUBLIC / "lasso.svg").write_text(mark(512))
    (BRAND / "lasso-wordmark.svg").write_text(wordmark())

    for name, px in (("lasso-icon.png", 1024), ("favicon-192.png", 192),
                     ("apple-touch-icon.png", 180), ("favicon-32.png", 32),
                     ("favicon-16.png", 16)):
        png(mark(px), px, PUBLIC / name)
        if name == "apple-touch-icon.png":
            flatten_and_shrink(PUBLIC / name)

    # .ico carries its own three renders; each is tier-tuned, not a downscale.
    with tempfile.TemporaryDirectory() as d:
        frames = []
        for px in (48, 32, 16):
            p = Path(d) / f"{px}.png"
            png(mark(px), px, p)
            frames.append(Image.open(p).convert("RGBA"))
        frames[0].save(PUBLIC / "favicon.ico", format="ICO",
                       sizes=[(48, 48), (32, 32), (16, 16)],
                       append_images=frames[1:])

    with tempfile.NamedTemporaryFile("w", suffix=".svg", delete=False) as f:
        f.write(wordmark())
        wm = f.name
    subprocess.run(["resvg", "--width", "1760", wm,
                    str(BRAND / "lasso-wordmark.png")], check=True)
    Path(wm).unlink(missing_ok=True)

    for p in sorted(PUBLIC.glob("*.png")) + sorted(BRAND.glob("*.png")):
        print(f"  {p.relative_to(ROOT)}  {Image.open(p).size}")


if __name__ == "__main__":
    main()
