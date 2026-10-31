"""Writes a made-up project's dev assets for dev-bundle.car:
python3 devassets.py <out dir> [font.ttf]

Code in several languages, CI and server logs, configs, data (CSV, SQLite,
SQL, OpenAPI), a notebook, SVG icons, a tiny wasm module, a coverage report
and a release tarball: things a developer would dump into a CAR.
"""
import io, json, os, random, sqlite3, struct, sys, tarfile, zipfile, gzip, shutil

out = sys.argv[1]
random.seed(5)
def w(path, text, mode='w'):
    p = os.path.join(out, path)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, mode) as f:
        f.write(text)

w('README.md', """# tidepool

Syncs sensor readings from tide gauges into one dashboard.

```sh
make dev       # api on :8080, web on :5173
make test
```

- `src/api` Go HTTP API
- `src/ingest` Python ingest worker
- `src/web` TypeScript dashboard
- `src/sim` Rust tide simulator
- `logs/` CI and server logs from the 1.4.0 release
""")
w('CHANGELOG.md', """# Changelog

## 1.4.0 (2025-03-14)
- Sync is about 3x faster: batches of 500 readings.
- Dashboard bundle 41% smaller.
- Dark mode.

## 1.3.2 (2025-02-02)
- Fix: gauges with no readings for a day showed as offline.

## 1.3.0 (2025-01-10)
- Tide predictions from the simulator.
""")
w('LICENSE', 'MIT License\n\nCopyright (c) 2025 tidepool authors\n\nPermission is hereby granted, free of charge, to any person obtaining a copy of this software, to deal in it without restriction.\n')
w('src/api/main.go', '''package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type Reading struct {
	Gauge string    `json:"gauge"`
	At    time.Time `json:"at"`
	Level float64   `json:"level_m"`
}

func main() {
	db := mustOpen("tidepool.db")
	http.HandleFunc("/api/readings", func(w http.ResponseWriter, r *http.Request) {
		rs, err := db.Latest(r.URL.Query().Get("gauge"), 500)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(rs)
	})
	log.Println("api listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
''')
w('src/api/store.go', '''package main

import "database/sql"

type Store struct{ db *sql.DB }

// Latest returns the newest n readings of a gauge, newest first.
func (s *Store) Latest(gauge string, n int) ([]Reading, error) {
	rows, err := s.db.Query(`SELECT gauge, at, level_m FROM readings
		WHERE gauge = ? ORDER BY at DESC LIMIT ?`, gauge, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reading
	for rows.Next() {
		var r Reading
		if err := rows.Scan(&r.Gauge, &r.At, &r.Level); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
''')
w('src/ingest/worker.py', '''"""Pulls readings from the gauges and writes them in batches."""
import time, sqlite3, requests

BATCH = 500

def fetch(gauge):
    r = requests.get(f"https://gauges.example/{gauge}/latest", timeout=10)
    r.raise_for_status()
    return r.json()["readings"]

def run(db, gauges):
    while True:
        batch = [x for g in gauges for x in fetch(g)]
        for i in range(0, len(batch), BATCH):
            db.executemany("INSERT OR IGNORE INTO readings VALUES (?,?,?)",
                           [(x["gauge"], x["at"], x["level_m"]) for x in batch[i:i + BATCH]])
            db.commit()
        time.sleep(60)

if __name__ == "__main__":
    run(sqlite3.connect("tidepool.db"), ["sf-01", "sf-02", "oak-07"])
''')
w('src/web/dashboard.ts', '''import { Chart } from "./chart";

type Reading = { gauge: string; at: string; level_m: number };

export async function load(gauge: string): Promise<Reading[]> {
  const res = await fetch(`/api/readings?gauge=${encodeURIComponent(gauge)}`);
  if (!res.ok) throw new Error(`api: ${res.status}`);
  return res.json();
}

export async function render(el: HTMLElement, gauge: string) {
  const rs = await load(gauge);
  const chart = new Chart(el, { dark: matchMedia("(prefers-color-scheme: dark)").matches });
  chart.line(rs.map((r) => [Date.parse(r.at), r.level_m]));
}
''')
w('src/web/index.html', '<!doctype html>\n<html lang="en">\n<meta charset="utf-8">\n<title>tidepool</title>\n<link rel="stylesheet" href="style.css">\n<body>\n  <header><img src="../../assets/icons/wave.svg" width="28"> tidepool</header>\n  <main id="chart"></main>\n  <script type="module">import { render } from "./dashboard.js"; render(document.getElementById("chart"), "sf-01");</script>\n</body>\n</html>\n')
w('src/web/style.css', ':root { --bg: #fff; --fg: #0b2540; --accent: #1e88e5 }\n@media (prefers-color-scheme: dark) { :root { --bg: #0b1622; --fg: #e3f2fd } }\nbody { margin: 0; font: 15px/1.5 system-ui; background: var(--bg); color: var(--fg) }\nheader { display: flex; gap: 8px; align-items: center; padding: 12px 20px; font-weight: 600 }\n#chart { height: 70vh; margin: 0 20px }\n')
w('src/sim/src/main.rs', '''//! Predicts tide levels from harmonic constituents.
use std::f64::consts::PI;

struct Constituent { amp: f64, speed_deg_h: f64, phase_deg: f64 }

fn level(cs: &[Constituent], hours: f64) -> f64 {
    cs.iter().map(|c| c.amp * ((c.speed_deg_h * hours - c.phase_deg) * PI / 180.0).cos()).sum()
}

fn main() {
    let cs = [
        Constituent { amp: 0.58, speed_deg_h: 28.984, phase_deg: 191.0 }, // M2
        Constituent { amp: 0.37, speed_deg_h: 15.041, phase_deg: 106.0 }, // K1
        Constituent { amp: 0.13, speed_deg_h: 30.000, phase_deg: 199.0 }, // S2
    ];
    for h in 0..48 {
        println!("{h:>3} {:+.3}", level(&cs, h as f64));
    }
}
''')
w('src/sim/Cargo.toml', '[package]\nname = "tidesim"\nversion = "0.3.1"\nedition = "2021"\n\n[dependencies]\n')
w('scripts/profile.py', 'import cProfile, pstats\nfrom src.ingest.worker import fetch\n\ncProfile.run("fetch(\'sf-01\')", "fetch.prof")\npstats.Stats("fetch.prof").sort_stats("cumtime").print_stats(15)\n')
w('Makefile', 'dev:\n\tgo run ./src/api & npm --prefix src/web run dev\n\ntest:\n\tgo test ./... && pytest src/ingest && cargo test --manifest-path src/sim/Cargo.toml\n')
w('config/docker-compose.yml', '''services:
  api:
    build: .
    ports: ["8080:8080"]
    volumes: ["./data:/data"]
    environment:
      TIDEPOOL_DB: /data/tidepool.db
  ingest:
    build: .
    command: python -m src.ingest.worker
    depends_on: [api]
  web:
    image: nginx:1.27
    volumes: ["./src/web:/usr/share/nginx/html:ro", "./config/nginx.conf:/etc/nginx/conf.d/default.conf:ro"]
    ports: ["5173:80"]
''')
w('config/nginx.conf', 'server {\n  listen 80;\n  location /api/ { proxy_pass http://api:8080; }\n  location / { root /usr/share/nginx/html; try_files $uri /index.html; }\n}\n')
w('config/ci.yml', 'name: ci\non: [push]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - run: make install\n      - run: make lint\n      - run: make test\n      - run: make build\n')
w('config/.env.example', 'TIDEPOOL_DB=./data/tidepool.db\nGAUGE_API=https://gauges.example\nLOG_LEVEL=info\n')
w('package.json', json.dumps({'name': 'tidepool-web', 'version': '1.4.0', 'private': True, 'type': 'module',
                              'scripts': {'dev': 'vite', 'build': 'vite build', 'test': 'vitest run'},
                              'devDependencies': {'typescript': '^5.6.0', 'vite': '^6.0.0', 'vitest': '^2.1.0'}}, indent=2) + '\n')
