#!/usr/bin/env python3
"""Find Ubuntu's green "Install" button in a QEMU screendump.

The Desktop installer stops at "Ready to install — Review your choices" and
waits for Install. vm.sh uses this to know when that screen is up, so it can
press the button then rather than after a fixed number of minutes: how long the
review screen takes to appear depends on the machine, and a guess is either a
run that presses nothing or minutes of waiting for nothing.

Rather than hard-coding where the button was on the day this was written, it is
found by what makes it the button: a solid block of the suggested-action green,
wide enough to be a button, in the lower part of the screen.

Exit status is the answer: 0 when that button is on screen, 1 when it is not,
which is how the caller knows to look again at the next screenshot. The centre
of the button is printed as QEMU absolute-pointer coordinates (0..32767, what
`mouse_move` wants from a usb-tablet) for a caller that would rather click it
than reach it with the keyboard.

    findbutton.py <screendump.ppm>
"""
import sys
from collections import Counter

QEMU_AXIS_MAX = 32767
MIN_W, MIN_H = 60, 20


def read_ppm(path):
    with open(path, "rb") as f:
        if f.readline().strip() != b"P6":
            raise SystemExit("not a binary PPM")
        line = f.readline()
        while line.startswith(b"#"):
            line = f.readline()
        w, h = map(int, line.split())
        f.readline()  # maxval
        return w, h, f.read(w * h * 3)


def main():
    if len(sys.argv) != 2:
        raise SystemExit(__doc__)
    w, h, data = read_ppm(sys.argv[1])

    def px(x, y):
        i = (y * w + x) * 3
        return data[i], data[i + 1], data[i + 2]

    # A suggested-action button is a large area of one saturated green. Only
    # the bottom half is searched: that is where a dialog puts its buttons,
    # and it keeps the wallpaper out of it.
    counts = Counter()
    for y in range(h // 2, h):
        for x in range(w):
            c = px(x, y)
            r, g, b = c
            if g > 100 and g - r > 40 and g - b > 40:
                counts[c] += 1
    if not counts:
        return 1
    colour, n = counts.most_common(1)[0]
    if n < MIN_W * MIN_H:
        return 1

    pts = [(x, y) for y in range(h // 2, h) for x in range(w) if px(x, y) == colour]
    if not pts:
        return 1
    xs = [p[0] for p in pts]
    ys = [p[1] for p in pts]
    x0, x1, y0, y1 = min(xs), max(xs), min(ys), max(ys)
    if x1 - x0 < MIN_W or y1 - y0 < MIN_H:
        return 1

    # A bounding box is not a button. Two separate things sharing the accent
    # colour — a button and a highlight somewhere else in the bottom half —
    # have a box spanning both, whose centre is the empty space between them,
    # and pressing there does nothing. A button fills its own box; this does
    # not, so require most of the box to actually be that colour.
    box = (x1 - x0 + 1) * (y1 - y0 + 1)
    if len(pts) < 0.6 * box:
        return 1

    cx, cy = (x0 + x1) // 2, (y0 + y1) // 2
    print(round(cx * QEMU_AXIS_MAX / (w - 1)), round(cy * QEMU_AXIS_MAX / (h - 1)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
