// The explorer tour video on the website, recorded from the demo mirror.
// Serve site/ (python3 -m http.server 8765 in site/), then
//   node tour.js <out dir>
//   ffmpeg -i <out dir>/*.webm -vf fps=30 -c:v libx264 -pix_fmt yuv420p -crf 26 -movflags +faststart explorer-tour.mp4
const { chromium } = require('playwright');
const B = process.env.BASE || 'http://127.0.0.1:8765/demo/all/explorer/';
(async () => {
  const out = process.argv[2] || '.';
  const b = await chromium.launch();
  const ctx = await b.newContext({ viewport: { width: 1440, height: 900 }, recordVideo: { dir: out, size: { width: 1440, height: 900 } } });
  await ctx.addInitScript(() => { try { localStorage.setItem('i2f.view', 'grid'); localStorage.setItem('i2f.appview.git', 'grid'); localStorage.setItem('i2f.appview.bsky', 'grid'); } catch (e) { /* */ } });
  const p = await ctx.newPage();
  const wait = (ms) => p.waitForTimeout(ms);
  const keys = async (ks, ms) => { for (const k of ks) { await p.keyboard.press(k); await wait(ms || 450); } };
  await p.goto(B + 'browse.html?p=ada/pins/recursive/bafybeif7eoxlm2szl5vbpwcjjv3zpl5i7wjvemndopi7lfglsworwbwgly&kind=image&deep=1');
  await wait(2500);
  await keys(['j', 'j', 'j', 'j', 'j'], 600);
  await keys(['v'], 1400);          // list
  await keys(['j', 'j', 'j'], 500);
  await keys(['v', 'v'], 1200);     // tree, columns
  await keys(['v'], 1000);          // back to list
  await p.goto(B + 'browse.html?app=bsky'); await wait(2600);
  await keys(['v'], 1800);          // accounts as a list
  await p.click('[data-sortcol="3"]'); await wait(1200);
  await p.click('tr.app-item >> nth=0'); await wait(3200);
  await p.mouse.wheel(0, 500); await wait(1500);
  await p.goto(B + 'browse.html?app=git'); await wait(2400);
  await keys(['v'], 1800);          // repos as a list
  await p.click('a.app-main >> nth=0'); await wait(2500);
  await p.click('text=Commits'); await wait(2200);
  await p.goto(B + 'browse.html?kind=site&deep=1'); await wait(2000);
  await keys(['j'], 3000);
  await keys(['j', 'j', 'j', 'j'], 700); await wait(2500);
  await ctx.close(); await b.close();
})();
