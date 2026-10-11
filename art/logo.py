#!/usr/bin/env python3
"""Generates the logo files of dymploy (logo.svg, mark.svg, avatar.svg) and, with ImageMagick, the PNGs.

Same visual family as the other Dymmer marks: the blue of Dymmer, a rounded diamond (a 500 px square
rotated 45 degrees, corner radius 82) on a black squircle, and the symbol cut out of the diamond in the
colour of the background. The symbol is a rocket, pointing up and to the right.

    python3 logo.py        # writes the SVGs next to this file and, if `magick` is there, the PNGs
"""
import math, os, shutil, subprocess, tempfile

BLUE, BLACK = "#5F82AD", "#000000"
C = 512
HALF = 250
DIAG = HALF * math.sqrt(2)
RADIUS = 82
SQUIRCLE = ("M923.347,288.083c8.858,-100.22 -75.686,-184.628 -187.957,-188.083l-447.433,-0c-105.078,-1.342 "
            "-189.28,82.953 -187.957,188.083l-0,447.732c0.604,106.676 84.886,191.244 187.957,188.083l447.433,0"
            "c105.547,2.157 189.892,-81.99 187.957,-188.083l0,-447.732Z")


def diamond():
    v = [(C, C - DIAG), (C + DIAG, C), (C, C + DIAG), (C - DIAG, C)]

    def toward(a, b, d):
        dx, dy = b[0] - a[0], b[1] - a[1]
        n = math.hypot(dx, dy)
        return (a[0] + dx / n * d, a[1] + dy / n * d)

    corners = [(toward(p, v[i - 1], RADIUS), p, toward(p, v[(i + 1) % 4], RADIUS)) for i, p in enumerate(v)]
    d = "M %.2f %.2f " % corners[0][0]
    for i, (t1, p, t2) in enumerate(corners):
        if i:
            d += "L %.2f %.2f " % t1
        d += "Q %.2f %.2f %.2f %.2f " % (p[0], p[1], t2[0], t2[1])
    return d + "Z"


def poly(pts):
    return "M " + " L ".join("%.1f %.1f" % p for p in pts) + " Z"


def bezier(p0, p1, p2, p3, n=24):
    out = []
    for i in range(1, n + 1):
        t = i / n
        u = 1 - t
        out.append((u**3 * p0[0] + 3 * u * u * t * p1[0] + 3 * u * t * t * p2[0] + t**3 * p3[0],
                    u**3 * p0[1] + 3 * u * u * t * p1[1] + 3 * u * t * t * p2[1] + t**3 * p3[1]))
    return out


def circle(cx, cy, r, n=48):
    return [(cx + r * math.cos(2 * math.pi * i / n), cy + r * math.sin(2 * math.pi * i / n)) for i in range(n)]


def rocket(scale=0.93, angle=45):
    """The parts of the rocket, as polygons, pointing up in its own coordinates and then turned."""
    right = [(0, -200)] + bezier((0, -200), (60, -150), (78, -70), (70, 30)) + bezier((70, 30), (66, 80), (52, 110), (46, 130))
    body = right + [(-x, y) for x, y in reversed(right)]
    fin_r = [(60, 40), (122, 118), (122, 168), (46, 128)]
    fin_l = [(-x, y) for x, y in reversed(fin_r)]
    nozzle = [(-34, 130), (34, 130), (26, 150), (-26, 150)]
    flame = [(-30, 166)] + bezier((-30, 166), (-28, 205), (-8, 235), (0, 262)) + bezier((0, 262), (8, 235), (28, 205), (30, 166))
    window = circle(0, -62, 30)
    a = math.radians(angle)
    ca, sa = math.cos(a), math.sin(a)

    def place(pts):  # centre the rocket's own extent (y from -200 to 262) on the middle, then turn it
        return [(C + x * scale * ca - (y - 29) * scale * sa, C + x * scale * sa + (y - 29) * scale * ca) for x, y in pts]

    solid = [place(p) for p in (fin_r, fin_l, body, nozzle, flame)]
    return solid, place(window)


def header():
    return '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024" width="1024" height="1024">\n'


def write(name, text):
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), name), "w") as f:
        f.write(text)


def main():
    d = diamond()
    solid, window = rocket()
    black = "".join('  <path d="%s" fill="%s"/>\n' % (poly(p), BLACK) for p in solid)
    win = '  <path d="%s" fill="%s"/>\n' % (poly(window), BLUE)
    drawing = '  <path d="%s" fill="%s"/>\n' % (d, BLUE) + black + win
    write("logo.svg", header() + '  <path d="%s" fill="%s"/>\n' % (SQUIRCLE, BLACK) + drawing + "</svg>\n")
    write("avatar.svg", header() + '  <rect width="1024" height="1024" fill="%s"/>\n' % BLACK + drawing + "</svg>\n")
    # the mark alone: the diamond with the rocket cut out (a mask, so the rocket is a real hole)
    write("mark.svg", header() + '  <mask id="hole"><rect width="1024" height="1024" fill="#fff"/>\n'
          + "".join('    <path d="%s" fill="#000"/>\n' % poly(p) for p in solid)
          + '    <path d="%s" fill="#fff"/>\n  </mask>\n' % poly(window)
          + '  <path d="%s" fill="%s" mask="url(#hole)"/>\n</svg>\n' % (d, BLUE))
    if shutil.which("magick"):
        here = os.path.dirname(os.path.abspath(__file__))
        run = lambda *a: subprocess.run(a, check=True, cwd=here)
        run("magick", "-background", "none", "-density", "144", "logo.svg", "-resize", "1024x1024", "logo-1024.png")
        run("magick", "-background", "none", "-density", "144", "avatar.svg", "-resize", "1024x1024", "-resize", "512x512", "app-logo-512.png")
        # the mark: ImageMagick's own renderer has no masks, so build the hole from two renderings
        with tempfile.TemporaryDirectory() as t:
            def svg(name, body):
                open(os.path.join(t, name), "w").write(header() + body + "</svg>\n")
            svg("diamond.svg", '  <path d="%s" fill="%s"/>\n' % (d, BLUE))
            svg("hole.svg", '  <rect width="1024" height="1024" fill="#000"/>\n'
                + "".join('  <path d="%s" fill="#fff"/>\n' % poly(p) for p in solid)
                + '  <path d="%s" fill="#000"/>\n' % poly(window))
            for n in ("diamond", "hole"):
                subprocess.run(["magick", "-background", "none", "-density", "144", n + ".svg", "-resize", "1024x1024", n + ".png"], check=True, cwd=t)
            subprocess.run(["magick", "diamond.png", "-alpha", "extract", "da.png"], check=True, cwd=t)
            subprocess.run(["magick", "hole.png", "-alpha", "off", "-negate", "keep.png"], check=True, cwd=t)
            subprocess.run(["magick", "da.png", "keep.png", "-compose", "Multiply", "-composite", "alpha.png"], check=True, cwd=t)
            subprocess.run(["magick", "diamond.png", "-alpha", "off", "alpha.png", "-compose", "CopyOpacity", "-composite",
                            os.path.join(here, "mark-1024.png")], check=True, cwd=t)


if __name__ == "__main__":
    main()
