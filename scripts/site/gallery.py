"""Turns the PNGs shots.js wrote into the website's gallery.

python3 gallery.py <png dir>

Writes site/media/gallery/<id>.webp (at 1.5x, 2160 wide) and
site/gallery-rows.js: where each sidebar row is in the shots (CSS pixels of
the 1440-wide page), so hovering at that height shows that row's shot.
"""
import json, os, sys
from PIL import Image

src = sys.argv[1]
site = os.path.join(os.path.dirname(__file__), '..', '..', 'site')
out = os.path.join(site, 'media', 'gallery')
os.makedirs(out, exist_ok=True)
for f in sorted(os.listdir(src)):
    if f.endswith('.png'):
        im = Image.open(os.path.join(src, f)).convert('RGB')
        im = im.resize((im.width * 3 // 4, im.height * 3 // 4), Image.LANCZOS)
        im.save(os.path.join(out, f[:-4] + '.webp'), 'WEBP', quality=74, method=6)
# sidebar rows -> shot ids, in order (Smart's "Largest files" has no shot)
ids = ['everything', 'photos', 'videos', 'audio', 'posts', 'documents', 'sites', 'pages', 'text', 'code',
       'records', 'accounts', 'repositories', 'archives', 'other', 'bluesky', 'git', 'repos']
rows = [r for r in json.load(open(os.path.join(src, 'rows.json'))) if 'sort=size' not in r['href']]
res = []
for i, r in enumerate(rows):
    sid = ids[i] if i < len(ids) else r['name']
    res.append({'id': sid, 'name': r['name'], 'top': r['top'], 'h': r['height']})
with open(os.path.join(site, 'gallery-rows.js'), 'w') as fh:
    fh.write('// Made by scripts/site/gallery.py from the shots: where each sidebar row is.\n')
    fh.write('window.GALLERY_ROWS = ' + json.dumps(res) + ';\n')
