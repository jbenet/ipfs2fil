const { chromium } = require('playwright');
// Explorer screenshots for the website, from the demo mirror (site/demo).
// Serve site/ (python3 -m http.server 8765 in site/), then
//   node shots.js <out dir> [names...]
// and convert the PNGs to webp for site/media/explorer-<name>.webp.
const B = process.env.BASE || 'http://127.0.0.1:8765/demo/all/explorer/';
const shots = [
  ['photos', 'browse.html?p=ada/pins/recursive/bafybeif7eoxlm2szl5vbpwcjjv3zpl5i7wjvemndopi7lfglsworwbwgly&kind=image&deep=1', async (p) => { await p.evaluate(() => localStorage.setItem('i2f.view','grid')); await p.reload(); await p.waitForTimeout(2500); for (const k of 'jjjj') await p.keyboard.press(k); }],
  ['keys', 'browse.html?kind=image&deep=1', async (p) => { await p.keyboard.press('?'); }],
  ['pdf', 'browse.html?kind=doc&deep=1', async (p) => { await p.evaluate(() => localStorage.setItem('i2f.view','list')); await p.reload(); await p.waitForTimeout(2500); for (const k of 'jjjjj') await p.keyboard.press(k); }],
  ['video', 'browse.html?kind=video&deep=1', async (p) => { await p.keyboard.press('j'); }],
  ['overview', 'index.html'],
  ['pieces', 'pieces.html'],
  ['search', 'browse.html?q=apollo', async (p) => { await p.focus('#fq'); await p.keyboard.press('ArrowDown'); for (const k of 'jjjjjjj') await p.keyboard.press(k); }],
  ['bsky', 'browse.html?app=bsky&a=0'],
  ['lists', 'browse.html?app=bsky', async (p) => { await p.click('[data-appview="list"]'); }],
  ['git', 'browse.html?app=git&path=ipfs/go-cid'],
  ['sites', 'browse.html?kind=site&deep=1', async (p) => { await p.keyboard.press('j'); }],
];
(async () => {
  const b = await chromium.launch(); const ctx = await b.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2 }); const p = await ctx.newPage();
  const out = process.argv[2] || '.', only = process.argv.slice(3);
  for (const [n, u, f] of shots) {
    if (only.length && !only.includes(n)) continue;
    await p.goto(B + u); await p.waitForTimeout(2500);
    if (f) { await f(p); await p.waitForTimeout(2500); }
    await p.screenshot({ path: `${out}/explorer-${n}.png` });
  }
  await b.close();
})();
