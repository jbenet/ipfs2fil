"""Scales the big images in a mirrored demo down to keep the site small:
python3 shrink.py site/demo/all/_i2f/content

Images over 120 KB become 800 px wide (640 px for PNGs, which stay PNG).
Their bytes then no longer match their CIDs; the explorer doesn't check.
"""
import io, os, sys
from PIL import Image

d = sys.argv[1]
before = after = 0
for f in sorted(os.listdir(d)):
    p = os.path.join(d, f)
    n = os.path.getsize(p)
    if n <= 120_000:
        continue
    try:
        im = Image.open(p)
        fmt = im.format
    except Exception:
        continue
    if fmt not in ('JPEG', 'PNG') or getattr(im, 'is_animated', False):
        continue
    w = 640 if fmt == "PNG" else 800
    if im.width > w:
        im.thumbnail((w, w * 4))
    buf = io.BytesIO()
    if fmt == 'PNG':
        im.save(buf, 'PNG', optimize=True)
    else:
        im.convert('RGB').save(buf, 'JPEG', quality=80)
    m = buf.tell()
    if m >= n:
        continue
    open(p, 'wb').write(buf.getvalue())
    before += n
    after += m
print(f'images {before / 1048576:.1f} MiB -> {after / 1048576:.1f} MiB')
