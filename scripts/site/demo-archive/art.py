# Generative images for the public demo archive (made here, CC0).
import colorsys, math, os, random, sys
import numpy as np
from PIL import Image, ImageDraw, ImageFilter
out = os.path.join(sys.argv[1], 'images'); rnd = random.Random(42)
def save(im, *p, q=90):
    path = os.path.join(out, *p); os.makedirs(os.path.dirname(path), exist_ok=True)
    im.save(path, quality=q) if path.endswith('.jpg') else im.save(path)
def palette(hue, n=256):
    return np.array([[int(255 * v) for v in colorsys.hls_to_rgb((hue + 0.4 * t) % 1, 0.08 + 0.75 * t ** 0.7, 0.75)] for t in np.linspace(0, 1, n)], np.uint8)
def escape(z, c, it):
    n = np.zeros(z.shape); alive = np.ones(z.shape, bool)
    for i in range(it):
        z[alive] = z[alive] ** 2 + (c[alive] if isinstance(c, np.ndarray) else c)
        esc = alive & (np.abs(z) > 4); n[esc] = i + 1 - np.log2(np.log(np.abs(z[esc]))); alive &= ~esc
    return n, alive
def frac(path, n, alive, hue):
    t = (n / max(n.max(), 1)) ** 0.5; img = palette(hue)[(t * 255).astype(int)]; img[alive] = 0
    save(Image.fromarray(img), *path)
W = H = 1400
# Mandelbrot zoom
spots = [(-0.743643887, 0.131825904), (-0.1011, 0.9563), (-1.25066, 0.02012), (0.001643721971153, -0.822467633298876), (-0.761574, -0.0847596)]
for k, (cx, cy) in enumerate(spots):
    for j, s in enumerate([3e-3, 3e-4, 3e-5]):
        y, x = np.mgrid[-1:1:H * 1j, -1:1:W * 1j]; c = (cx + x * s) + 1j * (cy + y * s)
        n, a = escape(np.zeros_like(c), c, 400); frac(('mandelbrot', f'spot-{k + 1}-zoom-{j + 1}.jpg'), n, a, k / 5)
# Julia sets
for k in range(12):
    ang = 2 * math.pi * k / 12; c = 0.7885 * complex(math.cos(ang), math.sin(ang))
    y, x = np.mgrid[-1.4:1.4:H * 1j, -1.4:1.4:W * 1j]; n, a = escape(x + 1j * y, c, 250); frac(('julia', f'julia-{k + 1:02}.jpg'), n, a, k / 12)
# Flow fields
for k in range(10):
    r = random.Random(k); im = Image.new('RGB', (1600, 1000), tuple(r.randint(5, 25) for _ in range(3))); d = ImageDraw.Draw(im)
    f1, f2, hue = r.uniform(0.002, 0.006), r.uniform(0.002, 0.006), r.random()
    for _ in range(2500):
        x, y = r.uniform(0, 1600), r.uniform(0, 1000); col = tuple(int(255 * v) for v in colorsys.hls_to_rgb((hue + r.random() * 0.15) % 1, 0.6, 0.7))
        pts = [(x, y)]
        for _ in range(60):
            a = math.sin(x * f1) * 2 + math.cos(y * f2) * 2; x += math.cos(a) * 3; y += math.sin(a) * 3; pts.append((x, y))
        d.line(pts, fill=col, width=1)
    save(im, 'flow-fields', f'flow-{k + 1:02}.png')
# Voronoi mosaics
for k in range(8):
    r = np.random.default_rng(k); pts = r.uniform(0, 1, (90, 2)) * [1600, 1000]; hue = k / 8
    y, x = np.mgrid[0:1000, 0:1600]; dist = (x[..., None] - pts[:, 0]) ** 2 + (y[..., None] - pts[:, 1]) ** 2
    idx = dist.argmin(-1); cols = np.array([[int(255 * v) for v in colorsys.hls_to_rgb((hue + r.uniform(0, .2)) % 1, r.uniform(.35, .75), .6)] for _ in pts], np.uint8)
    im = Image.fromarray(cols[idx]); edges = Image.fromarray((np.abs(np.diff(idx, axis=0, prepend=0)) + np.abs(np.diff(idx, axis=1, prepend=0)) > 0).astype(np.uint8) * 255)
    im.paste((15, 15, 20), mask=edges.filter(ImageFilter.MaxFilter(3))); save(im, 'voronoi', f'mosaic-{k + 1:02}.jpg')
# Truchet tiles (SVG)
for k in range(8):
    r = random.Random(100 + k); n = 24; s = 40; hue = k / 8
    fg = '#%02x%02x%02x' % tuple(int(255 * v) for v in colorsys.hls_to_rgb(hue, .6, .7))
    paths = []
    for i in range(n):
        for j in range(n):
            x, y = i * s, j * s
            if r.random() < .5: paths.append(f'M{x + s / 2},{y} A{s / 2},{s / 2} 0 0 1 {x},{y + s / 2} M{x + s},{y + s / 2} A{s / 2},{s / 2} 0 0 0 {x + s / 2},{y + s}')
            else: paths.append(f'M{x + s / 2},{y} A{s / 2},{s / 2} 0 0 0 {x + s},{y + s / 2} M{x},{y + s / 2} A{s / 2},{s / 2} 0 0 1 {x + s / 2},{y + s}')
    svg = f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {n * s} {n * s}" width="{n * s}" height="{n * s}"><rect width="100%" height="100%" fill="#14161b"/><path d="{" ".join(paths)}" fill="none" stroke="{fg}" stroke-width="6" stroke-linecap="round"/></svg>'
    p = os.path.join(out, 'truchet', f'truchet-{k + 1:02}.svg'); os.makedirs(os.path.dirname(p), exist_ok=True); open(p, 'w').write(svg)
# Pixel sprites (PNG, upscaled)
for k in range(24):
    r = random.Random(500 + k); g = np.zeros((10, 10, 3), np.uint8); hue = r.random()
    cols = [tuple(int(255 * v) for v in colorsys.hls_to_rgb((hue + d) % 1, l, .7)) for d, l in [(0, .55), (.08, .35), (.5, .7)]]
    for yy in range(10):
        for xx in range(5):
            if r.random() < .5 - abs(yy - 5) * .04:
                c = cols[r.randrange(3)]; g[yy, 2 + xx] = c; g[yy, 7 - xx] = c
    save(Image.fromarray(g).resize((320, 320), Image.NEAREST), 'sprites', f'sprite-{k + 1:02}.png')
# Animated GIFs: rotating spirograph
for k in range(4):
    frames = []; hue = k / 4
    for f in range(36):
        im = Image.new('RGB', (480, 480), (12, 14, 18)); d = ImageDraw.Draw(im); R, rr, p = 150, 52 + k * 9, 70; ph = f / 36 * 2 * math.pi
        pts = [(240 + (R - rr) * math.cos(t) + p * math.cos((R - rr) / rr * t + ph), 240 + (R - rr) * math.sin(t) - p * math.sin((R - rr) / rr * t + ph)) for t in np.linspace(0, 2 * math.pi * rr, 2400)]
        d.line(pts, fill=tuple(int(255 * v) for v in colorsys.hls_to_rgb(hue, .6, .8)), width=2); frames.append(im)
    p = os.path.join(out, 'animated', f'spirograph-{k + 1}.gif'); os.makedirs(os.path.dirname(p), exist_ok=True)
    frames[0].save(p, save_all=True, append_images=frames[1:], duration=60, loop=0)
print('ok')