w('tsconfig.json', json.dumps({'compilerOptions': {'target': 'ES2022', 'module': 'ESNext', 'strict': True, 'outDir': 'dist'}}, indent=2) + '\n')

# Logs.
E = '\x1b['
w('logs/ci/01-install.log', ''.join(f'{E}36m[install]{E}0m added {p}\n' for p in ['typescript@5.6.3', 'vite@6.0.7', 'vitest@2.1.8', 'esbuild@0.24.2', 'rollup@4.29.1']) + 'added 214 packages in 38s\n')
w('logs/ci/02-lint.log', 'go vet ./...\nruff check src/ingest\nAll checks passed!\ntsc --noEmit\n')
tests = ['TestLatest', 'TestLatestEmpty', 'TestHandlerBadGauge', 'TestBatchInsert', 'TestDarkMode']
w('logs/ci/03-test.log', ''.join(f'=== RUN   {t}\n--- PASS: {t} (0.0{i}s)\n' for i, t in enumerate(tests)) +
  f'PASS\nok  \ttidepool/src/api\t0.412s\n{E}32m 14 passed{E}0m in 2.31s\n   Compiling tidesim v0.3.1\ntest result: ok. 6 passed; 0 failed\n')
w('logs/ci/04-build.log', 'vite v6.0.7 building for production...\n✓ 38 modules transformed.\ndist/index.html                0.46 kB\ndist/assets/index-4f2a9c.css   1.12 kB\ndist/assets/index-b81e07.js   48.73 kB │ gzip: 17.02 kB\n✓ built in 1.84s\n')
lines, t = [], 1710408413
paths = ['/api/readings?gauge=sf-01', '/api/readings?gauge=oak-07', '/', '/assets/index-b81e07.js', '/favicon.ico', '/api/readings?gauge=nope']
for i in range(4000):
    t += random.randint(0, 9)
    p = random.choice(paths)
    code = 404 if 'nope' in p or 'favicon' in p else (500 if random.random() < 0.004 else 200)
    lines.append(f'10.0.{random.randint(0, 9)}.{random.randint(2, 254)} - - [{t}] "GET {p} HTTP/1.1" {code} {random.randint(200, 52000)} "-" "Mozilla/5.0"\n')
