#!/usr/bin/env python3
"""Normalise provider marks so they sit identically in the dashboard's frames.

The assets arrive from a dozen sources and disagree on everything that matters
visually: some carry a baked-in opaque square with their own rounded corners, others
are transparent; the mark occupies anywhere from 40% to 100% of the canvas. Framed
side by side, that reads as ragged even though every file is 128x128.

What this does NOT do is strip backgrounds. Polarity is not uniform — Codex is a white
mark on black, Cursor is a dark mark on white — so making the background transparent
would leave one of them invisible against the panel. Each mark keeps its own backdrop;
only geometry is normalised, and the frame that makes them match is CSS.

Run: python3 tools/normalize-provider-logos.py [--check]
"""
import sys
from pathlib import Path

from PIL import Image

ASSETS = Path(__file__).resolve().parent.parent / "internal/ui/assets/providers"
CANVAS = 128
# The fraction of the canvas the mark itself should occupy when it has a transparent
# background. Opaque marks keep their own edge-to-edge backdrop.
CONTENT = 0.78
# How far a pixel must differ from the corner colour to count as content.
TOLERANCE = 24


def corner_background(image):
    """Return the uniform background colour, or None when there is not one."""
    width, height = image.size
    corners = [
        image.getpixel((0, 0)),
        image.getpixel((width - 1, 0)),
        image.getpixel((0, height - 1)),
        image.getpixel((width - 1, height - 1)),
    ]
    if any(pixel[3] == 0 for pixel in corners):
        return None
    first = corners[0]
    for pixel in corners[1:]:
        if max(abs(a - b) for a, b in zip(first[:3], pixel[:3])) > TOLERANCE:
            return None
    return first


def content_box(image, background):
    """Bounding box of the mark, ignoring its backdrop."""
    if background is None:
        return image.getbbox()
    pixels = image.load()
    width, height = image.size
    left, top, right, bottom = width, height, 0, 0
    for y in range(height):
        for x in range(width):
            pixel = pixels[x, y]
            if pixel[3] == 0:
                continue
            if max(abs(a - b) for a, b in zip(pixel[:3], background[:3])) <= TOLERANCE:
                continue
            left, top = min(left, x), min(top, y)
            right, bottom = max(right, x + 1), max(bottom, y + 1)
    if right <= left or bottom <= top:
        return None
    return (left, top, right, bottom)


def normalise(path):
    image = Image.open(path).convert("RGBA")
    background = corner_background(image)
    box = content_box(image, background)
    if box is None:
        return image.resize((CANVAS, CANVAS), Image.LANCZOS), background

    mark = image.crop(box)
    if background is None:
        # Transparent marks are centred with a consistent margin, so a wide logo and a
        # tall one end up optically the same weight.
        target = int(CANVAS * CONTENT)
        scale = target / max(mark.size)
        size = (max(1, round(mark.width * scale)), max(1, round(mark.height * scale)))
        mark = mark.resize(size, Image.LANCZOS)
        canvas = Image.new("RGBA", (CANVAS, CANVAS), (0, 0, 0, 0))
        canvas.paste(mark, ((CANVAS - size[0]) // 2, (CANVAS - size[1]) // 2), mark)
        return canvas, background

    # An opaque mark is left alone. Its backdrop is part of the design — Codex is white
    # on black, Cursor is a light cube on a dark badge — and re-padding it only adds a
    # second margin inside the frame. The CSS frame clips it to a common shape instead,
    # which is the one thing that makes marks of different origins line up.
    if image.size != (CANVAS, CANVAS):
        image = image.resize((CANVAS, CANVAS), Image.LANCZOS)
    return image, background


def main():
    check = "--check" in sys.argv
    problems = []
    for path in sorted(ASSETS.glob("*.png")):
        original = Image.open(path).convert("RGBA")
        result, background = normalise(path)
        kind = "opaque" if background else "transparent"
        if check:
            if original.size != (CANVAS, CANVAS):
                problems.append(f"{path.name}: {original.size} is not {CANVAS}x{CANVAS}")
            continue
        result.save(path)
        print(f"  {path.name:<20} {kind:<12} -> {CANVAS}x{CANVAS}")
    if problems:
        print("\n".join(problems))
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
