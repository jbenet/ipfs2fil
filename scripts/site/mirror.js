// Mirrors a running `ipfs2fil explore` into a static folder for the website's
// live demo: every explorer page and index file the pages ask for, and the
// files themselves (up to maxBytes each, video up to videoBytes, audio up to
// audioBytes, any JPEG or PNG up to 16 MiB) under all/_i2f/content/.
//
//   node scripts/site/mirror.js http://127.0.0.1:8484 site/demo [maxBytes] [videoBytes] [audioBytes]
//
// Needs playwright. Per-repo explorers read file contents from the combined
// one's copy (their _i2f/info points there).
const { chromium } = require('playwright');
const fs = require('fs'), path = require('path');
const [base, out, maxS = String(6 << 20), videoS = String(10 << 20), audioS = String(4 << 20)] = process.argv.slice(2);
const max = parseInt(maxS, 10), avMax = { video: parseInt(videoS, 10), audio: parseInt(audioS, 10) };
const save = (u, body) => {
  let p = new URL(u).pathname;
  if (p.endsWith('/')) p += 'index.html';
  const f = path.join(out, decodeURIComponent(p));
  fs.mkdirSync(path.dirname(f), { recursive: true });
  fs.writeFileSync(f, body);
};
(async () => {
  fs.rmSync(out, { recursive: true, force: true });
  const info = await (await fetch(base + '/all/_i2f/info')).json();
  const b = await chromium.launch(); const ctx = await b.newContext();
  const seen = new Set();
  ctx.on('response', async (r) => {
    const u = r.url().split('?')[0], req = r.request();
    if (!u.startsWith(base) || req.method() !== 'GET' || r.status() !== 200 || seen.has(u) || u.includes('/_i2f/')) return;
    seen.add(u);
    try { save(u, await r.body()); } catch (e) { /* redirects have no body */ }
  });
  const p = await ctx.newPage();
  // The combined index lists each repo and the link to its own explorer.
  const meta = await (await fetch(base + '/all/explorer/idx/meta.js')).text();
  const repos = [...new Set([...meta.matchAll(/"href":"\.\.\/\.\.\/([^/"]+)\/explorer\/"/g)].map((m) => m[1]))];
  const visit = async (u) => { await p.goto(base + u, { waitUntil: 'networkidle' }); await p.waitForTimeout(800); };
  // pin.html and search.html are only linked to, never visited first; they
  // load (and show their "no such root" note) without a query.
  const pages = ['index.html', 'browse.html', 'pieces.html', 'browse.html?deep=1', 'pin.html', 'search.html'];
  if (meta.includes('"accounts"')) pages.push('posts.html'); // Bluesky accounts
  for (const page of pages) {
    await visit('/all/explorer/' + page);
    for (const r of repos) await visit(`/${r}/explorer/${page}`);
  }
  await b.close();
  save(base + '/all/_i2f/info', JSON.stringify({ content: true }));
  // Per-repo explorers read file contents from the combined one's copy.
  for (const r of repos) save(`${base}/${r}/_i2f/info`, JSON.stringify({ content: '../../all/_i2f/content/', all: info.all ? '../../all/explorer/' : undefined }));
  // Every CID the index names; only files come back from content. Bluesky
  // likes, reposts, follows and the like are many tiny records that only
  // point at other accounts' posts, so they are left out.
  const cids = new Set(), skip = new Set(), kinds = new Map();
  const pointers = /\/app\.bsky\.(feed\.(like|repost)|graph\.(follow|block|listitem|verification))$/;
  const sdir = path.join(out, 'all/explorer/idx/s');
  for (const f of fs.existsSync(sdir) ? fs.readdirSync(sdir) : []) {
    const t = fs.readFileSync(path.join(sdir, f), 'utf8'), sh = JSON.parse(t.slice(t.indexOf(',') + 1, t.lastIndexOf(')')));
    for (const r of sh.rows) {
      if (pointers.test(sh.dirs[r[0]])) skip.add(r[4]);
      if (avMax[r[2]]) kinds.set(r[4], r[2]);
    }
  }
  const walk = (d) => { for (const f of fs.readdirSync(d)) { const q = path.join(d, f); if (fs.statSync(q).isDirectory()) walk(q); else for (const m of fs.readFileSync(q, 'utf8').matchAll(/\b(Qm[1-9A-HJ-NP-Za-km-z]{44}|baf[a-z2-7]{52,})\b/g)) if (!skip.has(m[1])) cids.add(m[1]); } };
  walk(path.join(out, 'all/explorer'));
  let n = 0, bytes = 0, skipped = 0;
  for (const c of cids) {
    const head = await fetch(`${base}/all/_i2f/content/${c}`, { method: 'HEAD' });
    if (!head.ok) continue;
    const len = parseInt(head.headers.get('content-length') || '0', 10);
    // Big JPEGs and PNGs come along anyway: shrink.py scales them down after.
    const img = /^image\/(jpeg|png)/.test(head.headers.get('content-type') || '') && len <= 16 << 20;
    // Video and audio (by the index's kind) get their own caps, so the demo plays them.
    const av = kinds.has(c) && len <= avMax[kinds.get(c)];
    if (len > max && !img && !av) { skipped++; continue; }
    const body = Buffer.from(await (await fetch(`${base}/all/_i2f/content/${c}`)).arrayBuffer());
    save(`${base}/all/_i2f/content/${c}`, body); n++; bytes += body.length;
  }
  fs.writeFileSync(path.join(out, 'index.html'), '<!doctype html><meta http-equiv="refresh" content="0; url=all/explorer/"><a href="all/explorer/">Open the explorer</a>\n');
  console.log(`pages ${seen.size}, files ${n} (${(bytes / 1048576).toFixed(1)} MiB), ${skipped} over ${max} bytes left out, repos ${repos.join(' ')}`);
})();