access = ''.join(lines)
w('logs/server/access.log', access)
w('logs/server/access.log.1.gz', gzip.compress(access[:len(access) // 2].encode(), mtime=0), 'wb')
w('logs/server/app.jsonl', ''.join(json.dumps({'ts': 1710408413 + i * 61, 'level': random.choice(['info'] * 8 + ['warn', 'error']),
                                                 'msg': random.choice(['sync batch', 'gauge slow', 'db busy, retrying', 'sync batch']),
                                                 'gauge': random.choice(['sf-01', 'sf-02', 'oak-07']), 'n': random.randint(1, 500)}) + '\n' for i in range(600)))
w('logs/crash-2025-03-02.txt', '''panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x1 addr=0x18 pc=0x6f2a14]

goroutine 41 [running]:
main.(*Store).Latest(0x0, {0xc0000a6010, 0x5}, 0x1f4)
	/src/api/store.go:9 +0x34
main.main.func1({0x7a3e40, 0xc0001c2000}, 0xc0001b8000)
	/src/api/main.go:19 +0x7c
net/http.HandlerFunc.ServeHTTP(...)
	/usr/local/go/src/net/http/server.go:2220
''')

# Data.
rows = [(g, 1710400000 + i * 600, round(1.2 + 0.9 * random.random(), 3)) for g in ['sf-01', 'sf-02', 'oak-07'] for i in range(200)]
w('data/readings-sample.csv', 'gauge,at,level_m\n' + ''.join(f'{g},{a},{l}\n' for g, a, l in rows))
schema = 'CREATE TABLE gauges (id TEXT PRIMARY KEY, name TEXT, lat REAL, lon REAL);\nCREATE TABLE readings (gauge TEXT REFERENCES gauges(id), at INTEGER, level_m REAL, PRIMARY KEY (gauge, at));\n'
w('data/schema.sql', schema)
db = os.path.join(out, 'data/tidepool.db')
if os.path.exists(db):
    os.remove(db)
c = sqlite3.connect(db)
c.executescript(schema)
c.executemany('INSERT INTO gauges VALUES (?,?,?,?)', [('sf-01', 'Fort Point', 37.81, -122.47), ('sf-02', 'Crissy Field', 37.80, -122.45), ('oak-07', 'Oakland Pier', 37.80, -122.33)])
c.executemany('INSERT INTO readings VALUES (?,?,?)', rows)
c.commit(); c.close()
w('data/openapi.json', json.dumps({'openapi': '3.1.0', 'info': {'title': 'tidepool', 'version': '1.4.0'},
    'paths': {'/api/readings': {'get': {'parameters': [{'name': 'gauge', 'in': 'query', 'required': True, 'schema': {'type': 'string'}}],
              'responses': {'200': {'description': 'Newest readings first'}}}}}}, indent=2) + '\n')
w('reports/coverage.json', json.dumps({'total': 0.873, 'files': {'src/api/main.go': 0.81, 'src/api/store.go': 0.94, 'src/ingest/worker.py': 0.86, 'src/web/dashboard.ts': 0.88}}, indent=2) + '\n')
w('notebooks/tides.ipynb', json.dumps({'nbformat': 4, 'nbformat_minor': 5, 'metadata': {'kernelspec': {'name': 'python3', 'display_name': 'Python 3'}},
    'cells': [{'cell_type': 'markdown', 'metadata': {}, 'source': ['# Tide levels at Fort Point\n', 'From `data/readings-sample.csv`.']},
              {'cell_type': 'code', 'metadata': {}, 'execution_count': 1, 'source': ['import csv\n', 'rows = [r for r in csv.DictReader(open("data/readings-sample.csv")) if r["gauge"] == "sf-01"]\n', 'len(rows), max(float(r["level_m"]) for r in rows)'],
               'outputs': [{'output_type': 'execute_result', 'execution_count': 1, 'metadata': {}, 'data': {'text/plain': ['(200, 2.098)']}}]}]}, indent=1) + '\n')

# Assets.
for name, path in [('wave', 'M2 16c4-6 8-6 12 0s8 6 12 0 8-6 12 0'), ('gauge', 'M8 40a20 20 0 1 1 32 0M24 40l8-14'), ('sync', 'M10 20a14 14 0 0 1 26-6M38 28a14 14 0 0 1-26 6')]:
    w(f'assets/icons/{name}.svg', f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48" width="48" height="48"><path d="{path}" fill="none" stroke="#1e88e5" stroke-width="3" stroke-linecap="round"/></svg>\n')
# A wasm module exporting add(i32, i32) -> i32.
wasm = bytes.fromhex('0061736d0100000001070160027f7f017f030201000707010361646400000a09010700200020016a0b')
w('assets/add.wasm', wasm, 'wb')
if len(sys.argv) > 2:
    os.makedirs(os.path.join(out, 'assets/fonts'), exist_ok=True)
    shutil.copy(sys.argv[2], os.path.join(out, 'assets/fonts', os.path.basename(sys.argv[2])))

# The release tarball and a zip of the docs, made from the files above.
buf = io.BytesIO()
with tarfile.open(fileobj=buf, mode='w:gz') as tf:
    for d in ['src', 'config', 'README.md', 'CHANGELOG.md', 'LICENSE', 'package.json']:
        tf.add(os.path.join(out, d), arcname='tidepool-1.4.0/' + d)
w('dist/tidepool-1.4.0.tar.gz', buf.getvalue(), 'wb')
with zipfile.ZipFile(os.path.join(out, 'dist/docs.zip'), 'w') as z:
    for f in ['README.md', 'CHANGELOG.md', 'data/openapi.json']:
        z.write(os.path.join(out, f), f)
