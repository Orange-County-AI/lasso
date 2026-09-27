#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["pillow"]
# ///
"""Render favicon and home-screen assets from the raster icon.

One source, never a drawing: icon.png, the Execution Associates EXA monogram
(white, from design.execution.associates' exa-mark-white-alpha.png) centred on
the brand's ink #1B0A2C at ~60% of the square, at every size including the 16
and 32px browser-tab favicons.
"""
from pathlib import Path

from PIL import Image

HERE = Path(__file__).resolve().parent
PUBLIC = HERE.parents[1] / "src/web/public"


def main():
    icon = Image.open(HERE / "icon.png").convert("RGB")

    def sized(src: Image.Image, n: int) -> Image.Image:
        return src.resize((n, n), Image.LANCZOS)

    sized(icon, 1024).save(PUBLIC / "lasso-icon.png", optimize=True)
    sized(icon, 192).save(PUBLIC / "favicon-192.png", optimize=True)
    sized(icon, 180).save(PUBLIC / "apple-touch-icon.png", optimize=True)
    sized(icon, 32).save(PUBLIC / "favicon-32.png", optimize=True)
    sized(icon, 16).save(PUBLIC / "favicon-16.png", optimize=True)
    # One ICO frame per size, each resampled from the full-size art rather than
    # from the 48px frame, so the small ones are as sharp as LANCZOS gets them.
    sized(icon, 48).save(PUBLIC / "favicon.ico", format="ICO",
                         sizes=[(16, 16), (32, 32), (48, 48)],
                         append_images=[sized(icon, 16), sized(icon, 32)])


if __name__ == "__main__":
    main()
