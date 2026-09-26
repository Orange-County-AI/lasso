#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["pillow"]
# ///
"""Render favicon and home-screen assets from the raster mascot.

Two sources, both generated images, never drawings:

- icon.png: the full mascot (the robot wrangler), for every size where its
  detail survives: 1024, 192, 180 and the ICO's 48px frame.
- favicon-art.png: the same character simplified for tiny sizes (a bold hat
  outline, the screen face, one arc of rope). Shrinking the full mascot to
  32px turns it to mush, so the 16 and 32 favicons come from this instead,
  cropped to the character first: the art carries a wide navy margin, and at
  16px every pixel of margin is a pixel the face does not get.
"""
from pathlib import Path

from PIL import Image, ImageChops

HERE = Path(__file__).resolve().parent
PUBLIC = HERE.parents[1] / "src/web/public"

# Margin kept around the character's bounding box, as a fraction of its size.
PAD = 0.04


def tight(art: Image.Image) -> Image.Image:
    # The background is flat navy, so anything well away from the corner's
    # colour is the character.
    bg = Image.new("RGB", art.size, art.getpixel((4, 4)))
    mask = ImageChops.difference(art, bg).convert("L").point(
        lambda v: 255 if v > 40 else 0)
    x0, y0, x1, y1 = mask.getbbox()
    half = int(max(x1 - x0, y1 - y0) * (0.5 + PAD))
    cx, cy = (x0 + x1) // 2, (y0 + y1) // 2
    return art.crop((cx - half, cy - half, cx + half, cy + half))


def main():
    icon = Image.open(HERE / "icon.png").convert("RGB")
    art = tight(Image.open(HERE / "favicon-art.png").convert("RGB"))

    def sized(src: Image.Image, n: int) -> Image.Image:
        return src.resize((n, n), Image.LANCZOS)

    sized(icon, 1024).save(PUBLIC / "lasso-icon.png", optimize=True)
    sized(icon, 192).save(PUBLIC / "favicon-192.png", optimize=True)
    sized(icon, 180).save(PUBLIC / "apple-touch-icon.png", optimize=True)
    sized(art, 32).save(PUBLIC / "favicon-32.png", optimize=True)
    sized(art, 16).save(PUBLIC / "favicon-16.png", optimize=True)
    # One ICO frame per size, and the small frames must be the simplified art,
    # so they are handed in explicitly rather than derived from the 48px one.
    sized(icon, 48).save(PUBLIC / "favicon.ico", format="ICO",
                         sizes=[(16, 16), (32, 32), (48, 48)],
                         append_images=[sized(art, 16), sized(art, 32)])


if __name__ == "__main__":
    main()
