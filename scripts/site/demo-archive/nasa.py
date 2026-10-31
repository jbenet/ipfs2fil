# Public-domain NASA images and short videos from images-api.nasa.gov: nasa.py <content dir>
import json, os, re, sys, urllib.parse, urllib.request
UA = {'User-Agent': 'Mozilla/5.0 (ipfs2fil demo archive)'}
def get(url):
    return urllib.request.urlopen(urllib.request.Request(url, headers=UA), timeout=120).read()
out = sys.argv[1]
def search(q, media, n):
    r = json.loads(get('https://images-api.nasa.gov/search?' + urllib.parse.urlencode({'q': q, 'media_type': media})))
    items = [i for i in r['collection']['items'] if i['data'][0].get('center') != None or True]
    return items[:n]
def slug(s): return re.sub(r'[^a-z0-9]+', '-', s.lower()).strip('-')[:60]
credits = []
for q, folder, n in [('hubble galaxy', 'hubble', 14), ('webb telescope nebula', 'webb', 10), ('mars rover surface', 'mars', 12),
                     ('earth from space station', 'earth', 14), ('apollo moon surface', 'apollo', 12), ('saturn cassini', 'saturn', 8),
                     ('jupiter juno', 'jupiter', 10), ('aurora from space', 'aurora', 10), ('carina nebula', 'nebulae', 10)]:
    d = os.path.join(out, 'photos/nasa', folder); os.makedirs(d, exist_ok=True)
    for it in search(q, 'image', n):
        meta = it['data'][0]; nid = meta['nasa_id']
        try:
            links = json.loads(get(it['href'].replace(' ', '%20')))
            url = next((u for u in links if u.endswith('~large.jpg')), None) or next((u for u in links if u.endswith('~orig.jpg')), None)
            if not url: continue
            p = os.path.join(d, slug(meta.get('title', nid)) + '.jpg')
            if not os.path.exists(p): open(p, 'wb').write(get(url.replace('http://', 'https://')))
            credits.append(f"{folder}/{os.path.basename(p)}: {meta.get('title')} ({nid}), NASA, {meta.get('date_created','')[:10]}")
        except Exception as e: print('FAIL', nid, e)
d = os.path.join(out, 'video/nasa'); os.makedirs(d, exist_ok=True)
for q in ['earth timelapse space station', 'mars helicopter flight', 'apollo 11 launch']:
    for it in search(q, 'video', 3):
        meta = it['data'][0]; nid = meta['nasa_id']
        try:
            links = json.loads(get(it['href'].replace(' ', '%20')))
            url = next((u for u in links if u.endswith('~mobile.mp4')), None) or next((u for u in links if u.endswith('~small.mp4')), None)
            if not url: continue
            p = os.path.join(d, slug(meta.get('title', nid)) + '.mp4')
            if not os.path.exists(p):
                b = get(url.replace('http://', 'https://').replace(' ', '%20'))
                if len(b) > 60 << 20: continue  # long briefings
                open(p, 'wb').write(b)
            credits.append(f"video/{os.path.basename(p)}: {meta.get('title')} ({nid}), NASA")
        except Exception as e: print('FAIL', nid, e)
open(os.path.join(out, 'photos/nasa/CREDITS.txt'), 'w').write('NASA imagery is in the public domain (https://www.nasa.gov/nasa-brand-center/images-and-media/).\n\n' + '\n'.join(credits) + '\n')
print(len(credits), 'files')
