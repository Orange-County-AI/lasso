#!/usr/bin/env -S uv run --with pillow --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["pillow"]
# ///
"""Render src/web/public's icon set from the two pixel grids beside this file.

The mark is a lasso — a wide loop, a knotted neck with two arms, and a tail
that hooks right — drawn on a 32x32 grid of cells. Everything shipped is that
grid at an INTEGER number of pixels per cell, so no rasterizer ever gets to
resample it and every edge stays hard:

    lasso-icon.png      1024  = 32 cells x 32px
    favicon-192.png      192  = 32 cells x 6px
    apple-touch-icon    180   = the 192 render trimmed 6px a side, because
                                180/32 is 5.625 and uneven cells look wrong
                                next to each other at that size
    favicon-32.png        32  = the grid itself, 1:1

lasso16.txt is a SEPARATE drawing, not a downscale: at half the cells the
knot's arms and second wrap have nowhere to go, and averaging them shut turns
the loop into a filled blob. The 48px frame in favicon.ico is that 16 tripled.

Run it after editing either grid:  ./docs/icon/build.py
"""

from pathlib import Path

from PIL import Image

HERE = Path(__file__).resolve().parent
PUBLIC = HERE.parent.parent / "src" / "web" / "public"


def load(name: str) -> Image.Image:
    rows = (HERE / name).read_text().splitlines()
    size = len(rows)
    if any(len(r) != size for r in rows):
        raise SystemExit(f"{name}: not square — {size} rows")
    im = Image.new("L", (size, size), 0)
    im.putdata([255 if c == "#" else 0 for r in rows for c in r])
    return im


def up(im: Image.Image, factor: int) -> Image.Image:
    return im.resize((im.width * factor, im.height * factor), Image.NEAREST)


def main() -> None:
    grid32, grid16 = load("lasso32.txt"), load("lasso16.txt")
    up(grid32, 32).save(PUBLIC / "lasso-icon.png")
    up(grid32, 6).save(PUBLIC / "favicon-192.png")
    up(grid32, 6).crop((6, 6, 186, 186)).save(PUBLIC / "apple-touch-icon.png")
    grid32.save(PUBLIC / "favicon-32.png")
    grid16.save(PUBLIC / "favicon-16.png")
    up(grid16, 3).convert("RGBA").save(
        PUBLIC / "favicon.ico",
        format="ICO",
        sizes=[(16, 16), (32, 32), (48, 48)],
        append_images=[grid16.convert("RGBA"), grid32.convert("RGBA")],
    )
    for name in (
        "lasso-icon.png",
        "apple-touch-icon.png",
        "favicon-192.png",
        "favicon-32.png",
        "favicon-16.png",
        "favicon.ico",
    ):
        print(name, Image.open(PUBLIC / name).size)


if __name__ == "__main__":
    main()
