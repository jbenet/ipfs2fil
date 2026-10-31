"""Writes the demo's notes, data and art texts into the current folder.

Run from the content folder after content.py; everything is made up.
"""
import csv, json, math, os, random

random.seed(11)
os.makedirs('notes', exist_ok=True)
os.makedirs('data', exist_ok=True)
os.makedirs('art-texts', exist_ok=True)

NOTES = {
    'reading-list.md': """# Reading list

- [x] *Flatland*, Edwin A. Abbott
- [x] *The Time Machine*, H. G. Wells
- [ ] *Moby-Dick*, Herman Melville (started on the ferry, page 212)
- [ ] *Little Brother*, Cory Doctorow
- [ ] RFC 8890, *The Internet is for End Users*

> Books are in `books/` as EPUB, the papers in `papers/`.
""",
    'packing-iceland.md': """# Packing: Iceland, June 2024

| Thing | Packed |
|---|---|
| Rain shell | yes |
| Wool layers (3) | yes |
| Camera + 2 batteries | yes |
| Spare SD card | **forgot** |
| Road atlas | yes |

Photos are pinned as `iceland-2024`. The trip report PDF is in `papers/`.
""",
    'ideas.md': """# Ideas

1. Print the fractal series at 50x50 cm.
2. A photo book of the desert trip, 24 pages.
3. Archive everything to Filecoin before the old laptop dies.
4. Keep the Bluesky posts with the photos: same pictures, same CIDs.

```sh
ipfs2fil up --all
```
""",
    'todo.txt': """- renew passport (expires March)
- back up the old laptop
- move the photo app off the SD card
- send Jo the desert photos
""",
    'recipes/sourdough.md': """# Sourdough

- 500 g flour (450 white, 50 rye)
- 375 g water
- 100 g starter
- 10 g salt

Mix, rest 30 min, add salt. Four folds 30 min apart. Shape, retard overnight
in the fridge. Bake at 250 C covered 20 min, then 230 C uncovered 25 min.
""",
}
for name, text in NOTES.items():
    os.makedirs(os.path.dirname('notes/' + name), exist_ok=True)
    open('notes/' + name, 'w').write(text)

# A year of made-up daily weather, mentioned in the Iceland trip report.
with open('data/weather-reykjavik-2024.csv', 'w', newline='') as f:
    w = csv.writer(f)
    w.writerow(['date', 'high_c', 'low_c', 'rain_mm', 'wind_ms'])
    for d in range(366):
        m = 1 + d * 12 // 366
        t = 5 - 6 * math.cos(2 * math.pi * (d - 15) / 366)
        w.writerow([f'2024-{m:02d}-{1 + d % 30:02d}', round(t + random.uniform(0, 4), 1),
                    round(t - random.uniform(2, 6), 1), round(max(0, random.gauss(2.2, 3)), 1),
                    round(abs(random.gauss(7, 3)), 1)])
json.dump({'trip': 'iceland-2024', 'days': 9, 'km': 1478,
           'stops': ['Vik', 'Hofn', 'Seydisfjordur', 'Myvatn', 'Akureyri', 'Snaefellsnes', 'Reykjavik']},
          open('data/trip.json', 'w'), indent=2)

# Texts that go into art.car beside the images.
open('art-texts/artist-statement.md', 'w').write("""# Escape-time

Every image here is a rule run many times. The Mandelbrot and Julia sets
iterate z = z^2 + c and color each point by how fast it escapes; the flow
fields push particles through a noise field; the Truchet tiles turn one arc
in four ways. The short video zooms into the seahorse valley.

The NASA video and photos are public domain. The Bluesky images came from
@bsky.app's posts: the same files, so the same CIDs, as in the Bluesky
archive and the photo app.
""")
open('art-texts/poems.txt', 'w').write("""ITERATION

begin with zero, square it, add c,
and see if what you get will stay or flee.
the ones that stay are black; the rest are bright
by how many steps they took to leave the light.


HASH

name the thing by what it is,
not where it sleeps tonight:
a copy anywhere is it,
and anyone can check it's right.
""")
json.dump([
    {'file': 'images/mandelbrot.png', 'title': 'Mandelbrot', 'year': 2024},
    {'file': 'images/seahorse-valley.png', 'title': 'Seahorse valley', 'year': 2024},
    {'file': 'images/elephants.png', 'title': 'Elephant valley', 'year': 2024},
    {'file': 'video/seahorse-zoom.mp4', 'title': 'Seahorse zoom', 'year': 2025},
], open('art-texts/catalog.json', 'w'), indent=2)
