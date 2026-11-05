// The explorer screenshots on the website, from the demo mirror (site/demo):
// one per sidebar entry (Library, Viewers, Archive repos) and one per gallery
// pill, plus where each sidebar row sits, for the page's hover.
//
// Serve site/ (python3 -m http.server 8765 in site/), then
//   node shots.js <png dir> [ids...]
// which writes <png dir>/<id>.png (1440x1140, device scale 2) and
// <png dir>/rows.json. scripts/site/gallery.py turns them into site/media/gallery/.
const { chromium } = require('playwright');
const fs = require('fs');
const B = process.env.BASE || 'http://127.0.0.1:8765/demo/all/explorer/';
const W = 1440, H = 1140;

(async () => {
  const out = process.argv[2] || '.', only = process.argv.slice(3);
  const b = await chromium.launch();
  const ctx = await b.newContext({ viewport: { width: W, height: H }, deviceScaleFactor: 2 });
  const p = await ctx.newPage();
  const wait = (ms) => p.waitForTimeout(ms);
  const view = async (v) => { await p.evaluate((v) => { localStorage.setItem('i2f.view', v); localStorage.setItem('i2f.layout', 'split'); }, v); };
  // The path of the first search result for q (to select it by URL).
  const pathOf = async (q) => {
    await p.goto(B + 'browse.html?q=' + encodeURIComponent(q)); await wait(1800);
    await p.focus('#fq'); await p.keyboard.press('ArrowDown'); await wait(800);
    return new URL(p.url()).searchParams.get('sel');
  };
  await p.goto(B + 'browse.html'); await wait(1500);
  const sel = {
    earthrise: await pathOf('earth-rise-as-photographed'),
    apollo: await pathOf('apollo-11-crewmembers-participate'),
    synth: await pathOf('01-pieces-in-motion'),
    seahorse: await pathOf('seahorse-zoom.mp4'),
    flatland: await pathOf('flatland.epub'),
    worker: await pathOf('worker.py'),
    tidepool: await pathOf('tidepool-1.4.0.tar.gz'),
    docszip: await pathOf('docs.zip'),
    changelog: await pathOf('CHANGELOG.md')
  };
  const q = (o) => 'browse.html?' + new URLSearchParams(o).toString();
  const keys = async (ks) => { for (const k of ks) { await p.keyboard.press(k); await wait(250); } };

  // id, url, view (list/grid), then what to do before the shot
  const shots = [
    // Library
    ['everything', q({ deep: 1, sel: sel.apollo }), 'list'],
    ['photos', q({ kind: 'image', deep: 1, sort: 'size', dir: '-1', sel: sel.earthrise }), 'grid'],
    ['videos', q({ kind: 'video', deep: 1, sel: sel.seahorse }), 'grid'],
    ['audio', q({ kind: 'audio', deep: 1, sel: sel.synth }), 'list'],
    ['posts', q({ kind: 'post', deep: 1 }), 'grid', () => keys('j')],
    ['documents', q({ kind: 'doc', deep: 1, sel: sel.flatland }), 'list'],
    ['sites', q({ kind: 'site', deep: 1 }), 'list', async () => { await keys('j'); await wait(4000); }],
    ['pages', q({ kind: 'page', deep: 1 }), 'list', () => keys('jjj')],
    ['text', q({ kind: 'text', deep: 1, sel: sel.changelog }), 'list'],
    ['code', q({ kind: 'code', deep: 1, sel: sel.worker }), 'list'],
    ['records', q({ kind: 'record', deep: 1 }), 'list', () => keys('jjjj')],
    ['accounts', q({ kind: 'account', deep: 1 }), 'list', () => keys('j')],
    ['repositories', q({ kind: 'repo', deep: 1 }), 'list', () => keys('j')],
    ['archives', q({ kind: 'archive', deep: 1, sel: sel.tidepool }), 'list'],
    ['other', q({ kind: 'other', deep: 1 }), 'list', () => keys('j')],
    // Viewers
    ['bluesky', 'browse.html?app=bsky&a=0', 'grid'],
    ['git', 'browse.html?app=git&path=ipfs/go-cid', 'grid'],
    // Archive repos
    ['repos', 'browse.html', 'list', () => keys('j')],
    ['ada', q({ p: 'ada' }), 'list', () => keys('jj')],
    ['old-laptop', q({ p: 'old-laptop' }), 'list', () => keys('j')],
    ['bsky-bsky.app', q({ p: 'bsky-bsky.app' }), 'list', () => keys('j')],
    ['js-ipfs', q({ p: 'js-ipfs' }), 'list', () => keys('j')],
    ['helia-photo-app', q({ p: 'helia-photo-app' }), 'list', () => keys('j')],
    ['art.car', q({ p: 'art.car' }), 'list', () => keys('j')],
    ['dev-bundle.car', q({ p: 'dev-bundle.car' }), 'list', () => keys('j')],
    ['multiformats', q({ p: 'multiformats' }), 'list', () => keys('j')],
    ['go-cid', q({ p: 'go-cid' }), 'list', () => keys('j')],
    ['pds', q({ p: 'pds' }), 'list', () => keys('j')],
    ['sites-ipfs-websites', q({ p: 'sites-ipfs-websites' }), 'list', () => keys('j')],
    ['pinning-service', q({ p: 'pinning-service' }), 'list', () => keys('j')],
    // the other pills
    ['overview', 'index.html', 'list'],
    ['pieces', 'pieces.html', 'list'],
    ['zip', q({ p: sel.docszip }), 'list', () => keys('jjj')],
    ['webapps', q({ kind: 'site', deep: 1 }), 'list', async () => { await keys('jjjjjj'); await wait(5000); await p.mouse.click(1100, 600); for (let i = 0; i < 5; i++) { await wait(300); await p.mouse.click(1100, 600); } }],
    ['lists', 'browse.html?app=bsky', 'grid', async () => { await p.click('[data-appview="list"]'); }],
    ['search', q({ q: 'apollo' }), 'list', async () => { await p.focus('#fq'); await p.keyboard.press('ArrowDown'); await keys('jjjjjj'); }],
    ['shortcuts', q({ kind: 'image', deep: 1 }), 'grid', () => keys('?')]
  ];
  let rows = null;
  for (const [id, url, v, f] of shots) {
    if (only.length && !only.includes(id)) continue;
    await view(v);
    await p.goto(B + url); await wait(2600);
    if (f) { await f(); await wait(2200); }
    if (!rows && url.startsWith('browse.html?kind=image')) {
      rows = await p.evaluate(() => Array.from(document.querySelectorAll('.lib a')).map((a) => { const r = a.getBoundingClientRect(); return { href: a.getAttribute('href'), name: a.textContent.replace(/[\d.]+\s?[KM]?$/, '').trim(), top: Math.round(r.top), height: Math.round(r.height) }; }));
    }
    await p.screenshot({ path: `${out}/${id}.png` });
    console.log(id);
  }
  if (rows) fs.writeFileSync(`${out}/rows.json`, JSON.stringify(rows, null, 1));
  await b.close();
})();
