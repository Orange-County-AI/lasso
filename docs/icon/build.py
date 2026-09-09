#!/usr/bin/env python3
"""Render favicon and home-screen assets from the current standalone L mark.

The amber outline and teal terminal cursor reproduce the mark at the left of
brand/lasso-wordmark.png. The wordmark itself is deliberately left untouched.
Requires cairosvg and Pillow.
"""
from pathlib import Path
import io

import cairosvg
from PIL import Image

HERE = Path(__file__).resolve().parent
PUBLIC = HERE.parents[1] / "src/web/public"
MARK = '''<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 320 320">
  <rect width="320" height="320" rx="36" fill="#00172e"/>
  <path fill="#faa968" d="M32 32H130L148 50V188H260L280 208V236H254V214H122V58H58V258H226V284H46L32 270Z"/>
  <path fill="#8cbfb8" d="M254 254H280V284H254Z"/>
</svg>
'''


def main():
    (HERE / "lasso.svg").write_text(MARK)
    (PUBLIC / "lasso.svg").write_text(MARK)
    for name, size in (("lasso-icon.png", 1024), ("favicon-192.png", 192),
                       ("apple-touch-icon.png", 180), ("favicon-32.png", 32),
                       ("favicon-16.png", 16)):
        cairosvg.svg2png(bytestring=MARK.encode(), output_width=size,
                        output_height=size, write_to=str(PUBLIC / name))
    rendered = cairosvg.svg2png(bytestring=MARK.encode(), output_width=256,
                               output_height=256)
    Image.open(io.BytesIO(rendered)).save(PUBLIC / "favicon.ico", format="ICO",
                                        sizes=[(16, 16), (32, 32), (48, 48)])


if __name__ == "__main__":
    main()
