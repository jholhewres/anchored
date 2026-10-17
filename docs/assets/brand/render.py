#!/usr/bin/env python3
"""The README mark, rendered from the logo.

`logo.png` is the only file anyone edits (generated with Codex imagegen, in the
style of devpit's mascot). The mark the READMEs show comes out of here, so a
copy kept by hand cannot drift from it.

    python3 docs/assets/brand/render.py
"""
from pathlib import Path

from PIL import Image, ImageDraw

HERE = Path(__file__).resolve().parent
SOURCE = HERE / "logo.png"
RADIUS = 0.15  # just inside the tile's own corner, so its dark fringe is cut


def rounded(size: int) -> Image.Image:
    """Rendered at 4x and reduced, so the corner is clean at small sizes too."""
    big = size * 4
    tile = Image.open(SOURCE).convert("RGB").resize((big, big), Image.LANCZOS).convert("RGBA")
    mask = Image.new("L", (big, big), 0)
    ImageDraw.Draw(mask).rounded_rectangle(
        [0, 0, big - 1, big - 1], radius=int(big * RADIUS), fill=255
    )
    tile.putalpha(mask)
    return tile.resize((size, size), Image.LANCZOS)


def main() -> None:
    rounded(256).save(HERE / "mark.png", "PNG", optimize=True)


if __name__ == "__main__":
    main()
