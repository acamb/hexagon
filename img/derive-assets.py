#!/usr/bin/env python3
"""Derive every logo asset the README and the frontend use from the two originals.

The artwork is navy (#003257) and green on transparency: it was drawn for a light
background, and on a dark one it all but disappears. Rather than ask a designer for
a second set, each dark variant is computed here — hue is left alone, saturation is
pulled back and value is lifted, so the mark keeps its identity and gains contrast.

The crops and sizes are here too, so a new original only has to be dropped into this
directory and the script re-run. Needs Pillow; it is not part of any build.
"""

import colorsys
from pathlib import Path

from PIL import Image

HERE = Path(__file__).resolve().parent
PUBLIC = HERE.parent / "web" / "public"

# The wordmark sits below a band of empty rows in the logo; everything above it is
# the hexagon cluster, which is what works as a favicon and beside the navigation.
MARK_HEIGHT = 305


def for_dark(image: Image.Image) -> Image.Image:
    """Return the image recoloured for a dark background."""
    out = image.copy()
    pixels = out.load()
    width, height = out.size
    for y in range(height):
        for x in range(width):
            r, g, b, a = pixels[x, y]
            if a == 0:
                continue
            h, s, v = colorsys.rgb_to_hsv(r / 255, g / 255, b / 255)
            r, g, b = colorsys.hsv_to_rgb(h, s * 0.55, 0.66 + 0.34 * v)
            pixels[x, y] = (round(r * 255), round(g * 255), round(b * 255), a)
    return out


def trimmed(image: Image.Image) -> Image.Image:
    """Crop away fully transparent margins, so a size is a size of the artwork."""
    box = image.getchannel("A").point(lambda v: 255 if v > 8 else 0).getbbox()
    return image.crop(box) if box else image


def scaled(image: Image.Image, width: int) -> Image.Image:
    height = round(image.height * width / image.width)
    return image.resize((width, height), Image.LANCZOS)


def squared(image: Image.Image) -> Image.Image:
    """Centre the artwork on a transparent square, which is what a favicon has to be."""
    side = max(image.size)
    canvas = Image.new("RGBA", (side, side), (0, 0, 0, 0))
    canvas.paste(image, ((side - image.width) // 2, (side - image.height) // 2))
    return canvas


def save(image: Image.Image, path: Path) -> None:
    # A palette of 128 colours is indistinguishable from the original here — the
    # artwork is flat fills and one gradient — and a quarter of the bytes, which
    # matters because the web/public copies are compiled into the binary.
    image.quantize(colors=128, method=Image.FASTOCTREE).save(path, optimize=True)
    print(f"{path.relative_to(HERE.parent)}  {image.width}x{image.height}")


def main() -> None:
    logo = trimmed(Image.open(HERE / "hexagon-logo-alpha.png").convert("RGBA"))
    banner = trimmed(Image.open(HERE / "hexagon-banner-alpha.png").convert("RGBA"))
    mark = trimmed(logo.crop((0, 0, logo.width, MARK_HEIGHT)))

    # The README is read on github.com, which honours <picture> and a dark banner.
    save(for_dark(banner), HERE / "hexagon-banner-dark.png")

    # Twice the size each is displayed at, which is all a high-density screen asks
    # for and keeps the binary from carrying a megabyte of artwork.
    save(scaled(logo, 420), PUBLIC / "hexagon-logo.png")
    save(scaled(for_dark(logo), 420), PUBLIC / "hexagon-logo-dark.png")
    save(scaled(mark, 64), PUBLIC / "hexagon-mark.png")
    save(scaled(for_dark(mark), 64), PUBLIC / "hexagon-mark-dark.png")

    # A tab strip is light or dark depending on the browser theme, and index.html
    # picks between these two the same way the pages do.
    save(squared(scaled(mark, 96)), PUBLIC / "favicon.png")
    save(squared(scaled(for_dark(mark), 96)), PUBLIC / "favicon-dark.png")


if __name__ == "__main__":
    main()
