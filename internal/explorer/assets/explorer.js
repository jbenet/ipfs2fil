// ipfs2fil explorer: every page of the static explorer. Plain JavaScript, no
// libraries, nothing fetched from outside the archive. The index format is
// described in internal/explorer/index.go.
'use strict';
(() => {
  const D = document;
  const PAGE = D.body.dataset.page;
  const params = new URLSearchParams(location.search);

  // ---- Loading the index. Each idx/*.js file calls I2F.put(key, value).
  const got = {}, waits = {};
  window.I2F = {
    put(key, v) {
      got[key] = v;
      (waits[key] || []).forEach((w) => w[0](v));
      delete waits[key];
    }
  };
  function load(key) {
    if (key in got) return Promise.resolve(got[key]);
    return new Promise((res, rej) => {
      if (waits[key]) return void waits[key].push([res, rej]);
      waits[key] = [[res, rej]];
      const s = D.createElement('script');
      // ?f= keeps a browser from using an index file it cached from an
      // older explorer at the same path.
      s.src = 'idx/' + key + '.js?f=2';
      s.onerror = () => {
        const w = waits[key] || [];
        delete waits[key];
        w.forEach((f) => f[1](new Error('Could not load idx/' + key + '.js. The archive may be incomplete.')));
      };
      D.head.appendChild(s);
    });
  }
  // From a gateway the newest manifest is index/manifest.json next to us;
  // from file:// only script tags work, so fall back to the copy in idx/.
  let manifestP;
  function manifest() {
    return manifestP || (manifestP = (async () => {
      if (/^https?:$/.test(location.protocol)) {
        try {
          const r = await fetch('../index/manifest.json');
          if (r.ok) return await r.json();
        } catch (e) { /* fall through */ }
      }
      return load('manifest');
    })());
  }
  // bucketOf must match bucketOf in index.go: FNV-1a 32 of the UTF-8 path.
  function bucketOf(p, n) {
    let h = 0x811c9dc5;
    for (const b of new TextEncoder().encode(p)) h = Math.imul(h ^ b, 0x01000193) >>> 0;
    return h % n;
  }
  async function dirInfo(p) {
    const meta = await load('meta');
    const b = await load('d/' + bucketOf(p, meta.buckets));
    return b[p] || inArchive(p);
  }
  // A folder inside a .tar, .tar.gz or zip, read in the browser (arc in
  // viewers.js); null when p is not in one.
  async function inArchive(p) {
    const A = window.I2FV && window.I2FV.arc;
    if (!A || !p.split('/').some(A.type)) return null;
    const entry = async (q) => {
      const d = q.includes('/') ? q.slice(0, q.lastIndexOf('/')) : '';
      const di = await dirInfo(d);
      return di ? (await listDir(di)).find((x) => x.path === q) || null : null;
    };
    try {
      return await A.dir(p, { entry, url: (e) => contentURL(e) });
    } catch (err) { toast(err.message); return null; }
  }
  // Archives open like folders.
  const isDir = (e) => !!e && (e.kind === 'folder' || !!(window.I2FV && window.I2FV.arc && window.I2FV.arc.is(e)));
  // Shards name pieces by PieceCID; PIECE maps them to their index here.
  let PIECE = {};
  // This snapshot's bundle is "" there: its PieceCID was not known yet.
  const pieceAt = (cid) => (typeof cid === 'string' && PIECE[cid] != null ? PIECE[cid] : -1);
  function entryOf(s, r) {
    const dir = s.dirs[r[0]], seg = r.length > 6 ? r[6] : r[1];
    return { dir, name: r[1], kind: r[2], bytes: r[3], cid: r[4], piece: r[5] >= 0 ? pieceAt(s.pieces[r[5]]) : -1, path: dir ? dir + '/' + seg : seg };
  }
  async function listDir(info) {
    if (info.kids) return info.kids;
    const parts = await Promise.all(info.chunks.map(async (c) => {
      const s = await load('s/' + c[0]);
      return s.rows.slice(c[1], c[1] + c[2]).map((r) => entryOf(s, r));
    }));
    return [].concat(...parts);
  }

  // ---- Formatting
  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const num = (n) => Number(n).toLocaleString('en-US');
  // Why a file did not load. explore answers in plain text, but a static
  // copy (the website demo on GitHub Pages) answers a file it left out with
  // its own HTML 404 page, which is no use to show.
  function missing(e, r) {
    if (r.status !== 404) return `The server answered ${r.status} for this file.`;
    if (LOCAL && /^text\/plain/.test(r.headers.get('content-type') || '')) return 'This file is not in the pieces on this machine.';
    return `This copy of the archive does not include this file${e && e.bytes ? ' (' + bytes(e.bytes) + ')' : ''}: big files are left out to keep it small. Run ipfs2fil explore on the archive to ${e && (e.kind === 'video' || e.kind === 'audio') ? 'play' : 'open'} it.`;
  }
  function bytes(n) {
    const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i === 0 || n >= 10 ? Math.round(n) : n.toFixed(1)) + ' ' + u[i];
  }
  const count = (n) => n >= 1e6 ? (n / 1e6).toFixed(1) + ' M' : n >= 1e4 ? (n / 1e3).toFixed(0) + ' K' : num(n);
  const shortCID = (c) => c && c.length > 20 ? c.slice(0, 11) + '…' + c.slice(-5) : (c || '');
  // A shortened CID that shows all of itself when clicked, with a copy
  // button when cp is set. One listener below handles every page's.
  const COPYIC = '<svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true"><path d="M5.5 5.5h7v8h-7zM3.5 10.5v-8h7" fill="none" stroke="currentColor" stroke-width="1.4"/></svg>';
  const cidx = (c, cp) => c ? `<button type="button" class="cidx" data-cid="${esc(c)}" title="${esc(c)}" aria-label="Show the whole CID">${esc(shortCID(c))}</button>${cp ? `<button type="button" class="cpy" data-copy="${esc(c)}" title="Copy the CID" aria-label="Copy the CID">${COPYIC}</button>` : ''}` : '';
  const day = (t) => new Date(t).toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' });
  const plural = (n, one, many) => num(n) + ' ' + (n === 1 ? one : many || one + 's');
  // sourceLabel names the kind of repo an archive came from; archives with no
  // kind came from kubo fs-repos.
  const sourceLabel = (s) => ({ car: 'CAR files', 'kubo-rpc': 'a kubo node, read through its RPC API', atproto: 'a Bluesky account', 'js-ipfs': `a js-ipfs repo · repo ${esc(s.repoVersion)}`, helia: 'a Helia blockstore' })[s.kind] || `a kubo repo · fs-repo ${esc(s.repoVersion)}`;
  const segs = (p) => p.split('/').map(encodeURIComponent).join('/');

  // Kinds: icon path, color, label, plural label. From the boards.
  const KIND = {
    folder: ['M1.5 4.5h4l1.5 1.5h7.5v7.5h-13z', '#1B5FD6', 'Folder', 'Folders'],
    image: ['M2 2.5h12v11h-12z M2 11l3.5-3.5 3 3 2-2 3.5 3.5', '#2F6FB0', 'Photo', 'Photos'],
    video: ['M1.5 3.5h9v9h-9z M10.5 7l4-2.5v7l-4-2.5', '#0B3B8C', 'Video', 'Videos'],
    audio: ['M6 12V3.5l7-1.5v8.5 M6 12a2 2 0 1 1-4 0a2 2 0 0 1 4 0z M13 10.5a2 2 0 1 1-4 0a2 2 0 0 1 4 0z', '#6D28D9', 'Audio', 'Audio'],
    post: ['M2 2.5h12v8.5h-7l-3 3v-3h-2z M5 5.5h6 M5 8h4', '#1185FE', 'Post', 'Posts'],
    page: ['M1.5 2.5h13v11h-13z M1.5 5.5h13', '#4B5260', 'Web page', 'Web pages'],
    doc: ['M4 1.5h5l3 3v10h-8z M9 1.5v3h3 M5.5 9h5', '#B91C1C', 'Document', 'Documents'],
    text: ['M4 1.5h5l3 3v10h-8z M9 1.5v3h3 M6 8.5h4 M6 11h4', '#9A3412', 'Text', 'Text'],
    code: ['M5.5 4.5l-3.5 3.5 3.5 3.5 M10.5 4.5l3.5 3.5-3.5 3.5', '#B45309', 'Code', 'Code'],
    record: ['M3 3.5c0-1 2.2-1.6 5-1.6s5 .6 5 1.6v9c0 1-2.2 1.6-5 1.6s-5-.6-5-1.6z M3 3.5c0 1 2.2 1.6 5 1.6s5-.6 5-1.6 M3 8c0 1 2.2 1.6 5 1.6s5-.6 5-1.6', '#7C3AED', 'Record', 'Records'],
    archive: ['M3 1.5h10v13h-10z M8 1.5v1.5 M8 4.5v1.5 M7 7.5h2v2.5h-2z', '#047857', 'Archive', 'Archives'],
    disk: ['M8 1.5a6.5 6.5 0 1 0 0 13a6.5 6.5 0 0 0 0-13z M8 6.5a1.5 1.5 0 1 0 0 3a1.5 1.5 0 0 0 0-3z', '#0E7490', 'Disk image', 'Disk images'],
    other: ['M4 1.5h5l3 3v10h-8z M9 1.5v3h3', '#8A919E', 'Other', 'Other'],
    // not a file: a repo, archived or inside the content (see repoPaths)
    repo: ['M3.5 1.5h9v13h-9z M3.5 11.5h9 M6 4.5h4 M6 7.5h4', '#2563EB', 'Repository', 'Repositories'],
    // not a file: an account a repo holds (a Bluesky account)
    account: ['M8 2a3 3 0 1 0 0 6a3 3 0 0 0 0-6z M2.5 14c.5-3 2.8-4.5 5.5-4.5s5 1.5 5.5 4.5', '#0F766E', 'Account', 'Accounts']
  };
  const kindOf = (k) => KIND[k] || KIND.other;
  // Bluesky's butterfly, small.
  const BSKY = '<svg width="16" height="16" viewBox="0 0 600 530" aria-hidden="true"><path fill="#1185FE" d="m135.72 44.03c66.496 49.921 138.02 151.14 164.28 205.46 26.262-54.316 97.782-155.54 164.28-205.46 47.98-36.021 125.72-63.892 125.72 24.795 0 17.712-10.155 148.79-16.111 170.07-20.703 73.984-96.144 92.854-163.25 81.433 117.3 19.964 147.14 86.092 82.697 152.22-122.39 125.59-175.91-31.511-189.63-71.766-2.514-7.3797-3.6904-10.832-3.7077-7.8964-0.0174-2.9357-1.1937 0.51669-3.7077 7.8964-13.714 40.255-67.233 197.36-189.63 71.766-64.444-66.128-34.605-132.26 82.697-152.22-67.108 11.421-142.55-7.4491-163.25-81.433-5.9562-21.282-16.111-152.36-16.111-170.07 0-88.687 77.742-60.816 125.72-24.795z"/></svg>';
  const KINDS = Object.keys(KIND);
  const icon = (k) => `<svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="${kindOf(k)[1]}" stroke-width="1.5" stroke-linejoin="round" aria-hidden="true"><path d="${kindOf(k)[0]}"></path></svg>`;
  const PART = { pins: ['Pins', '#1B5FD6'], mfs: ['MFS', '#8DB2F0'], orphans: ['Orphans', '#C2410C'], index: ['Index and explorer', '#9AA1AD'] };
  const ROOTKIND = { 'pin-recursive': 'Recursive pin', 'pin-direct': 'Direct pin', mfs: 'MFS root', orphan: 'Orphan, not pinned' };
  const whereOf = (p) => { const t = p.split('/')[0]; return PART[t] ? t : ''; };

  // ---- Gateways
  const GATEWAYS = [['ipfs.io', 'https://ipfs.io/ipfs/'], ['dweb.link', 'https://dweb.link/ipfs/'], ['your node', 'http://127.0.0.1:8080/ipfs/']];
  // The gateway serving this page, if any: path style (/ipfs/<cid>/...) or subdomain style (<cid>.ipfs.host).
  const HERE = (() => {
    if (!/^https?:$/.test(location.protocol)) return null;
    if (/^\/ip[fn]s\//.test(location.pathname)) return location.origin + '/ipfs/';
    const m = location.host.match(/^[^.]+\.ip[fn]s\.(.+)$/);
    return m ? location.protocol + '//' + m[1] + '/ipfs/' : null;
  })();
  // The archive root: index/manifest.json inside an archive cannot name its own
  // root, so take it from the gateway URL when there is one.
  const URLROOT = (() => {
    const m = location.pathname.match(/^\/ipfs\/([^/]+)\//) || (/^https?:$/.test(location.protocol) && location.host.match(/^([^.]+)\.ipfs\./));
    return m ? m[1] : '';
  })();
  const gw = (cid) => (HERE || GATEWAYS[0][1]) + cid;
  const gwLinks = (cid) => `<span class="gw">${HERE ? `<a href="${esc(HERE + cid)}" target="_blank" rel="noopener">this gateway ↗</a>` : ''}${GATEWAYS.map((g) => `<a href="${esc(g[1] + cid)}" target="_blank" rel="noopener">${g[0]} ↗</a>`).join('')}</span>`;

  // ---- Names for roots: pins live at pins/<mode>/<cid>; show their names instead.
  let ROOTS = {};
  // In the explorer `ipfs2fil explore` serves over all its archives, data/
  // holds one folder per repo, listed in meta.repos.
  let REPOS = null;
  // Library > Repos: the repos archived here (in a combined explorer), each
  // Bluesky account's repo, and the repos inside the content, such as git
  // repos, IPFS repos and CAR files (meta.subRepos), as paths under data/.
  const repoPaths = (meta) => [...new Set((REPOS || []).map((r) => r.name.replace(/[/\\]/g, '_'))
    .concat((meta.accounts || []).map((a) => a.path).filter(Boolean), meta.subRepos || []))];
  function rootAt(p) { return ROOTS[p]; }
  const rootLabel = (r) => r.name || (r.kind === 'mfs' ? 'mfs' : r.cid);
  function showPath(p) {
    const parts = p ? p.split('/') : [];
    const out = [];
    for (let i = 0; i < parts.length; i++) {
      const r = rootAt(parts.slice(0, i + 1).join('/'));
      if (r && r.kind !== 'mfs') { out.length = REPOS ? 1 : 0; out.push(rootLabel(r)); } else out.push(parts[i]);
    }
    return out.join('/');
  }
  function browseHref(p) { return 'browse.html' + (p ? '?p=' + encodeURIComponent(p) : ''); }
  function entryHref(e) {
    const r = rootAt(e.path);
    if (r && r.kind !== 'mfs') return 'pin.html?cid=' + encodeURIComponent(r.cid);
    if (e.kind === 'folder') return browseHref(e.path);
    return 'browse.html?p=' + encodeURIComponent(e.dir) + '&sel=' + encodeURIComponent(e.path);
  }

  // ---- Clipboard and messages
  let toastT;
  function toast(msg) {
    let t = D.querySelector('.toast');
    if (!t) { t = D.createElement('div'); t.className = 'toast'; t.setAttribute('role', 'status'); D.body.appendChild(t); }
    t.textContent = msg;
    t.hidden = false;
    clearTimeout(toastT);
    toastT = setTimeout(() => { t.hidden = true; }, 1800);
  }
  function copy(text) {
    if (!text) return;
    const done = () => toast('Copied ' + shortCID(text));
    if (navigator.clipboard && window.isSecureContext) return void navigator.clipboard.writeText(text).then(done, () => fallback());
    fallback();
    function fallback() {
      const t = D.createElement('textarea');
      t.value = text; D.body.appendChild(t); t.select();
      try { D.execCommand('copy'); done(); } catch (e) { toast('Could not copy'); }
      t.remove();
    }
  }
  D.addEventListener('click', (ev) => {
    const t = ev.target.closest && ev.target.closest('.cidx, [data-copy]');
    if (!t) return;
    ev.preventDefault();
    if (t.dataset.copy) return void copy(t.dataset.copy);
    const open = t.classList.toggle('open');
    t.textContent = open ? t.dataset.cid : shortCID(t.dataset.cid);
    t.setAttribute('aria-label', open ? 'Shorten the CID' : 'Show the whole CID');
  });
  const openTab = (url) => url && window.open(url, '_blank', 'noopener');

  // ---- Page frame: header, bottom rail, shortcuts dialog
  const LOGO = '<svg width="22" height="22" viewBox="0 0 22 22" fill="none" stroke="#1B5FD6" stroke-width="1.8" aria-hidden="true"><rect x="2" y="2" width="8" height="8" rx="1.5"></rect><rect x="12" y="2" width="8" height="8" rx="1.5"></rect><rect x="2" y="12" width="8" height="8" rx="1.5"></rect><rect x="12" y="12" width="8" height="8" rx="1.5" fill="#1B5FD6"></rect></svg>';
  const kbd = (...ks) => ks.map((k) => `<kbd>${esc(k)}</kbd>`).join('');
  const RAIL = {
    overview: [[['j', 'k'], 'move'], [['↵'], 'open'], [['/'], 'search'], [['g', 'f'], 'content'], [['g', 'p'], 'pieces'], [['y'], 'copy root']],
    browse: [[['j', 'k'], 'move'], [['↵'], 'open'], [['u'], 'up a level'], [['/'], 'search here'], [['v'], 'view'], [['l'], 'layout'], [['space'], 'focus the file'], [['F'], 'full screen'], [['d'], 'download'], [['y'], 'copy CID']],
    pin: [[['j', 'k'], 'move'], [['↵'], 'open'], [['u'], 'up a level'], [['o'], 'open in gateway'], [['y'], 'copy CID'], [['g', 'f'], 'content']],
    pieces: [[['j', 'k'], 'move'], [['↵'], 'open'], [['f'], 'filter'], [['s'], 'sort by'], [['r'], 'reverse'], [['y'], 'copy PieceCID'], [['g', 'o'], 'overview']],
    posts: [[['f'], 'filter'], [['y'], 'copy commit CID'], [['o'], 'open commit in gateway']],
    search: [[['↓'], 'into results'], [['j', 'k'], 'move'], [['↵'], 'open'], [['1', '…', '9'], 'kind'], [['0'], 'all kinds'], [['esc'], 'clear'], [['y'], 'copy CID']]
  };
  const GROUPS = [
    ['Anywhere', [['Search', ['/']], ['This list', ['?']], ['Go to overview', ['g', 'o']], ['Go to content', ['g', 'f']], ['Go to pieces', ['g', 'p']], ['Close or cancel', ['esc']]]],
    ['Lists', [['Next or previous row', ['j', 'k']], ['Same, with arrows', ['↓', '↑']], ['Open the row', ['↵']], ['Up a level', ['u', '⌫']], ['Filter the list', ['f']], ['Copy the CID', ['y']], ['Open in a gateway', ['o']], ['Sort by the next column', ['s']], ['Reverse the order', ['r']]]],
    ['Content', [['Search this folder and below', ['/']], ['Move into results', ['↓']], ['Filter by kind', ['1', '…', '9']], ['Show all kinds', ['0']], ['List, grid, tree, columns', ['v']], ['Wide list, split, wide file', ['l']], ['Give the file the room', ['space']], ['Full screen', ['F']], ['Download the file', ['d']], ['Open a tree folder', ['→']], ['Close a tree folder', ['←']]]]
  ];
  let statusEl;
  function frame() {
    const nav = [['overview', 'index.html', 'Overview'], ['browse', 'browse.html', 'Content'], ['pieces', 'pieces.html', 'Pieces']]
      .map((n) => `<a href="${n[1]}"${n[0] === PAGE ? ' aria-current="page"' : ''}>${n[2]}</a>`).join('');
    const q = PAGE === 'search' ? esc(params.get('q') || '') : '';
    D.body.insertAdjacentHTML('afterbegin', `<header class="top"><div class="bar"><a class="brand" href="index.html">${LOGO}<span>ipfs2fil</span></a><nav class="pages" aria-label="Explorer">${nav}</nav><form class="search" action="browse.html" role="search"><label class="sr" for="q">Search names and CIDs</label><input id="q" name="q" type="search" value="${q}" placeholder="Search names and CIDs" autocomplete="off" spellcheck="false"><kbd>/</kbd></form></div></header>`);
    const rail = (RAIL[PAGE] || []).map((r) => `<span>${kbd(...r[0])} ${r[1]}</span>`).join('');
    D.body.insertAdjacentHTML('beforeend', `<footer class="rail-wrap"><div class="rail">${rail}<button type="button" data-act="help">${kbd('?')} all shortcuts</button><span class="status"></span></div></footer>`);
    statusEl = D.querySelector('.rail .status');
    D.querySelector('[data-act=help]').onclick = () => help(true);
    // Served by `ipfs2fil explore` with other archives: link the explorer over all of them.
    if (/^https?:$/.test(location.protocol) && !HERE) {
      fetch('../_i2f/info').then((r) => r.ok ? r.json() : {}).then((j) => {
        if (j.all && !location.pathname.startsWith(j.all)) D.querySelector('nav.pages').insertAdjacentHTML('afterbegin', `<a href="${esc(j.all)}">All repos</a>`);
      }).catch(() => {});
    }
  }
  const status = (t) => { if (statusEl) statusEl.textContent = t; };
  function help(open) {
    let d = D.getElementById('help');
    if (!open) { if (d) { d.remove(); D.querySelector('.shade').remove(); } return; }
    if (d) return;
    const groups = GROUPS.map((g) => `<section><h3>${g[0]}</h3>${g[1].map((r) => `<div class="k"><span>${r[0]}</span><span>${kbd(...r[1])}</span></div>`).join('')}</section>`).join('');
    D.body.insertAdjacentHTML('beforeend', `<div class="shade"></div><div class="dialog" id="help" role="dialog" aria-modal="true" aria-labelledby="help-t"><div class="titled"><h2 id="help-t">Keyboard shortcuts</h2><span>They work on every page. Keys are ignored while you type in a box.</span><button type="button" class="close">Close ${kbd('esc')}</button></div><div class="groups">${groups}</div><p class="note">Two-key shortcuts like ${kbd('g')} ${kbd('b')} are pressed one after the other. The bottom bar on each page shows the keys that matter there.</p></div>`);
    D.querySelector('.shade').onclick = D.querySelector('#help .close').onclick = () => help(false);
    D.querySelector('#help .close').focus();
  }

  // ---- Lists: filter box, kind chips, selects, sortable columns, rows that
  // grow as you scroll, and j/k selection. One list per page takes the keys.
  let active = null;
  class List {
    // o: {cols: [{key, label, cls, sort, html(e)}], kinds: [[k, label]], kindOf(e), text(e),
    //     selects: [{key, label, options: [[v, label]], test(e, v)}], sort0, group(e), step,
    //     href(e), cid(e), filter: placeholder or null, sortSelect: bool, empty: text}
    constructor(el, o) {
      this.el = el; this.o = o; this.items = []; this.loading = true;
      this.s = { q: '', kind: 'all', sort: o.sort0, dir: o.dir0 || 1, limit: o.step || 200, cur: 0, sel: {} };
      (o.selects || []).forEach((x) => { this.s.sel[x.key] = x.value || 'any'; });
      this.build();
      if (!active) active = this;
    }
    build() {
      const o = this.o;
      const sortable = o.cols.filter((c) => c.sort);
      const selects = (o.selects || []).map((x) => `<label>${x.label}<select data-sel="${x.key}">${x.options.map((p) => `<option value="${p[0]}">${p[1]}</option>`).join('')}</select></label>`).join('');
      const sortSel = o.sortSelect === false ? '' : `<label>Sort<select data-sort>${sortable.map((c) => `<option value="${c.key}">${c.label}</option>`).join('')}</select></label><button type="button" class="flip" data-flip aria-label="Reverse the order"></button>`;
      const filter = o.filter ? `<div class="filter"><svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><circle cx="7" cy="7" r="5"></circle><path d="M11 11l3.5 3.5"></path></svg><input type="search" data-q placeholder="${esc(o.filter)}" aria-label="${esc(o.filter)}" autocomplete="off" spellcheck="false"><kbd>f</kbd></div>` : '';
      const tools = filter || selects || sortSel || o.kinds ? `<div class="tools"><div class="row1">${filter}<div class="selects">${selects}${sortSel}</div></div>${o.kinds ? '<div class="chips" role="group" aria-label="Filter by kind"></div>' : ''}</div>` : '';
      const head = o.cols.map((c) => `<th class="${c.cls || ''}">${c.sort ? `<button type="button" data-col="${c.key}">${c.label} <span></span></button>` : c.label}</th>`).join('');
      this.el.innerHTML = `${tools}<div class="scroll" tabindex="0" aria-label="${esc(o.label || 'List')}. J and K move, Enter opens."><table><thead><tr>${head}</tr></thead><tbody></tbody></table></div><div class="foot"><span class="fn"></span><button type="button" class="more" hidden>Load more</button></div><div class="sentinel"></div>`;
      const $ = (s) => this.el.querySelector(s);
      this.tbody = $('tbody'); this.fn = $('.fn'); this.moreBtn = $('.more'); this.chipsEl = $('.chips');
      this.qEl = $('[data-q]');
      if (this.qEl) this.qEl.oninput = () => this.set({ q: this.qEl.value });
      this.el.querySelectorAll('[data-sel]').forEach((s) => { s.value = this.s.sel[s.dataset.sel]; s.onchange = () => { this.s.sel[s.dataset.sel] = s.value; this.set({}); }; });
      this.sortEl = $('[data-sort]');
      if (this.sortEl) this.sortEl.onchange = () => this.set({ sort: this.sortEl.value, dir: 1 });
      const flip = $('[data-flip]');
      if (flip) flip.onclick = () => this.set({ dir: -this.s.dir });
      this.el.querySelectorAll('[data-col]').forEach((b) => { b.onclick = () => this.sortBy(b.dataset.col); });
      this.moreBtn.onclick = () => this.more();
      this.tbody.addEventListener('click', (ev) => {
        const tr = ev.target.closest('tr');
        if (tr) { active = this; this.s.cur = +tr.dataset.i; this.mark(); }
      });
      this.el.addEventListener('focusin', () => { active = this; });
      if ('IntersectionObserver' in window) {
        new IntersectionObserver((es) => { if (es.some((e) => e.isIntersecting) && this.hasMore) this.more(); }, { rootMargin: '600px' }).observe($('.sentinel'));
      }
    }
    sortBy(k) { this.set({ sort: k, dir: k === this.s.sort ? -this.s.dir : 1 }); }
    set(ch) {
      const reset = 'q' in ch || 'kind' in ch || 'sort' in ch || 'dir' in ch || Object.keys(ch).length === 0;
      Object.assign(this.s, ch);
      if (reset && !('cur' in ch)) { this.s.cur = 0; this.s.limit = this.o.step || 200; }
      this.dirty = true;
      this.render();
      if (this.o.onChange) this.o.onChange(this.s);
    }
    setItems(items, loading) { this.items = items; this.loading = !!loading; this.dirty = true; this.render(); }
    more() { this.s.limit += this.o.step || 200; this.render(); }
    compute() {
      const o = this.o, s = this.s, ql = s.q.trim().toLowerCase();
      const sels = (o.selects || []).filter((x) => s.sel[x.key] !== 'any');
      const base = this.items.filter((e) => (!ql || o.text(e).toLowerCase().includes(ql)) && sels.every((x) => x.test(e, s.sel[x.key])));
      const counts = {};
      if (o.kinds) base.forEach((e) => { const k = o.kindOf(e); counts[k] = (counts[k] || 0) + 1; });
      const shown = s.kind === 'all' || !o.kinds ? base : base.filter((e) => o.kindOf(e) === s.kind);
      const col = o.cols.find((c) => c.key === s.sort);
      if (col && col.sort) {
        const key = col.sort, dir = s.dir, g = o.group;
        const keyed = shown.map((e) => [g ? g(e) : 0, key(e), e]);
        keyed.sort((a, b) => a[0] - b[0] || (a[1] < b[1] ? -dir : a[1] > b[1] ? dir : 0));
        this.shown = keyed.map((x) => x[2]);
      } else this.shown = shown;
      this.base = base; this.counts = counts; this.dirty = false;
    }
    render() {
      if (this.dirty) this.compute();
      const o = this.o, s = this.s, shown = this.shown;
      if (this.chipsEl) {
        const chips = [['all', 'All', this.base.length]].concat(o.kinds.filter((k) => this.counts[k[0]]).map((k) => [k[0], k[1], this.counts[k[0]]]));
        if (s.kind !== 'all' && !this.counts[s.kind]) chips.push([s.kind, (o.kinds.find((k) => k[0] === s.kind) || [0, s.kind])[1], 0]);
        this.chipsEl.innerHTML = chips.map((c) => `<button type="button" class="pill" data-k="${c[0]}" aria-pressed="${c[0] === s.kind}">${esc(c[1])} <span class="n">${num(c[2])}</span></button>`).join('');
        this.chipsEl.querySelectorAll('button').forEach((b) => { b.onclick = () => this.set({ kind: b.dataset.k }); });
        this.chipKeys = chips.map((c) => c[0]);
      }
      if (this.sortEl) this.sortEl.value = s.sort;
      const flip = this.el.querySelector('[data-flip]');
      if (flip) flip.textContent = s.dir > 0 ? '▲' : '▼';
      this.el.querySelectorAll('[data-col] span').forEach((sp) => { sp.textContent = sp.parentNode.dataset.col === s.sort ? (s.dir > 0 ? '▲' : '▼') : ''; });
      const page = shown.slice(0, s.limit);
      s.cur = Math.min(s.cur, Math.max(0, page.length - 1));
      this.page = page;
      this.tbody.innerHTML = page.map((e, i) => `<tr data-i="${i}" aria-selected="${i === s.cur && active === this}">${o.cols.map((c) => `<td class="${c.cls || ''}">${c.html(e)}</td>`).join('')}</tr>`).join('') ||
        `<tr><td colspan="${o.cols.length}" class="muted">${this.loading ? 'Loading…' : esc(o.empty || 'Nothing here.')}</td></tr>`;
      this.hasMore = shown.length > page.length;
      this.moreBtn.hidden = !this.hasMore;
      this.moreBtn.textContent = 'Load ' + num(Math.min(o.step || 200, shown.length - page.length)) + ' more';
      this.fn.textContent = (this.hasMore ? 'Showing ' + num(page.length) + ' of ' + num(shown.length) + '. More load as you scroll down.' : (shown.length === 1 ? 'Showing 1.' : 'Showing all ' + num(shown.length) + '.')) + (this.loading ? ' Still loading…' : '');
      if (active === this) this.status();
    }
    mark() {
      this.tbody.querySelectorAll('tr[aria-selected="true"]').forEach((t) => t.setAttribute('aria-selected', 'false'));
      const tr = this.tbody.querySelector(`tr[data-i="${this.s.cur}"]`);
      if (tr) { tr.setAttribute('aria-selected', 'true'); tr.scrollIntoView({ block: 'nearest' }); }
      this.status();
    }
    status() { const e = this.current(); status(e ? (this.o.statusOf ? this.o.statusOf(e) : e.name) : ''); }
    current() { return this.page && this.page[this.s.cur]; }
    move(d) {
      if (!this.page || !this.page.length) return;
      const n = Math.max(0, Math.min(this.page.length - 1, this.s.cur + d));
      this.s.cur = n;
      if (n >= this.page.length - 5 && this.hasMore) { this.more(); }
      this.mark();
    }
    key(k) {
      const e = this.current(), o = this.o;
      switch (k) {
        case 'j': case 'ArrowDown': this.move(1); return true;
        case 'k': case 'ArrowUp': this.move(-1); return true;
        case 'Enter': if (e && o.href) location.href = o.href(e); return !!e;
        case 'f': if (this.qEl) { this.qEl.focus(); this.qEl.select(); return true; } return false;
        case 's': {
          const ks = o.cols.filter((c) => c.sort).map((c) => c.key);
          if (!ks.length) return false;
          this.set({ sort: ks[(ks.indexOf(this.s.sort) + 1) % ks.length], dir: 1 });
          toast('Sorted by ' + o.cols.find((c) => c.key === this.s.sort).label);
          return true;
        }
        case 'r': if (o.cols.some((c) => c.sort)) { this.set({ dir: -this.s.dir }); return true; } return false;
        case 'y': if (e && o.cid && o.cid(e)) { copy(o.cid(e)); return true; } return false;
        case 'o': if (e && o.cid && o.cid(e) && o.gateway !== false) { openTab(gw(o.cid(e))); return true; } return false;
      }
      if (/^[0-9]$/.test(k) && this.chipKeys && o.digits) {
        const c = this.chipKeys[+k];
        if (c) this.set({ kind: c });
        return true;
      }
      return false;
    }
  }

  // ---- Keyboard
  let pendingG = 0, pageKeys = {};
  D.addEventListener('keydown', (ev) => {
    if (ev.metaKey || ev.ctrlKey || ev.altKey) return;
    const t = ev.target, k = ev.key;
    // Another widget's keys (explore --feedback's box) stay its own.
    if ((t.closest && t.closest('.fbk')) || D.querySelector('.fbk [role="dialog"], dialog[open]')) return;
    const typing = t.matches && (t.matches('input, select, textarea') || t.isContentEditable);
    if (k === 'Escape') {
      if (D.getElementById('help')) { help(false); ev.preventDefault(); return; }
      if (!typing && pageKeys.esc && pageKeys.esc()) { ev.preventDefault(); return; }
      if (typing) {
        if (t.value && t.type === 'search') { t.value = ''; t.dispatchEvent(new Event('input')); } else t.blur();
        ev.preventDefault();
      }
      return;
    }
    if (typing) {
      if (t.type === 'search' && (k === 'ArrowDown' || k === 'Enter') && pageKeys.fromBox) { if (pageKeys.fromBox(k, t)) ev.preventDefault(); }
      return;
    }
    if (D.getElementById('help') && k !== '?') return;
    if (pendingG && Date.now() - pendingG < 1500) {
      pendingG = 0;
      const to = { o: 'index.html', b: 'browse.html', f: 'browse.html', p: 'pieces.html', s: 'search.html' }[k];
      if (to) { location.href = to; ev.preventDefault(); }
      return;
    }
    pendingG = 0;
    let done = true;
    if (k === '/') { const q = D.getElementById('fq') || D.getElementById('q'); q.focus(); q.select(); }
    else if (k === '?') help(!D.getElementById('help'));
    else if (k === 'g') { pendingG = Date.now(); status('g…'); }
    else if (pageKeys[k] && pageKeys[k]()) { /* handled */ }
    else if (active && active.key(k)) { /* handled */ }
    else if (k === 'y' && pageKeys.cid) copy(pageKeys.cid());
    else if (k === 'o' && pageKeys.cid) openTab(gw(pageKeys.cid()));
    else done = false;
    if (done) ev.preventDefault();
  });

  // ---- Common columns
  const nameCol = (sub) => ({ key: 'name', label: 'Name', sort: (e) => e.name.toLowerCase(), html: (e) => `<span class="nm">${icon(e.kind)}<span><a href="${esc(entryHref(e))}">${esc(e.name)}${e.kind === 'folder' ? '/' : ''}</a>${sub ? `<div class="sub">${esc(sub(e))}</div>` : ''}</span></span>` });
  const kindCol = { key: 'kind', label: 'Kind', cls: 'muted hide-sm', sort: (e) => kindOf(e.kind)[2], html: (e) => esc(kindOf(e.kind)[2]) };
  const sizeCol = { key: 'size', label: 'Size', cls: 'r', sort: (e) => e.bytes, html: (e) => bytes(e.bytes) };
  const pieceCol = { key: 'piece', label: 'Piece', cls: 'r hide-sm', sort: (e) => e.piece, html: (e) => e.piece >= 0 ? `<a href="pieces.html?n=${e.piece}">${e.piece}</a>` : '' };
  const cidCol = { key: 'cid', label: 'CID', cls: 'cid hide-sm', html: (e) => e.cid ? `${cidx(e.cid, true)}<a href="${esc(gw(e.cid))}" target="_blank" rel="noopener" title="Open in an IPFS gateway" aria-label="Open ${esc(e.name)} in an IPFS gateway">↗</a>` : '' };
  const SIZES = [['any', 'Any'], ['small', 'Under 1 MiB'], ['mid', '1 to 100 MiB'], ['big', 'Over 100 MiB']];
  const sizeTest = (e, v) => v === 'small' ? e.bytes < 1 << 20 : v === 'mid' ? e.bytes >= 1 << 20 && e.bytes <= 100 << 20 : e.bytes > 100 << 20;
  const kindChips = (exclude) => KINDS.filter((k) => k !== exclude).map((k) => [k, KIND[k][3]]);
  const folderFirst = (e) => (e.kind === 'folder' ? 0 : 1);

  function bar(parts, label) {
    const total = parts.reduce((a, p) => a + p[1], 0) || 1;
    return `<div class="barchart" role="img" aria-label="${esc(label)}">${parts.filter((p) => p[1] > 0).map((p) => `<div style="flex:${Math.max(p[1] / total * 1000, 4)} 1 0;background:${p[2]}"></div>`).join('')}</div>`;
  }
  function crumbs(p) {
    const parts = p ? p.split('/') : [];
    let out = `<a href="index.html">${REPOS ? 'all repos' : 'archive'}</a><span aria-hidden="true">/</span>` + (parts.length ? `<a href="${browseHref('')}">data</a>` : '<span aria-current="page">data</span>');
    let start = 0;
    for (let i = 0; i < parts.length; i++) {
      const sub = parts.slice(0, i + 1).join('/'), r = rootAt(sub);
      if (r && r.kind !== 'mfs') start = i;
    }
    for (let i = 0; i < parts.length; i++) {
      const sub = parts.slice(0, i + 1).join('/'), r = rootAt(sub);
      const label = r && r.kind !== 'mfs' && i === start ? rootLabel(r) : parts[i];
      const shown = label.length > 40 ? shortCID(label) : label;
      out += '<span aria-hidden="true">/</span>' + (i === parts.length - 1 ? `<span aria-current="page">${shown !== label ? cidx(label) : esc(shown)}</span>` : `<a href="${esc(r && r.kind !== 'mfs' ? 'pin.html?cid=' + encodeURIComponent(r.cid) : browseHref(sub))}">${esc(shown)}</a>`);
    }
    return `<nav class="crumbs" aria-label="Path">${out}</nav>`;
  }
  const main = () => D.querySelector('main');
  function fail(err) {
    main().innerHTML = `<div class="err"><h2>Something is missing</h2><p class="note">${esc(err.message || err)}</p><p class="note">If you opened this from disk, open explorer/index.html inside the archive, or run <code>ipfs2fil explore</code>.</p></div>`;
  }

  // ---- Overview
  async function overview() {
    const [m, meta] = await Promise.all([manifest(), load('meta')]);
    const roots = m.roots || [], pieces = m.pieces || [], copies = m.copies || [];
    // own counts each block once across roots; archives packed before it was added have only bytes
    const hasOwn = roots.some((r) => r.own != null);
    // a root several repos hold counts once
    const uniq = (rs) => { const seen = new Set(); return REPOS ? rs.filter((r) => !seen.has(r.cid) && seen.add(r.cid)) : rs; };
    const sum = (f) => uniq(roots.filter(f)).reduce((a, r) => a + (hasOwn ? r.own || 0 : r.bytes), 0);
    const isPin = (r) => r.kind === 'pin-recursive' || r.kind === 'pin-direct';
    const pins = uniq(roots.filter(isPin)), rec = pins.filter((r) => r.kind === 'pin-recursive').length;
    const orphans = uniq(roots.filter((r) => r.kind === 'orphan'));
    const parts = meta.parts || {};
    const indexBytes = pieces.filter((p, i) => (meta.pieces[i] || {}).where === 'index').reduce((a, p) => a + p.carBytes, 0);
    const snaps = m.snapshots || [], last = snaps[snaps.length - 1];
    const total = m.source.bytes;
    const stored = copies.length ? `stored on Filecoin in ${plural(copies.length, 'copy', 'copies')}` : 'not on Filecoin yet';
    const root = m.root || URLROOT;
    pageKeys.cid = () => root;
    const types = KINDS.filter((k) => meta.types[k]).map((k) => [k, meta.types[k][1], KIND[k][1], KIND[k][3]]).sort((a, b) => b[1] - a[1]);
    const partBars = [['pins', sum(isPin)], ['mfs', sum((r) => r.kind === 'mfs')], ['orphans', sum((r) => r.kind === 'orphan')], ['index', indexBytes]].map((p) => [PART[p[0]][0], p[1], PART[p[0]][1]]);
    main().innerHTML = `
<section class="head">
  <div class="eyebrow">${REPOS ? `All ${plural(REPOS.length, 'repo')} on this machine, together` : `Archive of ${sourceLabel(m.source)}`}</div>
  <h1>${bytes(total)}, ${plural(pins.length, 'pin')}, ${stored}</h1>
  <div class="meta">
    ${REPOS ? '<span>Each file a repo shares with an earlier one counts once</span>' : `<span>Archive root ${root ? `${cidx(root)} <button type="button" class="link" data-copy="${esc(root)}">Copy</button>` : 'this archive <span class="faint">(open it through a gateway to see its CID)</span>'}</span>`}
    ${m.dataRoot ? `<span>Data root ${cidx(m.dataRoot)} <button type="button" class="link" data-copy="${esc(m.dataRoot)}">Copy</button></span>` : ''}
    ${last ? `<span>Snapshot ${day(last.time)}${snaps.length > 1 ? ` · ${plural(snaps.length - 1, 'earlier snapshot')}` : ''}</span>` : ''}
  </div>
</section>
<section class="stats" aria-label="Totals">
  ${stat('Total size', bytes(total), count(m.source.blocks) + ' blocks')}
  ${stat('Pinned', bytes(sum(isPin)), `${num(rec)} recursive, ${num(pins.length - rec)} direct`)}
  ${stat('MFS', bytes(sum((r) => r.kind === 'mfs')), plural((parts.mfs || [0])[0], 'file'))}
  ${stat('Orphans', bytes(sum((r) => r.kind === 'orphan')), plural(orphans.length, 'root') + ', not pinned')}
  ${stat('On Filecoin', plural(pieces.length, 'piece'), copies.length ? `${plural(copies.length, 'copy', 'copies')} · providers ${copies.map((c) => esc(c.provider)).join(', ')}` : 'not stored yet')}
</section>
<section class="card" style="gap:18px">
  <h2>What takes the space</h2>
  <div class="stack"><div class="faint" style="font-weight:500">By part of the repo</div>
    ${bar(partBars, partBars.map((p) => p[0] + ' ' + bytes(p[1])).join(', '))}
    <div class="legend">${partBars.filter((p) => p[1] > 0).map((p) => `<span><span class="sw" style="background:${p[2]}"></span>${p[0]} ${bytes(p[1])}</span>`).join('')}</div></div>
  <div class="stack"><div class="faint" style="font-weight:500">By file type <span style="font-weight:400">(guessed from names and first bytes)</span></div>
    ${bar(types.map((t) => [t[3], t[1], t[2]]), types.map((t) => t[3] + ' ' + bytes(t[1])).join(', '))}
    <div class="legend">${types.map((t) => `<a href="browse.html?deep=1&kind=${t[0]}"><span class="sw" style="background:${t[2]}"></span>${t[3]} ${bytes(t[1])}</a>`).join('')}</div></div>
</section>
<section class="card"><div class="titled"><h2>What it holds</h2><a href="browse.html?deep=1">All content</a></div>${contents()}</section>
${REPOS ? `<section class="card"><h2>Archive repos</h2><div id="repos"></div></section>` : ''}
<div class="split">
  <section class="card wide"><div class="titled"><h2>Largest roots</h2><a href="${browseHref('')}">Browse all</a></div><div id="roots"></div></section>
  <section class="card side" style="gap:12px"><div class="titled"><h2>Pieces</h2><a href="pieces.html">All ${num(pieces.length)}</a></div>
    <div class="toggle" role="group" aria-label="Color pieces by"><button type="button" class="pill" data-by="part" aria-pressed="true">Part of repo</button><button type="button" class="pill" data-by="type" aria-pressed="false">File type</button></div>
    <div class="pmap" role="img" id="pmap"></div>
    <div class="faint" id="pnote"></div></section>
</div>`;
    // Every kind of content, counted, with what is worth knowing about it.
    function contents() {
      const accts = meta.accounts || [], few = (xs) => xs.slice(0, 4).map(esc).join(', ') + (xs.length > 4 ? ` and ${num(xs.length - 4)} more` : '');
      const NOTE = {
        post: () => accts.length ? 'from ' + few(accts.map((a) => a.name)) : '',
        record: () => accts.length ? `likes, follows and other IPLD records from ${plural(accts.length, 'Bluesky repo')} (posts are under Posts)` : 'IPLD (dag-cbor, dag-json) roots',
        image: () => accts.some((a) => a.blobs && a.blobs.images) ? `${num(accts.reduce((n, a) => n + ((a.blobs && a.blobs.images) || 0), 0))} attached to posts` : '',
        video: () => accts.some((a) => a.blobs && a.blobs.videos) ? `${num(accts.reduce((n, a) => n + ((a.blobs && a.blobs.videos) || 0), 0))} attached to posts` : ''
      };
      const rows = KINDS.filter((k) => k !== 'folder' && meta.types[k]).map((k) => [KIND[k][3], meta.types[k][0], meta.types[k][1], (NOTE[k] || (() => ''))(), 'browse.html?deep=1&kind=' + k, k]);
      if (accts.length) rows.push(['Accounts', accts.length, accts.reduce((n, a) => n + ((a.blobs && a.blobs.bytes) || 0), 0), few(accts.map((a) => a.name)), 'browse.html?deep=1&kind=account', 'account']);
      const rp = repoPaths(meta);
      if (rp.length) rows.push(['Repositories', rp.length, REPOS ? total : 0, few(rp.map((p) => p.slice(p.lastIndexOf('/') + 1))), 'browse.html?deep=1&kind=repo', 'repo']);
      return `<table class="ctable"><thead><tr><th>Kind</th><th class="r">Items</th><th class="r">Size</th><th>Notes</th></tr></thead><tbody>${rows.map((r) => `<tr><td><a href="${esc(r[4])}">${icon(r[5])}${esc(r[0])}</a></td><td class="r">${num(r[1])}</td><td class="r">${r[2] ? bytes(r[2]) : ''}</td><td class="faint">${r[3]}</td></tr>`).join('')}</tbody></table>`;
    }
    function stat(k, v, s) { return `<div class="card tight stat"><div class="k">${k}</div><div class="v">${v}</div><div class="s">${s}</div></div>`; }
    const once = new Set();
    const largest = roots.filter((r) => !REPOS || (!once.has(r.cid) && once.add(r.cid))).sort((a, b) => b.bytes - a.bytes).slice(0, 10).map((r) => ({ ...r, name: rootLabel(r), path: r.path, dir: '', piece: r.firstPiece, kind: kindAt(r), root: r }));
    function kindAt(r) { return r.kind === 'mfs' ? 'folder' : 'other'; }
    const where = (r) => (REPOS ? r.path.split('/')[0] + ' · ' : '') + ({ 'pin-recursive': 'pin', 'pin-direct': 'direct pin', mfs: 'MFS', orphan: 'orphan' }[r.root.kind] + ' · ' + shortCID(r.cid));
    const list = new List(D.getElementById('roots'), {
      label: 'Largest roots', step: 10, sortSelect: false, sort0: null, filter: null,
      cols: [nameCol(where), { key: 'pieces', label: 'Pieces', cls: 'r', html: (r) => num(r.root.lastPiece - r.root.firstPiece + 1) }, sizeCol],
      href: entryHref, cid: (r) => r.cid
    });
    list.setItems(largest);
    if (REPOS) {
      const rows = REPOS.map((r) => ({ ...r, path: r.name, dir: '', kind: 'folder', cid: r.root, piece: -1 }));
      new List(D.getElementById('repos'), {
        label: 'Archive repos', step: 50, sortSelect: false, sort0: null, filter: null,
        cols: [nameCol((r) => r.repo || ''), { key: 'pieces', label: 'Pieces', cls: 'r', html: (r) => num(r.pieces) },
          { key: 'shared', label: 'Shared', cls: 'r', html: (r) => r.shared ? bytes(r.shared) : '' }, sizeCol,
          { key: 'own', label: '', cls: 'r', html: (r) => `<a href="${esc(r.href)}">its own explorer</a>` }],
        href: (r) => browseHref(r.name), cid: (r) => r.root
      }).setItems(rows);
    }
    // Kinds of root entries come from the index; fill them in when it loads.
    Promise.all(largest.map(async (r) => { const d = await dirInfo(r.path); if (d) r.kind = 'folder'; })).then(() => list.setItems(largest));

    const cells = Math.min(pieces.length, 216);
    let by = 'part';
    const paint = () => {
      D.querySelectorAll('[data-by]').forEach((b) => b.setAttribute('aria-pressed', b.dataset.by === by));
      D.getElementById('pmap').innerHTML = meta.pieces.slice(0, cells).map((p, i) => `<a href="pieces.html?n=${i}" title="#${i} · ${esc(p.label)}" style="background:${by === 'part' ? (PART[p.where] || PART.index)[1] : kindOf(p.kind)[1]}"></a>`).join('');
      D.getElementById('pmap').setAttribute('aria-label', `Piece map: ${cells} of ${pieces.length} pieces, colored by ${by === 'part' ? 'part of repo' : 'file type'}`);
    };
    D.querySelectorAll('[data-by]').forEach((b) => { b.onclick = () => { by = b.dataset.by; paint(); }; });
    paint();
    D.getElementById('pnote').textContent = `Each square is a piece, colored by what most of it holds.${cells < pieces.length ? ` Showing ${num(cells)} of ${num(pieces.length)}.` : ''}${last ? ` Stored as of ${day(last.time)}; for live proofs run ipfs2fil status.` : ''}`;
    status(bytes(total) + ' · ' + num(pieces.length) + ' pieces' + (copies.length ? ' × ' + copies.length : '') + ' · ' + (m.tool || 'ipfs2fil'));
  }

  // ---- Files: browse folders, search under one, view what is in them.
  // Layout from the round 2 boards: the library by type on the left, search
  // scoped to this folder and below at the top, a list (or grid, tree or
  // columns) in the middle and the selected file with its details on the right.
  //
  // File bytes come from `ipfs2fil explore` (../_i2f/content/<cid>) or, through
  // a gateway, from ../data/ in this archive. From file:// there are none
  // unless the viewer asks for a public gateway.
  // CONTENT is where explore serves file bytes; a static copy of the
  // explorer (the website's demo) can point its _i2f/info elsewhere.
  let LOCAL = false, PUBLIC = false, CONTENT = '../_i2f/content/';
  async function probeLocal() {
    if (HERE || !/^https?:$/.test(location.protocol)) return false;
    try {
      const r = await fetch('../_i2f/info');
      const j = r.ok ? await r.json() : {};
      if (typeof j.content === 'string') CONTENT = j.content;
      return !!j.content;
    } catch (e) { return false; }
  }
  const qs = (o) => Object.keys(o).filter((k) => o[k] !== '' && o[k] != null).map((k) => k + '=' + encodeURIComponent(o[k])).join('&');
  // Bluesky records are dag-cbor, which gateways turn into JSON on request;
  // the records and blobs under an account's folder are not UnixFS, so a
  // gateway cannot find them by path, only by CID.
  const isCBOR = (c) => /^bafyrei/.test(c || '');
  let ATPATHS = [];
  const byCID = (e) => isCBOR(e.cid) || ATPATHS.some((p) => e.path.startsWith(p + '/'));
  function contentURL(e, extra) {
    if (e && e.blob && V) return V.arc.url(e); // a file read out of an archive
    if (!e || !e.cid) return null;
    if (LOCAL) return CONTENT + encodeURIComponent(e.cid) + '?' + qs({ name: e.name, ...extra });
    const dl = extra && extra.dl;
    if ((HERE || PUBLIC) && byCID(e)) return gw(e.cid) + '?' + qs({ format: isCBOR(e.cid) ? 'dag-json' : '', filename: e.name, download: dl ? 'true' : '' });
    if (HERE) return '../data/' + segs(e.path) + (dl ? '?download=true&filename=' + encodeURIComponent(e.name) : '');
    if (PUBLIC) return gw(e.cid) + '?filename=' + encodeURIComponent(e.name) + (dl ? '&download=true' : '');
    return null;
  }
  const thumbURL = (e, w) => contentURL(e, LOCAL ? { w: w || 320 } : null);
  // Static hosts (the website demo on GitHub Pages) send files with no
  // extension as application/octet-stream, which <img> refuses for SVG; such
  // images are shown again from a blob typed as SVG.
  D.addEventListener('error', (ev) => {
    const im = ev.target;
    if (!(im instanceof HTMLImageElement) || im.dataset.svg || !/[?&](file)?name=[^&]*\.svg(&|$)/i.test(im.src)) return;
    im.dataset.svg = '1';
    fetch(im.src).then((r) => r.ok ? r.blob() : null).then((b) => { if (b) im.src = URL.createObjectURL(new Blob([b], { type: 'image/svg+xml' })); }).catch(() => {});
  }, true);
  const canShow = () => LOCAL || HERE || PUBLIC;
  const AVTYPE = { mp4: 'video/mp4', m4v: 'video/mp4', mov: 'video/quicktime', webm: 'video/webm', ogv: 'video/ogg', mp3: 'audio/mpeg', m4a: 'audio/mp4', aac: 'audio/aac', ogg: 'audio/ogg', oga: 'audio/ogg', opus: 'audio/ogg', flac: 'audio/flac', wav: 'audio/wav' };

  // Kinds an older pack could not tell from names; explore sniffs them.
  const KOVER = {}, asked = new Set();
  async function fixKinds(items, then) {
    if (!LOCAL) return;
    const want = items.filter((e) => e.kind === 'other' && e.cid && !asked.has(e.cid)).map((e) => e.cid);
    if (!want.length) return;
    want.forEach((c) => asked.add(c));
    for (let i = 0; i < want.length; i += 2000) {
      try {
        const r = await fetch('../_i2f/kinds', { method: 'POST', body: want.slice(i, i + 2000).join('\n') });
        if (r.ok) Object.assign(KOVER, await r.json());
      } catch (e) { return; }
      then();
    }
  }
  const kindFix = (e) => { if (e.kind === 'other' && KOVER[e.cid]) e.kind = KOVER[e.cid]; return e; };
  // Files only partly in the pieces (an orphan whose root a repo had, but
  // not the rest): CID -> [blocks here, blocks in all]. explore tells.
  const PARTS = {}, askedParts = new Set();
  async function findParts(items, then) {
    if (!LOCAL) return;
    const want = items.filter((e) => e.kind !== 'folder' && e.cid && !askedParts.has(e.cid)).map((e) => e.cid);
    if (!want.length) return;
    want.forEach((c) => askedParts.add(c));
    try {
      const r = await fetch('../_i2f/parts', { method: 'POST', body: want.join('\n') });
      if (r.ok) { const got = await r.json(); Object.assign(PARTS, got); if (Object.keys(got).length) then(); }
    } catch (e) { /* older explore */ }
  }
  const V = window.I2FV || null;

  // Every entry, streamed once from all shards, for search and the library.
  let allP = null;
  function allEntries(progress) {
    return allP || (allP = (async () => {
      const meta = await load('meta'), out = [];
      let next = 0, last = 0;
      const work = async () => {
        while (next < meta.shards.length) {
          const s = await load('s/' + meta.shards[next++]);
          for (const r of s.rows) out.push(entryOf(s, r));
          if (progress && Date.now() - last > 250) { last = Date.now(); progress(out); }
        }
      };
      await Promise.all([work(), work(), work(), work()]);
      return out;
    })());
  }
  const under = (p) => (e) => !p || e.path.startsWith(p + '/');
  const SORTS = { name: (e) => e.name.toLowerCase(), size: (e) => e.bytes, kind: (e) => kindOf(e.kind)[2], piece: (e) => e.piece, where: (e) => e.dir };
  const VIEWS = [['list', 'List', 'M2 4h12M2 8h12M2 12h12'], ['grid', 'Grid', 'M2 2h5v5h-5z M9 2h5v5h-5z M2 9h5v5h-5z M9 9h5v5h-5z'], ['tree', 'Tree', 'M3 2v11h4M3 7.5h4M9 4.5h5M9 7.5h5M9 13h5'], ['cols', 'Columns', 'M1.5 2.5h13v11h-13z M6 2.5v11 M10.5 2.5v11']];
  const LAYOUTS = [['list', 'Wide list', 'M1.5 2.5h15v11h-15z M13 2.5v11'], ['split', 'Split', 'M1.5 2.5h15v11h-15z M9 2.5v11'], ['file', 'Wide file', 'M1.5 2.5h15v11h-15z M5 2.5v11']];
  const LIB = [['all', 'Everything'], ['image', 'Photos'], ['video', 'Videos'], ['audio', 'Audio'], ['post', 'Posts'], ['doc', 'Documents'], ['page', 'Web pages'], ['text', 'Text'], ['code', 'Code'], ['record', 'Records'], ['account', 'Accounts'], ['repo', 'Repositories'], ['archive', 'Archives'], ['disk', 'Disk images'], ['other', 'Other']];
  const pref = (k, d) => { try { return localStorage.getItem('i2f.' + k) || d; } catch (e) { return d; } };
  const keep = (k, v) => { try { localStorage.setItem('i2f.' + k, v); } catch (e) { /* private mode */ } };
  const svg = (d, w) => `<svg width="${w || 16}" height="16" viewBox="0 0 ${w || 16} 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round" aria-hidden="true"><path d="${d}"></path></svg>`;
  const ext = (n) => { const i = n.lastIndexOf('.'); return i > 0 ? n.slice(i + 1).toLowerCase() : ''; };

  async function files() {
    const [m, meta] = await Promise.all([manifest(), load('meta')]);
    // Accounts (Bluesky's): each one's posts open in the Bluesky App viewer,
    // here in the main pane, or in its own archive's explorer when this
    // combined one does not hold its summary.
    const ACC = meta.accounts || [];
    const inHere = (a) => !a.href || /^(posts|browse)\.html/.test(a.href);
    const accHref = (i, k) => inHere(ACC[i]) ? appHref(i, k) : ACC[i].href.replace(/#.*/, '') + (k ? '&t=' + encodeURIComponent(k) : '');
    const POSTDIR = '/app.bsky.feed.post/';
    const acctOf = (e) => ACC.findIndex((a) => a.path && e.path.startsWith(a.path + POSTDIR));
    // A Bluesky post's record links to the post in the Bluesky App.
    const recordHref = (e, rec) => {
      if (rec.$type !== 'app.bsky.feed.post') return '';
      const i = acctOf(e);
      return i < 0 ? '' : accHref(i, e.name.replace(/\.json$/, ''));
    };
    if (V) V.use({ esc, icon, bytes, recordHref, show: showIn });
    LOCAL = await probeLocal();
    // Posts draw from the account summaries, loaded when a post shows up.
    const KX = {};
    const postOf = (e) => {
      if (e.kind !== 'post') return null;
      const i = acctOf(e);
      const K = i >= 0 && kitOf(i), x = K && K.byKey.get(e.name.replace(/\.json$/, ''));
      return x ? { K, x, n: i } : null;
    };
    const kitOf = (i) => { const K = KX[i]; if (K && K !== 'wait' && K !== 'none') return K; if (!K && inHere(ACC[i])) { KX[i] = 'wait'; bsky(i).then((k) => { KX[i] = k; update(true); }).catch(() => { KX[i] = 'none'; }); } return null; };
    const label = (e) => { const P = postOf(e); return P ? (P.x.t ? day(P.x.t) + ' · ' : '') + P.K.snippet(P.x) : e.name; };
    // The accounts, as entries of their own under Library > Accounts.
    const REPOP = repoPaths(meta);
    const ACCTS = ACC.map((a, i) => ({ name: a.name, path: a.path || '', dir: '', kind: 'account', cid: a.did || 'account ' + i, bytes: (a.blobs && a.blobs.bytes) || 0, piece: -1, acct: i }));
    let NAV = false;
    try { NAV = sessionStorage.getItem('i2f.nav') === '1'; } catch (e) { /* private mode */ }
    const navOn = (on) => { NAV = on; try { sessionStorage.setItem('i2f.nav', on ? '1' : ''); } catch (e) { /* private mode */ } };
    main().addEventListener('click', (ev) => navOn(!!(ev.target.closest && ev.target.closest('.lib a'))), true);
    const S = { view: pref('view', 'list'), ly: pref('layout', 'split'), cur: -1, limit: 300, open: new Set() };
    // The folder, search and filters live in the URL: every page link and
    // the back and forward buttons set them from it.
    function fromURL() {
      const P = new URLSearchParams(location.search);
      Object.assign(S, {
        p: P.get('p') || '', q: P.get('q') || '', kind: P.get('kind') || 'all', deep: P.get('deep') === '1' || !!P.get('q'),
        size: P.get('size') || 'any', sort: P.get('sort') || 'name', dir: P.get('dir') === '-1' ? -1 : 1, sel: P.get('sel') || '',
        app: P.get('app') || '', an: +P.get('a') || 0
      });
      if (S.kind !== 'all' && !P.has('deep')) S.deep = true;
    }
    fromURL();
    let info = null, kids = [], all = null, loadingAll = false, shown = [], rows = [], counts = {}, base = [];

    main().className = 'files-main';
    main().innerHTML = `<div class="files">
  <nav class="lib" aria-label="Library"></nav>
  <section class="fmain">
    <div class="fbar"><div class="fcrumbs"></div>
      <div class="seg" role="group" aria-label="View">${VIEWS.map((v) => `<button type="button" data-view="${v[0]}" title="${v[1]} view (v)" aria-label="${v[1]}">${svg(v[2])}</button>`).join('')}</div>
      <div class="seg ly" role="group" aria-label="Layout">${LAYOUTS.map((v) => `<button type="button" data-ly="${v[0]}" title="${v[1]} (l)" aria-label="${v[1]}">${svg(v[2], 18)}</button>`).join('')}</div></div>
    <div class="fsearch"><div class="sbox"><svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true"><circle cx="7" cy="7" r="4.5"></circle><path d="M10.5 10.5l3.5 3.5"></path></svg>
      <span class="scope"></span><label class="sr" for="fq">Search names and CIDs</label><input id="fq" type="search" autocomplete="off" spellcheck="false"><kbd>/</kbd></div></div>
    <div class="fchips"><div class="chips" role="group" aria-label="Filter by kind"></div>
      <div class="selects"><label class="deep"><input type="checkbox" id="fdeep"> Include subfolders</label>
        <label>Size<select id="fsize">${SIZES.map((s) => `<option value="${s[0]}">${s[1]}</option>`).join('')}</select></label>
        <label>Sort<select id="fsort"><option value="name">Name</option><option value="size">Size</option><option value="kind">Kind</option><option value="piece">Piece</option><option value="where">Folder</option></select></label><button type="button" class="flip" id="fflip" aria-label="Reverse the order"></button></div></div>
    <div class="fbody"><div class="flist" tabindex="0" aria-label="Files. J and K move, Enter opens."></div><aside class="fprev" aria-label="Preview and details"><div class="pv"></div><div class="deck"></div></aside></div>
  </section></div>`;
    const $ = (s) => main().querySelector(s);
    const box = $('#fq');
    box.value = S.q;
    box.placeholder = 'Search names and CIDs';
    D.getElementById('q').closest('form').hidden = true; // this page has its own box

    // ---- the library sidebar
    const libHref = (k) => 'browse.html?' + qs({ kind: k === 'all' ? '' : k, deep: '1' });
    function lib() {
      const parts = meta.parts || {}, t = meta.types || {};
      const libOn = S.deep && !S.q && !S.p && !S.app;
      const nOf = (k) => k === 'all' ? meta.entries : k === 'account' ? ACCTS.length : k === 'repo' ? REPOP.length : t[k] ? t[k][0] : 0;
      $('.lib').innerHTML = `<div class="h">Library</div>${LIB.filter((k) => nOf(k[0])).map((k) => `<a href="${libHref(k[0])}"${libOn && S.kind === k[0] ? ' aria-current="page"' : ''}><span style="color:${k[0] === 'all' ? 'var(--muted)' : kindOf(k[0])[1]}">${k[0] === 'all' ? svg(VIEWS[1][2]) : icon(k[0])}</span>${k[1]}<span class="n">${count(nOf(k[0]))}</span></a>`).join('')}
<div class="h">${REPOS ? 'Archive repos' : 'Places'}</div><a href="browse.html"${!S.p && !S.deep && !S.app ? ' aria-current="page"' : ''}>${icon('folder')}${REPOS ? 'All repos' : 'All of data/'}</a>${(REPOS ? REPOS.map((r) => [r.name, r.name]) : ['pins', 'mfs', 'orphans'].filter((w) => parts[w]).map((w) => [w, PART[w][0]])).map((w) => `<a href="${browseHref(w[0])}"${S.p === w[0] && !S.deep ? ' aria-current="page"' : ''}>${icon('folder')}${esc(w[1])}</a>`).join('')}
${ACC.length ? `<div class="h">Viewers</div><a href="${esc(accHref(ACC.findIndex(inHere) >= 0 ? ACC.findIndex(inHere) : 0))}"${S.app === 'bsky' ? ' aria-current="page"' : ''}>${BSKY}Bluesky App</a>` : ''}
<div class="h">Smart</div><a href="browse.html?${qs({ deep: '1', sort: 'size', dir: '-1' })}">${svg('M2 13h12M4 13V8M8 13V3M12 13V6')}Largest files</a>`;
      const on = NAV && $('.lib a[aria-current="page"]');
      if (on) on.focus({ preventScroll: true });
    }

    // A viewer (the Bluesky App) takes the place of the file list.
    if (S.app === 'bsky' && ACC[S.an]) {
      lib();
      const ap = Object.assign(D.createElement('section'), { className: 'fapp' });
      $('.fmain').replaceWith(ap);
      Object.assign(pageKeys, { ArrowDown: () => NAV && navStep(1), ArrowUp: () => NAV && navStep(-1) });
      return bskyApp(ap, S.an);
    }

    // ---- data for the current folder and scope
    async function enter(p, keepSel) {
      S.p = p;
      info = await dirInfo(p);
      if (!info) {
        // a file path: show its folder with it selected
        const d = p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : '';
        if (d !== p && await dirInfo(d)) { S.sel = p; return enter(d, true); }
        throw new Error(`There is no folder data/${p} in this archive.`);
      }
      if (!keepSel) S.sel = '';
      kids = (await listDir(info)).map(kindFix);
      pageKeys.cid = () => (current() || {}).cid || info.cid;
      S.cur = -1; S.limit = 300;
      update(true);
      fixKinds(kids, () => { kids.forEach(kindFix); update(); });
      if (S.deep) needAll();
    }
    function needAll() {
      if (all || loadingAll) return;
      loadingAll = true;
      allEntries((part) => { all = part; update(); }).then((es) => {
        all = es.map(kindFix); loadingAll = false; update();
        if (S.deep) fixKinds(all.filter(under(S.p)), () => { all.forEach(kindFix); update(); });
      });
    }
    function compute() {
      const ql = S.q.trim().toLowerCase();
      const src = S.deep ? (info && info.virtual ? info.flat : (all || []).filter(under(S.p)).concat(ACCTS.filter(under(S.p)))) : kids;
      base = src.filter((e) => (!ql || (e.name + ' ' + e.cid).toLowerCase().includes(ql)) && (S.size === 'any' || (e.kind !== 'folder' && sizeTest(e, S.size))));
      if (S.deep) base = once(base);
      counts = {};
      base.forEach((e) => { counts[e.kind] = (counts[e.kind] || 0) + 1; });
      let list = S.kind === 'all' ? base : base.filter((e) => e.kind === S.kind);
      if (S.deep && S.kind === 'all' && !ql && S.size === 'any') list = list.filter((e) => e.kind !== 'folder');
      const key = SORTS[S.sort] || SORTS.name, dir = S.dir;
      const keyed = list.map((e) => [S.deep ? 0 : e.kind === 'folder' ? 0 : 1, key(e), e.path, e]);
      keyed.sort((a, b) => a[0] - b[0] || (a[1] < b[1] ? -dir : a[1] > b[1] ? dir : a[2] < b[2] ? -1 : 1));
      shown = keyed.map((x) => x[3]);
      // Records start with the stores they come from: an account's repo.
      if (S.kind === 'record' && S.deep && !ql) shown = ACC.map((a) => a.path && (all || []).find((e) => e.path === a.path)).filter((e) => e && under(S.p)(e)).concat(shown);
      // Repos are folders and files of every kind, listed by path.
      if (S.kind === 'repo' && S.deep) {
        const at = new Map((all || []).map((e) => [e.path, e]));
        shown = REPOP.map((p) => at.get(p)).filter((e) => e && under(S.p)(e) && (!ql || (e.name + ' ' + e.cid).toLowerCase().includes(ql)));
      }
      // a grid of results groups them by folder
      if (S.view === 'grid' && S.deep && S.sort === 'name') shown = shown.map((e, i) => [e, i]).sort((a, b) => a[0].dir < b[0].dir ? -1 : a[0].dir > b[0].dir ? 1 : a[1] - b[1]).map((x) => x[0]);
      if (S.sel) { const i = shown.findIndex((e) => e.path === S.sel || (e.also && e.also.some((o) => o.path === S.sel))); if (i >= 0) { S.cur = i; S.limit = Math.max(S.limit, i + 50); } }
    }
    // A search shows each CID once; the other places it sits go in .also.
    function once(es) {
      const by = new Map(), out = [];
      for (const e of es) {
        const k = e.kind + ' ' + (e.cid || e.path), first = by.get(k);
        if (first) { first.also.push(e); continue; }
        const r = Object.assign({}, e, { also: [] });
        by.set(k, r); out.push(r);
      }
      return out;
    }
    const current = () => (S.view === 'tree' ? rows[S.cur] : shown[S.cur]) || null;

    // ---- render
    // Going to another folder, search or kind adds a history entry, so back
    // and forward step through them; moving the selection with the keys only
    // updates the current one. A click on a file adds one too.
    let lastURL = '', lastKey = null, pushSel = false;
    const urlOf = () => 'browse.html?' + qs({ p: S.p, q: S.q, kind: S.kind === 'all' ? '' : S.kind, deep: S.deep && !S.q ? '1' : '', size: S.size === 'any' ? '' : S.size, sort: S.sort === 'name' ? '' : S.sort, dir: S.dir < 0 ? '-1' : '', sel: S.sel });
    // A search being typed is one entry, not one per letter.
    const navKey = () => qs({ p: S.p, q: S.q ? '1' : '', kind: S.kind, deep: S.deep ? '1' : '' });
    function update(full) {
      compute();
      lib();
      $('.files').dataset.ly = S.ly;
      $('.files').dataset.view = S.view;
      main().querySelectorAll('[data-view]').forEach((b) => b.setAttribute('aria-pressed', b.dataset.view === S.view));
      main().querySelectorAll('[data-ly]').forEach((b) => b.setAttribute('aria-pressed', b.dataset.ly === S.ly));
      const r = rootAt(S.p);
      const title = S.p ? (r && r.kind !== 'mfs' ? rootLabel(r) : S.p.split('/').pop()) : 'data';
      $('.fcrumbs').innerHTML = crumbs(S.p) + (info ? `<span class="faint">${plural(info.n, 'item')} · ${bytes(info.bytes)}</span>${info.cid ? `<span class="fcid">${cidx(info.cid, true)}<a href="${esc(gw(info.cid))}" target="_blank" rel="noopener" title="Open this folder in an IPFS gateway">↗</a></span>` : ''}` : '');
      $('.scope').innerHTML = S.deep ? `<span class="pill on">In ${esc(S.p ? (title.length > 24 ? shortCID(title) : title) : REPOS ? 'all repos' : 'the whole archive')}${S.p ? ' and below' : ''}${S.p ? ` <button type="button" data-unscope aria-label="Search the whole archive">×</button>` : ''}</span>` : '';
      box.placeholder = S.deep ? 'Search names and CIDs' : `Search in ${S.p ? title.length > 24 ? shortCID(title) : title : 'data'} and below`;
      const chips = [['all', 'All', base.length]].concat(KINDS.filter((k) => counts[k]).map((k) => [k, KIND[k][3], counts[k]]));
      if (S.kind !== 'all' && !counts[S.kind]) chips.push([S.kind, kindOf(S.kind)[3], 0]);
      $('.chips').innerHTML = chips.map((c) => `<button type="button" class="pill" data-k="${c[0]}" aria-pressed="${c[0] === S.kind}">${esc(c[1])} <span class="n">${num(c[2])}${S.deep && loadingAll ? '+' : ''}</span></button>`).join('');
      $('#fdeep').checked = S.deep;
      $('#fsize').value = S.size; $('#fsort').value = S.sort; $('#fflip').textContent = S.dir > 0 ? '▲' : '▼';
      if (S.view === 'grid') grid(); else if (S.view === 'tree') tree(); else if (S.view === 'cols' && !S.deep) cols(); else listView();
      gen();
      findParts((S.view === 'tree' ? rows : shown).slice(0, S.limit), () => update(true));
      preview(full);
      settle(title);
    }
    // The URL, title and status line, after a change.
    function settle(title) {
      if (title == null) { const r = rootAt(S.p); title = S.p ? (r && r.kind !== 'mfs' ? rootLabel(r) : S.p.split('/').pop()) : 'data'; }
      const u = urlOf(), key = navKey();
      if (u !== lastURL) {
        if (lastKey !== null && (key !== lastKey || pushSel)) history.pushState(null, '', u); else history.replaceState(null, '', u);
        lastURL = u; lastKey = key;
      }
      pushSel = false;
      D.title = (S.q ? `“${S.q}”` : S.deep && !S.p && S.kind !== 'all' ? kindOf(S.kind)[3] : title.length > 40 ? shortCID(title) : title) + ' · ipfs2fil';
      status((S.deep ? `${num(shown.length)}${loadingAll ? '+' : ''} found` : showPath(S.p) || 'data') + (current() ? ' · ' + current().name : ''));
    }
    const more = () => shown.length > S.limit ? `<div class="foot"><button type="button" class="more" data-more>Show ${num(Math.min(300, shown.length - S.limit))} more</button><span>${num(S.limit)} of ${num(shown.length)}</span></div>` : '';
    const empty = () => `<p class="muted pad">${S.deep && loadingAll ? 'Searching…' : S.q ? 'No names match.' : S.kind !== 'all' ? `No ${esc(kindOf(S.kind)[3].toLowerCase())} here.` : 'This folder is empty.'}</p>`;
    // Book covers too, which explore reads out of EPUBs; a book with none
    // falls back to the icon (see the error listener below).
    // explore makes thumbnails of pictures, videos, sound with cover art and
    // documents (thumbnails.go); a file it has none for falls back to its
    // icon, or for a video to a frame the browser takes (viewers.js). From a
    // gateway the browser takes video frames itself, and grid tiles of text
    // show their first lines.
    const THUMBS = new Set(['image', 'video', 'audio', 'doc']);
    const thumbOr = (e, cls) => {
      const w = cls === 'tt' ? 320 : 96, badge = PARTS[e.cid] ? '<span class="partial" title="Only part of this file is here">partial</span>' : '';
      if (e.kind === 'post' || e.kind === 'account') {
        const P = e.kind === 'post' ? postOf(e) : null, K = P ? P.K : e.kind === 'account' ? kitOf(e.acct) : null;
        const u = P ? P.K.picture(P.x, w) : K && K.p.avatar ? K.blob(K.p.avatar, 'avatar', LOCAL ? { w } : null) : null;
        if (u && canShow()) return `<img class="${cls}${e.kind === 'account' ? ' av' : ''}" loading="lazy" alt="" data-k="${e.kind}" src="${esc(u)}">`;
        if (P && cls === 'tt') return `<span class="${cls} ptxt">${esc(P.K.snippet(P.x))}</span>`;
      }
      if (PARTS[e.cid] && e.kind !== 'image') return `<span class="${cls} ic">${icon(e.kind)}</span>` + badge;
      // files read out of an archive have no CID: they go by path
      const c = e.cid || e.path, mine = canShow() || e.blob;
      if (((e.kind === 'image' && mine) || (LOCAL && THUMBS.has(e.kind) && !e.blob)) && !noThumb.has(c)) return `<img class="${cls}" loading="lazy" alt="" data-k="${esc(e.kind)}" data-c="${esc(c)}" src="${esc(thumbURL(e, w))}">` + badge;
      if (V && mine && e.kind === 'video') return `<span class="${cls} ic" data-gen="v" data-c="${esc(c)}">${icon(e.kind)}</span>`;
      if (V && mine && cls === 'tt' && (e.kind === 'text' || e.kind === 'code') && e.bytes) return `<span class="${cls} ic" data-gen="t" data-c="${esc(c)}">${icon(e.kind)}</span>`;
      return `<span class="${cls} ic">${icon(e.kind)}</span>`;
    };
    // Thumbnails that failed once show their icon from then on, so drawing
    // the list again does not flash them.
    const noThumb = new Set();
    const byCID = (c) => shown.find((e) => (e.cid || e.path) === c) || rows.find((e) => (e.cid || e.path) === c);
    D.addEventListener('error', (ev) => {
      const x = ev.target;
      if (x.localName !== 'img' || !x.dataset || !x.dataset.k || x.dataset.k === 'image' || !x.closest('.flist')) return;
      if (x.dataset.c) noThumb.add(x.dataset.c);
      x.outerHTML = `<span class="${x.className} ic"${x.dataset.k === 'video' && V ? ` data-gen="v" data-c="${esc(x.dataset.c)}"` : ''}>${icon(x.dataset.k)}</span>`;
      gen();
    }, true);
    // Made in the browser, once each: blob URLs of video frames, text heads.
    const made = new Map();
    const io = 'IntersectionObserver' in window ? new IntersectionObserver((xs) => xs.forEach((x) => { if (x.isIntersecting) { io.unobserve(x.target); fillGen(x.target); } }), { rootMargin: '200px' }) : null;
    function gen() { if (io) setTimeout(() => main().querySelectorAll('.flist [data-gen]:not([data-seen])').forEach((el) => { el.dataset.seen = 1; io.observe(el); }), 0); }
    async function fillGen(el) {
      const e = byCID(el.dataset.c), url = e && contentURL(e);
      if (!url) return;
      const key = el.dataset.gen + ' ' + (e.cid || e.path);
      if (!made.has(key)) {
        made.set(key, el.dataset.gen === 'v'
          ? V.frame(url, 320).then((b) => {
            if (!b) return null;
            // explore keeps what the browser made, for next time
            if (LOCAL && e.cid) fetch('../_i2f/thumb/' + encodeURIComponent(e.cid) + '?w=320', { method: 'POST', headers: { 'X-I2F-Thumb': '1' }, body: b }).catch(() => {});
            return URL.createObjectURL(b);
          })
          : V.lines((n) => head(url, n)).catch(() => null));
      }
      const r = await made.get(key);
      if (!r || !el.isConnected) return;
      if (el.dataset.gen === 'v') el.outerHTML = `<img class="${el.className.replace(/\bic\b/, '')}" alt="" src="${esc(r)}">`;
      else { el.classList.add('txt'); el.textContent = r; }
    }
    const where = (e) => showPath(e.dir) || 'data';
    function listView() {
      const deepCols = S.deep;
      $('.flist').innerHTML = shown.length ? `<table><thead><tr><th>Name</th>${deepCols ? '<th class="w">Folder</th>' : ''}<th class="k">Kind</th><th class="r">Size</th><th class="r pc">Piece</th></tr></thead><tbody>${shown.slice(0, S.limit).map((e, i) => `<tr data-i="${i}" aria-selected="${i === S.cur}"><td><span class="nm">${thumbOr(e, 'th')}<a href="${esc(isDir(e) ? browseHref(e.path) : '#')}" data-open="${i}">${esc(label(e))}${e.kind === 'folder' ? '/' : ''}</a></span></td>${deepCols ? `<td class="w"><a class="muted" href="${esc(browseHref(e.dir))}">${esc(where(e))}/</a>${e.also && e.also.length ? ` <span class="faint">+${num(e.also.length)}</span>` : ''}</td>` : ''}<td class="k muted">${esc(kindOf(e.kind)[2])}</td><td class="r">${e.kind === 'folder' && !e.bytes ? '' : bytes(e.bytes)}</td><td class="r pc">${e.piece >= 0 ? `<a href="pieces.html?n=${e.piece}">${e.piece}</a>` : ''}</td></tr>`).join('')}</tbody></table>${more()}` : empty();
    }
    function grid() {
      let out = '', lastDir = null;
      const group = S.deep && S.sort === 'name', list = shown;
      list.slice(0, S.limit).forEach((e, i) => {
        if (group && e.dir !== lastDir) { out += `${lastDir !== null ? '</div>' : ''}<div class="gh"><a href="${esc(browseHref(e.dir))}">${esc(where(e))}</a></div><div class="tiles">`; lastDir = e.dir; }
        else if (!group && i === 0) out += '<div class="tiles">';
        out += `<button type="button" class="tile" data-i="${i}" aria-selected="${i === S.cur}" title="${esc(label(e))}">${thumbOr(e, 'tt')}<span class="tn">${esc(label(e))}</span></button>`;
      });
      $('.flist').innerHTML = list.length ? out + '</div>' + more() : empty();
    }
    // Tree: the folder's contents, folders opening in place; when searching,
    // only the branches that hold matches, all open.
    function tree() {
      rows = [];
      if (S.deep) {
        const keepDirs = new Set();
        shown.forEach((e) => { for (let d = e.dir; d && d !== S.p && d.startsWith(S.p); d = d.includes('/') ? d.slice(0, d.lastIndexOf('/')) : '') keepDirs.add(d); });
        const byDir = {};
        shown.forEach((e) => { (byDir[e.dir] = byDir[e.dir] || []).push(e); });
        (all || []).forEach((e) => { if (keepDirs.has(e.path)) (byDir[e.dir] = byDir[e.dir] || []).push(e); });
        const below = {};
        shown.forEach((e) => { for (let d = e.dir; d && d !== S.p; d = d.includes('/') ? d.slice(0, d.lastIndexOf('/')) : '') below[d] = (below[d] || 0) + 1; });
        const nOf = (d) => below[d] || 0;
        const walk = (d, depth) => (byDir[d] || []).sort((a, b) => (a.kind === 'folder' ? 0 : 1) - (b.kind === 'folder' ? 0 : 1) || (a.name < b.name ? -1 : 1)).forEach((e) => {
          if (rows.length > S.limit) return;
          const isDir = keepDirs.has(e.path);
          rows.push({ ...e, depth, open: isDir, n: isDir ? nOf(e.path) : 0, branch: isDir });
          if (isDir) walk(e.path, depth + 1);
        });
        walk(S.p, 0);
      } else {
        const walk = (list, depth) => list.forEach((e) => {
          const open = S.open.has(e.path);
          rows.push({ ...e, depth, open, branch: isDir(e) });
          if (open && TREE[e.path]) walk(TREE[e.path], depth + 1);
        });
        walk(shown, 0);
      }
      if (S.sel) { const i = rows.findIndex((e) => e.path === S.sel); if (i >= 0) S.cur = i; }
      $('.flist').innerHTML = rows.length ? `<div class="tree" role="tree">${rows.slice(0, S.limit + 200).map((e, i) => `<div class="tr" role="treeitem" data-i="${i}" aria-selected="${i === S.cur}"${e.branch ? ` aria-expanded="${e.open}"` : ''} style="padding-left:${8 + e.depth * 18}px"><button type="button" class="tw" data-tw="${i}" tabindex="-1" aria-hidden="true">${e.branch ? (e.open ? '▾' : '▸') : ''}</button>${thumbOr(e, 'th')}<span class="tnm">${esc(label(e))}</span><span class="n">${e.n ? num(e.n) : e.kind === 'folder' && !e.bytes ? '' : bytes(e.bytes)}</span></div>`).join('')}</div>` : empty();
    }
    const TREE = {};
    async function toggle(e) {
      if (!isDir(e) || S.deep) return;
      if (S.open.has(e.path)) S.open.delete(e.path);
      else {
        S.open.add(e.path);
        if (!TREE[e.path]) {
          const di = await dirInfo(e.path);
          TREE[e.path] = di ? (await listDir(di)).map(kindFix).sort((a, b) => (a.kind === 'folder' ? 0 : 1) - (b.kind === 'folder' ? 0 : 1) || (a.name.toLowerCase() < b.name.toLowerCase() ? -1 : 1)) : [];
          fixKinds(TREE[e.path], () => { TREE[e.path].forEach(kindFix); update(); });
        }
      }
      update();
    }
    // Columns: one per folder from data/ down to here, then the selection.
    let COLS = [];
    async function cols() {
      const parts = S.p ? S.p.split('/') : [];
      const want = [''].concat(parts.map((_, i) => parts.slice(0, i + 1).join('/')));
      const key = want.join('\n');
      if (COLS.key !== key) {
        $('.flist').innerHTML = '<p class="muted pad">Loading…</p>';
        const got = await Promise.all(want.slice(0, -1).map(async (d) => { const di = await dirInfo(d); return di ? (await listDir(di)).sort((a, b) => (a.kind === 'folder' ? 0 : 1) - (b.kind === 'folder' ? 0 : 1) || (a.name.toLowerCase() < b.name.toLowerCase() ? -1 : 1)) : []; }));
        COLS = got.map((items, i) => ({ dir: want[i], items, cur: items.findIndex((e) => e.path === want[i + 1]) }));
        COLS.key = key;
      }
      const last = { dir: S.p, items: shown, cur: S.cur };
      const all = COLS.concat([last]);
      $('.flist').innerHTML = `<div class="cols">${all.map((c, ci) => `<div class="col" data-c="${ci}">${c.items.slice(0, ci === all.length - 1 ? S.limit : 2000).map((e, i) => `<div class="ci" data-c="${ci}" data-i="${i}" aria-selected="${i === c.cur}">${icon(e.kind)}<span class="tnm">${esc(e.name)}</span>${isDir(e) ? '<span class="n">›</span>' : ''}</div>`).join('') || '<p class="muted pad">Empty</p>'}</div>`).join('')}</div>`;
      const c = $('.cols');
      c.scrollLeft = c.scrollWidth;
    }

    // ---- full screen: the preview pane (header and stage, not the facts)
    // fills the screen, and moving through the list keeps it there. Browsers
    // with no Fullscreen API for elements (iPhone Safari) get the pane laid
    // over the window instead.
    const fsEl = () => D.fullscreenElement || D.webkitFullscreenElement || null;
    const isFull = () => { const p = $('.fprev'); return !!p && p.classList.contains('full'); };
    const fullBtn = () => { const on = isFull(); return `<button type="button" class="ib fs" data-full aria-pressed="${on}" aria-label="${on ? 'Exit full screen' : 'Full screen'} (F)" title="${on ? 'Exit full screen' : 'Full screen'} (F)">${on ? svg('M6 2v4H2M10 2v4h4M6 14v-4H2M10 14v-4h4') : svg('M2 6V2h4M14 6V2h-4M2 10v4h4M14 10v4h-4')}</button>`; };
    function syncFull(on) {
      const p = $('.fprev');
      if (!p) return;
      p.classList.toggle('full', on);
      const b = p.querySelector('[data-full]');
      if (b) b.outerHTML = fullBtn();
    }
    function full(on) {
      const p = $('.fprev');
      if (!p) return false;
      if (on == null) on = !isFull();
      const req = p.requestFullscreen || p.webkitRequestFullscreen, exit = D.exitFullscreen || D.webkitExitFullscreen;
      if (on && req) { const r = req.call(p); if (r && r.catch) r.catch(() => syncFull(true)); }
      else if (!on && fsEl() && exit) exit.call(D);
      else syncFull(on);
      return true;
    }
    const onFs = () => syncFull(fsEl() === $('.fprev'));
    D.addEventListener('fullscreenchange', onFs);
    D.addEventListener('webkitfullscreenchange', onFs);

    // ---- the preview pane
    let shownPrev = null;
    async function preview(full) {
      const e = current(), pane = $('.fprev .pv');
      const key = e ? e.path + e.kind : 'dir:' + S.p;
      if (!full && key === shownPrev) return;
      shownPrev = key;
      deck.hide();
      if (!e) return folderPane(pane, info, S.p, true);
      if (e.kind === 'folder') return folderPane(pane, await dirInfo(e.path), e.path, false, e);
      if (e.kind === 'account') return accountPane(pane, e);
      const P = postOf(e);
      const url = contentURL(e), copies = m.copies || [];
      const piece = e.piece >= 0 ? (m.pieces || [])[e.piece] : null;
      const stored = piece ? copies.filter((c) => c.pieceIds && c.pieceIds[e.piece] != null) : [];
      const nav = `<button type="button" class="ib" data-step="-1" aria-label="Previous (k)">‹</button><button type="button" class="ib" data-step="1" aria-label="Next (j)">›</button>`;
      pane.innerHTML = `<div class="ph"><strong title="${esc(e.name)}">${P ? `${P.K.name} · ${esc(P.x.t ? day(P.x.t) : e.name)}` : esc(e.name)}</strong>${nav}${P ? `<a class="btn sm" href="${esc(accHref(P.n, P.x.k))}">Open in Bluesky App</a>` : ''}${isDir(e) ? `<a class="btn sm" href="${esc(browseHref(e.path))}" data-enter title="Open the archive like a folder (Enter)">Open</a>` : ''}${url ? `<a class="btn sm" href="${esc(contentURL(e, { dl: 1 }))}" download="${esc(e.name)}">${P ? 'Record' : 'Download'}</a>` : ''}${url ? fullBtn() : ''}</div>
<div class="stage" data-kind="${esc(e.kind)}">${P ? `<div class="at-thread">${P.K.thread(P.x).html}</div>` : stage(e, url)}</div>
<div class="facts2">
  <div><span>Size</span><b>${bytes(e.bytes)}</b></div><div><span>Kind</span><b>${esc(kindOf(e.kind)[2])}${ext(e.name) ? ' · ' + esc(ext(e.name).toUpperCase()) : ''}</b></div>
  <div><span>Piece</span><b>${e.piece >= 0 ? `<a href="pieces.html?n=${e.piece}">#${e.piece}</a>` : '—'}</b></div><div><span>Copies</span><b>${stored.length || (piece ? '0' : '—')}</b></div>
</div>
${e.va ? arcFacts(e) : `<div class="facts dl">
  <span>CID</span><span>${cidx(e.cid)} <button type="button" class="link" data-copy="${esc(e.cid)}">Copy</button></span>
  <span>Open in</span><span class="gw">${HERE ? `<a href="${esc(HERE + e.cid)}" target="_blank" rel="noopener">this gateway ↗</a>` : ''}${GATEWAYS.map((g) => `<a href="${esc(g[1] + e.cid + '?filename=' + encodeURIComponent(e.name))}" target="_blank" rel="noopener">${g[0]} ↗</a>`).join('')}<a href="${esc(gw(e.cid) + '?format=car')}" target="_blank" rel="noopener">CAR</a></span>
  <span>In${e.also && e.also.length ? ` (${num(e.also.length + 1)})` : ''}</span>${places(e)}
  <span>Folder</span><span class="pdir faint">…</span>
  ${piece ? `<span>Stored</span><span>${cidx(piece.pieceCid, true)} ${stored.length ? `<span class="tags">${stored.map((c) => `<span class="tag">${esc(c.provider)}</span>`).join('')}</span>` : '<span class="faint">not stored yet</span>'}</span>` : ''}
</div>`}`;
      if (P) lightbox(pane); else render(pane, e, url);
      warmAround();
      const di = await dirInfo(e.dir), pd = pane.querySelector('.pdir');
      if (pd && current() === e) pd.outerHTML = di && di.cid ? `<span>${cidx(di.cid, true)} <span class="gw">${GATEWAYS.map((g) => `<a href="${esc(g[1] + di.cid)}" target="_blank" rel="noopener">${g[0]} ↗</a>`).join('')}</span></span>` : '<span class="faint">—</span>';
    }
    function render(pane, e, url) {
      fill(pane, e, url);
      if (isPDF(e) && url) {
        if (url.startsWith('blob:')) pane.querySelector('.stage').innerHTML = `<iframe title="${esc(e.name)}" src="${esc(url)}"></iframe>`;
        else if (pdfInline) deck.show(url, pane.querySelector('.stage'), e.name);
        else pane.querySelector('.stage').innerHTML = `<div class="none">${icon('doc')}<p>This browser saves PDFs instead of showing them. Use Download to open this one.</p></div>`;
      }
      if (isEPUB(e) && url) book.show(pane.querySelector('.stage'), url, e);
    }
    // A file unpacked from a .gz, shown in el as if it were the one selected
    // (viewers.js, the gz viewer).
    function showIn(el, e) {
      const url = contentURL(e);
      el.innerHTML = `<div class="stage" data-kind="${esc(e.kind)}">${stage(e, url)}</div>`;
      render(el, e, url);
    }
    // Where a file read out of an archive came from.
    function arcFacts(e) {
      const a = e.va.arc, ad = a.includes('/') ? a.slice(0, a.lastIndexOf('/')) : '';
      return `<div class="facts dl">
  <span>In</span><span><a href="${esc('browse.html?' + qs({ p: ad, sel: a }))}" title="${esc(showPath(a) || a)}">${esc(a.split('/').pop())}</a> <span class="faint">${e.va.gz ? 'unpacked here' : 'read out of the archive here'}</span></span>
  ${e.va.gz ? '' : `<span>Path</span><span>${esc(e.va.inner)}</span>`}
  ${e.time ? `<span>Modified</span><span>${esc(new Date(e.time).toLocaleString('en-GB', { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit', timeZone: 'UTC' }))} UTC</span>` : ''}
  ${e.note ? `<span>Note</span><span class="faint">${esc(e.note)}</span>` : ''}
</div>`;
    }
    // An account: its profile and what the archive holds of it.
    function accountPane(pane, e) {
      const i = e.acct, a = ACC[i], K = kitOf(i);
      pane.innerHTML = `<div class="ph"><strong title="${esc(a.name)}">${esc(a.name)}</strong><button type="button" class="ib" data-step="-1" aria-label="Previous (k)">‹</button><button type="button" class="ib" data-step="1" aria-label="Next (j)">›</button><a class="btn sm" href="${esc(accHref(i))}">Open in Bluesky App</a></div>
<div class="stage acct" data-kind="account">${K ? `<section class="at-profile">${K.profile()}</section>` : ''}
<div class="pad2"><p class="faint">A Bluesky account. Its repo is IPLD: the signed commit, records and media are stored as IPFS blocks${a.path ? `, shown as <a href="${esc(browseHref(a.path))}">a folder of records and blobs</a>` : ''}.</p>${archivedHTML(a, K ? K.a : null, a.path ? browseHref(a.path) : '')}</div></div>`;
      lightbox(pane);
    }
    // Every folder the file sits in, under its name there.
    function places(e) {
      const all = [e].concat(e.also || []).sort((a, b) => a.path < b.path ? -1 : 1), cap = 8;
      const link = (o) => `<a href="${esc('browse.html?' + qs({ p: o.dir, sel: o.path }))}" title="${esc(showPath(o.path) || o.path)}">${esc(where(o))}/${o.name !== e.name ? esc(o.name) : ''}</a>`;
      if (all.length === 1) return link(e);
      const rest = all.length > cap ? `<details><summary class="faint">${num(all.length - cap)} more</summary>${all.slice(cap).map(link).join('')}</details>` : '';
      return `<span class="places">${all.slice(0, cap).map(link).join('')}${rest}</span>`;
    }
    function stage(e, url) {
      if (!url) return `<div class="none"><p>Open this explorer with <code>ipfs2fil explore</code> or through an IPFS gateway to see the file here.</p><button type="button" class="btn sm" data-public>Load from ${GATEWAYS[0][0]}</button></div>`;
      const x = ext(e.name);
      if (PARTS[e.cid]) return `<div class="none">${icon(e.kind)}<p>Only ${num(PARTS[e.cid][0])} of the ${num(PARTS[e.cid][1])} parts of this file are in the pieces: the IPFS repo it was packed from did not have the rest, so it cannot be shown.</p></div>`;
      if (V && V.find(e, x)) return '<p class="muted pad">Loading…</p>';
      switch (e.kind) {
        // Photos the browser cannot show (HEIC, TIFF, raw) come as large thumbnails.
        case 'image': return `<img src="${esc(LOCAL && /^(heic|heif|tiff?|cr2|nef|arw|dng|raw|psd)$/.test(x) ? thumbURL(e, 2048) : url)}" alt="${esc(e.name)}">`;
        case 'video': return `<video src="${esc(url)}" controls preload="metadata"></video>`;
        case 'audio': return `<div class="none">${LOCAL ? `<img class="cover" alt="" src="${esc(thumbURL(e, 640))}" onerror="this.remove()">` : icon('audio')}<audio src="${esc(url)}" controls preload="metadata"></audio></div>`;
        case 'page': return '<iframe sandbox="allow-scripts allow-forms allow-popups allow-modals allow-pointer-lock" title="' + esc(e.name) + '"></iframe>';
        case 'doc': return x === 'pdf' || !x ? '' : x === 'epub' ? '<p class="muted pad">Opening the book…</p>' : `<div class="none">${icon('doc')}<p>No preview for .${esc(x)} files.</p></div>`;
        case 'text': case 'code': return '<pre class="code"><code>Loading…</code></pre>';
        case 'folder': return '';
      }
      return '<pre class="hex">Loading…</pre>';
    }
    // The first n bytes of url, kept for the last few files so that moving
    // back and forth, and the prefetch below, do not ask again.
    const heads = new Map();
    function head(url, n) {
      const k = n + ' ' + url;
      let p = heads.get(k);
      if (!p) {
        // (blob URLs, for files read out of archives, take no Range)
        // (no-store: a browser that keeps this part in its cache may later hand
        // it back for the whole file, which then will not open)
        p = url.startsWith('blob:') ? fetch(url).then((r) => r.arrayBuffer()).then((b) => b.slice(0, n)) : fetch(url, { headers: { Range: 'bytes=0-' + (n - 1) }, cache: 'no-store' }).then((r) => {
          if (!r.ok) throw new Error(missing(null, r));
          return r.arrayBuffer();
        });
        p.catch(() => heads.delete(k));
        heads.set(k, p);
        if (heads.size > 40) heads.delete(heads.keys().next().value);
      }
      return p;
    }
    // Prefetch the files around the cursor (up and down a list, every way in
    // a grid) so the preview is ready when the cursor gets there. Images and
    // documents go into the browser's cache; text and other files into heads.
    const warmed = new Set();
    let warmT = 0;
    function warmAround() {
      clearTimeout(warmT);
      if (!canShow() || S.view === 'tree') return;
      warmT = setTimeout(() => {
        const a = across(), offs = a > 1 ? [1, -1, a, -a, 2, -2, a + 1, a - 1, 1 - a, -a - 1] : [1, -1, 2, -2];
        // PDFs next to the cursor (the nearest four) load in the deck.
        if (pdfInline) deck.keep(offs.map((d) => shown[S.cur + d]).filter((e) => isPDF(e) && e.bytes <= 64 << 20 && contentURL(e)).slice(0, 4).map((e) => [contentURL(e), e.name]));
        for (const d of offs) {
          const e = shown[S.cur + d], url = e && contentURL(e);
          if (!url || e.kind === 'folder' || warmed.has(url)) continue;
          if (isPDF(e)) continue;
          const doc = e.kind === 'page' || isEPUB(e);
          if (e.kind === 'text' || e.kind === 'code') head(url, 256 << 10).catch(() => {});
          else if (e.kind === 'image' && e.bytes <= 32 << 20) new Image().src = url;
          else if (doc && e.bytes <= 64 << 20) fetch(url, { priority: 'low' }).then((r) => r.arrayBuffer()).catch(() => {});
          else if (!['image', 'video', 'audio', 'doc', 'page'].includes(e.kind)) head(url, 512).catch(() => {});
          else continue;
          warmed.add(url);
        }
      }, 120);
    }
    // PDFs show in iframes kept in a deck laid over the stage, so the ones
    // next to the cursor can load hidden and be shown without starting the
    // browser's PDF viewer again. An iframe reloads when moved in the page,
    // so the deck stays put and only its frames come and go.
    const isPDF = (e) => e && e.kind === 'doc' && (ext(e.name) === 'pdf' || !ext(e.name));
    // Chrome set to download PDFs (and other browsers with no viewer) would
    // save every PDF the deck loads, so then PDFs are not shown at all.
    const pdfInline = navigator.pdfViewerEnabled !== false;
    const deck = (() => {
      const frames = new Map(); // url -> iframe, oldest first
      let on = null, stageEl = null;
      const el = () => $('.fprev .deck');
      const place = () => {
        const d = el();
        if (!d || !stageEl || !stageEl.isConnected) return;
        d.style.top = stageEl.offsetTop + 'px';
        d.style.height = stageEl.clientHeight + 'px';
      };
      const ro = 'ResizeObserver' in window ? new ResizeObserver(place) : null;
      // A frame saves its file as a download instead of showing it when the
      // server does not call it a PDF (static hosts such as the demo on
      // GitHub Pages send application/octet-stream), so the bytes are
      // checked first and, if need be, shown from a blob typed as a PDF.
      // f.ready is the URL in the frame, or null when it is not a PDF.
      function load(url) {
        return (url.startsWith('blob:') ? Promise.resolve({ ok: false }) : fetch(url, { method: 'HEAD' })).then((r) => {
          if (r.ok && /^application\/pdf/i.test(r.headers.get('content-type') || '')) return url;
          return fetch(url).then((r) => r.ok ? r.blob() : null).then(async (b) => b && await b.slice(0, 5).text() === '%PDF-' ? URL.createObjectURL(new Blob([b], { type: 'application/pdf' })) : null);
        }).catch(() => null);
      }
      function frame(url, title) {
        let f = frames.get(url);
        if (f) { frames.delete(url); frames.set(url, f); return f; }
        f = D.createElement('iframe');
        f.title = title || ''; f.tabIndex = -1;
        el().appendChild(f);
        frames.set(url, f);
        f.ready = load(url).then((src) => {
          if (src && src.startsWith('blob:')) f.blob = src;
          if (src && f.isConnected) f.src = src;
          else if (f.blob) URL.revokeObjectURL(f.blob);
          return src;
        });
        return f;
      }
      const drop = (f, u) => { f.remove(); frames.delete(u); if (f.blob) URL.revokeObjectURL(f.blob); };
      return {
        show(url, st, title) {
          const d = el();
          if (!d || !st) return;
          stageEl = st;
          if (ro) { ro.disconnect(); ro.observe(st); }
          place();
          const f = on = frame(url, title);
          on.tabIndex = 0;
          frames.forEach((f) => f.classList.toggle('on', f === on));
          d.hidden = false;
          f.ready.then((src) => { if (!src && on === f && st.isConnected) { st.innerHTML = `<div class="none">${icon('doc')}<p>This file is not a PDF the browser can show.</p></div>`; this.hide(); } });
        },
        hide() {
          if (on) { on.classList.remove('on'); on.tabIndex = -1; }
          on = null;
          if (ro) ro.disconnect();
        },
        // Load these hidden and drop the rest, beyond the one showing.
        keep(urls) {
          if (!el()) return;
          urls.forEach((u) => frame(u[0], u[1]));
          const want = new Set(urls.map((u) => u[0]));
          frames.forEach((f, u) => { if (f !== on && !want.has(u)) drop(f, u); });
        },
      };
    })();
    // EPUBs are zip files of XHTML chapters. The book opens on a title page
    // (cover, title, author, contents) and shows one chapter at a time in a
    // frame that runs no scripts, with its pictures and stylesheets read out
    // of the zip. The frame shares the explorer's origin only so this page
    // can follow the book's links and keys in it.
    const isEPUB = (e) => e && e.kind === 'doc' && ext(e.name) === 'epub';
    const book = (() => {
      const books = new Map(); // url -> promise of a book, the last few
      const at = new Map(); // url -> [chapter, scroll] last shown, chapter -1 for the title page
      let cur = null, gen = 0;
      const tags = (d, n) => d ? Array.from(d.getElementsByTagNameNS('*', n)) : [];
      // href relative to the zip entry base: [entry, fragment], or null when
      // it points outside the book.
      const join = (base, href) => {
        const u = new URL(href || '', 'http://z/' + base.split('/').map(encodeURIComponent).join('/'));
        if (u.host !== 'z') return null;
        try { return [decodeURIComponent(u.pathname.slice(1)), decodeURIComponent(u.hash.slice(1))]; } catch (e) { return null; }
      };
      // The zip's directory, and a function that reads one entry.
      function unzip(buf) {
        const v = new DataView(buf), files = new Map(), dec = new TextDecoder();
        let end = -1;
        for (let i = buf.byteLength - 22; i >= Math.max(0, buf.byteLength - 65557); i--) if (v.getUint32(i, true) === 0x06054b50) { end = i; break; }
        if (end < 0) throw new Error('This file is not a zip file, so it is not an EPUB the viewer can read.');
        let p = v.getUint32(end + 16, true);
        for (let n = v.getUint16(end + 10, true); n > 0 && p + 46 <= buf.byteLength && v.getUint32(p, true) === 0x02014b50; n--) {
          const nl = v.getUint16(p + 28, true);
          files.set(dec.decode(new Uint8Array(buf, p + 46, nl)), [v.getUint16(p + 10, true), v.getUint32(p + 20, true), v.getUint32(p + 42, true)]);
          p += 46 + nl + v.getUint16(p + 30, true) + v.getUint16(p + 32, true);
        }
        return async (name) => {
          const f = files.get(name);
          if (!f) return null;
          const [method, size, off] = f, start = off + 30 + v.getUint16(off + 26, true) + v.getUint16(off + 28, true);
          const raw = new Uint8Array(buf, start, Math.min(size, buf.byteLength - start));
          if (method === 0) return raw;
          if (method !== 8 || !('DecompressionStream' in window)) return null;
          return new Uint8Array(await new Response(new Blob([raw]).stream().pipeThrough(new DecompressionStream('deflate-raw'))).arrayBuffer());
        };
      }
      async function open(url) {
        const r = await fetch(url);
        if (!r.ok) throw new Error(missing(null, r));
        const get = unzip(await r.arrayBuffer()), dec = new TextDecoder();
        const doc = async (name, html) => {
          const b = await get(name);
          if (!b) return null;
          const s = dec.decode(b), p = new DOMParser();
          const d = p.parseFromString(s, html ? 'application/xhtml+xml' : 'application/xml');
          if (!d.getElementsByTagName('parsererror').length) return d;
          return html ? p.parseFromString(s, 'text/html') : null; // XHTML with HTML entities, or tag soup
        };
        const rf = tags(await doc('META-INF/container.xml'), 'rootfile')[0], opfPath = rf && rf.getAttribute('full-path');
        const opf = opfPath && await doc(opfPath);
        if (!opf) throw new Error('This EPUB has no package file the viewer can read.');
        const items = new Map(), types = new Map();
        for (const it of tags(opf, 'item')) {
          const j = join(opfPath, it.getAttribute('href'));
          if (!j) continue;
          const o = { path: j[0], type: it.getAttribute('media-type') || '', props: ' ' + (it.getAttribute('properties') || '') + ' ' };
          items.set(it.getAttribute('id'), o);
          types.set(o.path, o.type);
        }
        const all = [...items.values()];
        const chapters = tags(opf, 'itemref').map((r) => items.get(r.getAttribute('idref'))).filter((it) => it && /html/.test(it.type)).map((it) => it.path);
        if (!chapters.length) throw new Error('This EPUB has no chapters the viewer can read.');
        const meta = (n) => { const t = tags(opf, n)[0]; return t ? t.textContent.trim() : ''; };
        const coverMeta = tags(opf, 'meta').find((x) => x.getAttribute('name') === 'cover');
        const cover = all.find((it) => /^image\//.test(it.type) && it.props.includes(' cover-image ')) ||
          (coverMeta && items.get(coverMeta.getAttribute('content'))) || all.find((it) => /^image\//.test(it.type) && /cover/i.test(it.path));
        // Contents from the EPUB 3 nav document, or else the EPUB 2 NCX.
        const toc = [];
        const nav = all.find((it) => it.props.includes(' nav ')), ncx = items.get((tags(opf, 'spine')[0] || { getAttribute: () => '' }).getAttribute('toc')) || all.find((it) => it.type === 'application/x-dtbncx+xml');
        const depth = (el, n) => { let k = 0; for (let x = el.parentNode; x && x.nodeType === 1; x = x.parentNode) if (x.localName === n) k++; return k; };
        if (nav) {
          const d = await doc(nav.path, true), navs = tags(d, 'nav');
          const n = navs.find((x) => /\btoc\b/.test(x.getAttribute('epub:type') || x.getAttributeNS('http://www.idpf.org/2007/ops', 'type') || '')) || navs[0];
          for (const a of tags(n, 'a')) { const j = join(nav.path, a.getAttribute('href')); if (j) toc.push({ label: a.textContent.replace(/\s+/g, ' ').trim(), to: j, depth: depth(a, 'ol') - 1 }); }
        }
        if (!toc.length && ncx) {
          const d = await doc(ncx.path);
          for (const np of tags(d, 'navPoint')) {
            const c = tags(np, 'content')[0], t = tags(np, 'text')[0], j = c && join(ncx.path, c.getAttribute('src'));
            if (j) toc.push({ label: t ? t.textContent.replace(/\s+/g, ' ').trim() : '', to: j, depth: depth(np, 'navPoint') });
          }
        }
        const blobs = new Map();
        const blob = (path) => {
          let u = blobs.get(path);
          if (!u) blobs.set(path, u = get(path).then((b) => b ? URL.createObjectURL(new Blob([b], { type: types.get(path) || '' })) : ''));
          return u;
        };
        const free = () => blobs.forEach((u) => u.then((x) => x && URL.revokeObjectURL(x)));
        return { title: meta('title'), author: tags(opf, 'creator').map((x) => x.textContent.trim()).join(', '), language: meta('language'), chapters, toc, cover: cover && cover.path, get, doc, blob, free };
      }
      function load(url) {
        let p = books.get(url);
        if (p) { books.delete(url); books.set(url, p); return p; }
        p = open(url);
        p.catch(() => books.delete(url));
        books.set(url, p);
        while (books.size > 3) { const [k, old] = books.entries().next().value; books.delete(k); old.then((b) => b.free(), () => {}); }
        return p;
      }
      // url(...) in a stylesheet at base, pointed at the book's own files.
      async function css(b, text, base) {
        const found = [...text.matchAll(/url\(\s*(['"]?)([^'")]+)\1\s*\)/g)];
        const got = await Promise.all(found.map((m) => { const j = !/^(data|https?):/i.test(m[2]) && join(base, m[2]); return j ? b.blob(j[0]) : Promise.resolve(''); }));
        let i = 0;
        return text.replace(/url\(\s*(['"]?)([^'")]+)\1\s*\)/g, (m) => { const u = got[i++]; return u ? `url("${u}")` : m; }).replace(/<\/style/gi, '<\\/style');
      }
      const BASE = 'html{background:#FFFFFF;color:#1F2328}body{max-width:38em;margin:0 auto;padding:28px 32px 64px;font:17px/1.6 Georgia,"Iowan Old Style",serif}img,svg,video{max-width:100%;height:auto}a{color:#2459D6}pre{white-space:pre-wrap}';
      const attrs = (el) => el ? Array.from(el.attributes).filter((a) => !/^on/i.test(a.name)).map((a) => ` ${a.name}="${esc(a.value)}"`).join('') : '';
      // One chapter as a page: no scripts, and every file it uses read from
      // the zip. Links into the book carry where they go in data-to.
      async function chapter(b, path) {
        const d = await b.doc(path, true);
        if (!d) throw new Error('The viewer could not read this chapter.');
        d.querySelectorAll('script, iframe, frame, object, embed, form, base, meta, noscript').forEach((x) => x.remove());
        const styles = [];
        for (const x of Array.from(d.querySelectorAll('link, style'))) {
          if (x.localName === 'style') styles.push(css(b, x.textContent, path));
          else if (/\bstylesheet\b/i.test(x.getAttribute('rel') || '')) {
            const j = join(path, x.getAttribute('href'));
            styles.push(j ? b.get(j[0]).then((t) => t ? css(b, new TextDecoder().decode(t), j[0]) : '') : Promise.resolve(''));
          }
          x.remove();
        }
        const XL = 'http://www.w3.org/1999/xlink', work = [];
        for (const x of Array.from(d.querySelectorAll('*'))) {
          for (const a of Array.from(x.attributes)) if (/^on/i.test(a.name) || /^\s*javascript:/i.test(a.value)) x.removeAttribute(a.name);
          if (x.localName === 'a') {
            const h = x.getAttribute('href'), j = h && join(path, h);
            if (j) x.setAttribute('data-to', j.join('#'));
            continue;
          }
          const att = x.localName === 'image' ? (x.hasAttributeNS(XL, 'href') ? [XL, 'href'] : [null, 'href']) : ['img', 'source', 'video', 'audio', 'track'].includes(x.localName) ? [null, 'src'] : null;
          const src = att && x.getAttributeNS(att[0], att[1]), j = src && !/^(data|https?):/i.test(src) && join(path, src);
          if (j) work.push(b.blob(j[0]).then((u) => u && x.setAttributeNS(att[0], att[0] ? 'xlink:href' : att[1], u)));
          if (x.localName === 'img') x.removeAttribute('srcset');
        }
        const sheets = await Promise.all(styles);
        await Promise.all(work);
        const head = d.querySelector('head'), body = d.querySelector('body');
        return `<!doctype html><html${attrs(d.documentElement)}><head><meta charset="utf-8"><style>${BASE}</style>${sheets.map((s) => `<style>${s}</style>`).join('')}${head ? head.innerHTML : ''}</head><body${attrs(body)}>${body ? body.innerHTML : ''}</body></html>`;
      }
      async function titlePage(b) {
        const cover = b.cover ? await b.blob(b.cover) : '';
        const where = (t) => b.chapters.indexOf(t.to[0]);
        const toc = b.toc.filter((t) => where(t) >= 0);
        return `<!doctype html><html${b.language ? ` lang="${esc(b.language)}"` : ''}><head><meta charset="utf-8"><style>${BASE}
.tp{text-align:center;margin:8px 0 28px}.tp img{max-height:340px;box-shadow:0 2px 12px rgba(0,0,0,.25)}h1{font-size:1.6em;line-height:1.25;margin:.8em 0 .2em}.by{color:#59606B;margin:0}
ol{list-style:none;padding:0;margin:0;border-top:1px solid #E3E5E8}li a{display:block;padding:6px 4px;border-bottom:1px solid #E3E5E8;text-decoration:none}li a:hover{background:#F3F5F8}</style></head><body>
<div class="tp">${cover ? `<img src="${esc(cover)}" alt="Cover">` : ''}<h1>${esc(b.title || 'Untitled book')}</h1>${b.author ? `<p class="by">${esc(b.author)}</p>` : ''}</div>
<ol>${(toc.length ? toc : b.chapters.map((c, i) => ({ label: 'Section ' + (i + 1), to: [c, ''], depth: 0 }))).map((t) => `<li><a href="#" data-to="${esc(t.to.join('#'))}" style="padding-left:${4 + Math.max(0, t.depth) * 18}px">${esc(t.label || t.to[0].split('/').pop())}</a></li>`).join('')}</ol></body></html>`;
      }
      // Scroll the frame to the element named frag; scrollIntoView would
      // scroll the preview pane around the frame too.
      const jump = (d, frag) => {
        const t = d.getElementById(frag) || d.getElementsByName(frag)[0];
        if (t) d.scrollingElement.scrollTop += t.getBoundingClientRect().top;
      };
      // Show chapter i (-1 for the title page), at fragment frag or at scroll y.
      async function go(i, frag, y) {
        const c = cur, my = ++gen;
        if (!c || !c.st.isConnected) return;
        i = Math.max(-1, Math.min(c.b.chapters.length - 1, i));
        let html;
        try { html = i < 0 ? await titlePage(c.b) : await chapter(c.b, c.b.chapters[i]); } catch (err) { html = `<!doctype html><p style="font:14px system-ui;color:#59606B;padding:24px">${esc(err.message)}</p>`; }
        if (my !== gen || cur !== c) return;
        c.i = i;
        at.set(c.url, [i, 0]);
        const f = c.st.querySelector('iframe'), sel = c.st.querySelector('select');
        sel.value = String(i);
        c.st.querySelector('[data-bk="-1"]').disabled = i < 0;
        c.st.querySelector('[data-bk="1"]').disabled = i >= c.b.chapters.length - 1;
        f.onload = () => {
          const d = f.contentDocument;
          if (!d || cur !== c) return;
          if (frag) jump(d, frag); else if (y) d.scrollingElement.scrollTop = y;
          d.addEventListener('scroll', () => { if (cur === c && c.i === i) at.set(c.url, [i, d.scrollingElement.scrollTop]); }, { passive: true });
          d.addEventListener('click', (ev) => {
            const a = ev.target.closest && ev.target.closest('a[href], a[data-to]');
            if (!a) return;
            ev.preventDefault();
            const to = a.getAttribute('data-to');
            if (to != null) {
              const [p, h] = [to.slice(0, to.indexOf('#') < 0 ? to.length : to.indexOf('#')), to.indexOf('#') < 0 ? '' : to.slice(to.indexOf('#') + 1)];
              const k = c.b.chapters.indexOf(p);
              if (k === c.i && h) jump(d, h); else if (k >= 0) go(k, h);
            } else if (/^(https?|mailto):/i.test(a.getAttribute('href') || '')) window.open(a.getAttribute('href'), '_blank', 'noopener');
          });
          d.addEventListener('keydown', (ev) => {
            if (ev.metaKey || ev.ctrlKey || ev.altKey) return;
            if (ev.key === 'ArrowRight' || ev.key === ']') { ev.preventDefault(); go(c.i + 1); }
            else if (ev.key === 'ArrowLeft' || ev.key === '[') { ev.preventDefault(); go(c.i - 1); }
          });
        };
        f.srcdoc = html;
      }
      return {
        async show(st, url, e) {
          const want = e.path;
          cur = null;
          let b;
          try {
            if (e.bytes > 128 << 20) throw new Error(`This book is ${bytes(e.bytes)}, too big to open here. Download it to read it.`);
            b = await load(url);
          } catch (err) {
            if (st.isConnected) st.innerHTML = `<div class="none">${icon('doc')}<p>${esc(err.message)}</p></div>`;
            return;
          }
          if (!st.isConnected || (current() && current().path !== want)) return;
          const label = (c, i) => { const t = b.toc.find((x) => x.to[0] === c); return t && t.label ? t.label : 'Section ' + (i + 1); };
          st.innerHTML = `<div class="book"><div class="bkbar"><button type="button" class="ib" data-bk="-1" aria-label="Previous chapter ([)">‹</button><select aria-label="Chapter"><option value="-1">${esc(b.title || 'Title page')}</option>${b.chapters.map((c, i) => `<option value="${i}">${esc(label(c, i))}</option>`).join('')}</select><button type="button" class="ib" data-bk="1" aria-label="Next chapter (])">›</button></div><iframe sandbox="allow-same-origin" title="${esc(e.name)}"></iframe></div>`;
          cur = { st, url, b, i: -1 };
          st.querySelector('select').onchange = (ev) => go(+ev.target.value);
          st.querySelectorAll('[data-bk]').forEach((x) => { x.onclick = () => go(cur.i + +x.dataset.bk); });
          const last = at.get(url) || [-1, 0];
          go(last[0], '', last[1]);
        },
      };
    })();
    // Where a page's folder is served, so its relative links work: explore
    // serves folders by path (_i2f/path), as gateways do (../data/).
    // Pages go to the other name for this machine (localhost for
    // 127.0.0.1), another origin to the browser, so a page keeps its own
    // storage and fonts and cannot reach the explorer; see pageHost in
    // content.go.
    async function baseOf(e, page) {
      if (e.blob) return '';
      if (HERE) return contentURL(e).replace(/[^/]*$/, '');
      if (!LOCAL) return '';
      const di = await dirInfo(e.dir);
      if (!di || !di.cid) return '';
      const u = new URL('../_i2f/path/' + encodeURIComponent(di.cid) + '/', location.href);
      if (page && u.hostname === '127.0.0.1') u.hostname = 'localhost';
      return u.href;
    }
    // A page shown from its text, on a host that has its files only by CID
    // (the website's demo): its stylesheets, scripts and pictures are found
    // in the index by their paths relative to it and put in, as the page's
    // folder is not there for them to load from.
    async function pageDoc(e, html) {
      const d = new DOMParser().parseFromString(html, 'text/html');
      const find = async (rel, from) => {
        if (!rel || /^(?:[a-z][a-z0-9+.-]*:|\/\/|#)/i.test(rel.trim())) return null;
        const u = new URL(rel.trim(), 'http://z/' + segs(from));
        if (u.host !== 'z') return null;
        let p;
        try { p = decodeURIComponent(u.pathname.slice(1)); } catch (err) { return null; }
        const di = await dirInfo(p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : '');
        const x = di && (await listDir(di)).find((k) => k.path === p);
        return x && x.kind !== 'folder' ? x : null;
      };
      const abs = (x) => { const c = contentURL(x); return c ? new URL(c, location.href).href : ''; };
      const text = async (x) => { const c = contentURL(x), r = c && await fetch(c).catch(() => null); return r && r.ok ? r.text() : ''; };
      const URLS = /url\(\s*(['"]?)([^'")]+)\1\s*\)/g;
      const css = async (t, from) => {
        const got = await Promise.all([...t.matchAll(URLS)].map((m) => find(m[2], from).then((x) => x ? abs(x) : '')));
        let i = 0;
        return t.replace(URLS, (m) => { const u = got[i++]; return u ? `url("${u}")` : m; }).replace(/<\/style/gi, '<\\/style');
      };
      const work = [];
      for (const x of Array.from(d.querySelectorAll('link[href], style, script[src], img[src], source[src], video[src], video[poster], audio[src], input[src], [style]'))) {
        if (x.localName === 'link') {
          const rel = (x.getAttribute('rel') || '').toLowerCase();
          if (/\bstylesheet\b/.test(rel)) work.push(find(x.getAttribute('href'), e.path).then(async (k) => { if (!k) return; const t = await text(k); const st = d.createElement('style'); st.textContent = await css(t, k.path); x.replaceWith(st); }));
          else if (/\bicon\b/.test(rel)) work.push(find(x.getAttribute('href'), e.path).then((k) => { if (k) x.setAttribute('href', abs(k)); }));
        } else if (x.localName === 'style') {
          work.push(css(x.textContent, e.path).then((t) => { x.textContent = t; }));
        } else if (x.localName === 'script') {
          work.push(find(x.getAttribute('src'), e.path).then(async (k) => { if (!k) return; const t = await text(k); x.removeAttribute('src'); x.textContent = t.replace(/<\/script/gi, '<\\/script'); }));
        } else {
          for (const a of ['src', 'poster']) if (x.hasAttribute(a) && x.localName !== 'script') work.push(find(x.getAttribute(a), e.path).then((k) => { if (k) x.setAttribute(a, abs(k)); }));
          if (x.hasAttribute('style')) work.push(css(x.getAttribute('style'), e.path).then((t) => x.setAttribute('style', t)));
          x.removeAttribute('srcset');
        }
      }
      await Promise.all(work);
      // Shown with no origin of its own, a page has no storage; many games
      // save to it, so give them some that lasts while the page is open.
      const shim = d.createElement('script');
      shim.textContent = `(function(){function S(){var m=new Map();return{getItem:function(k){return m.has(String(k))?m.get(String(k)):null},setItem:function(k,v){m.set(String(k),String(v))},removeItem:function(k){m.delete(String(k))},clear:function(){m.clear()},key:function(i){return Array.from(m.keys())[i]||null},get length(){return m.size}}}['localStorage','sessionStorage'].forEach(function(n){try{window[n].length}catch(e){try{Object.defineProperty(window,n,{value:S(),configurable:true})}catch(e2){}}})})();`;
      d.head.prepend(shim);
      // Files the page asks for as it runs (fetch, XMLHttpRequest, pictures
      // and sounds made in script) come from the same map of its folder.
      const files = {}, dirs = [e.dir];
      let n = 0;
      while (dirs.length && n < 4000) {
        const di = await dirInfo(dirs.shift());
        for (const k of di ? await listDir(di) : []) {
          if (k.kind === 'folder') { dirs.push(k.path); continue; }
          const u = abs(k);
          if (u) { files[k.path] = u; n++; }
        }
      }
      const rt = d.createElement('script');
      rt.textContent = `(function(){var F=${JSON.stringify(files).replace(/</g, '\\u003c')},B=${JSON.stringify('http://z/' + segs(e.dir) + (e.dir ? '/' : ''))};
function R(u){try{var x=new URL(String(u),B);if(x.host!=='z')return u;var p=decodeURIComponent(x.pathname.slice(1));return F[p]||u}catch(e){return u}}
var f=window.fetch;if(f)window.fetch=function(i,o){return f.call(this,typeof i==='string'||i instanceof URL?R(i):i,o)};
var X=XMLHttpRequest.prototype.open;XMLHttpRequest.prototype.open=function(m,u){arguments[1]=R(u);return X.apply(this,arguments)};
[[HTMLImageElement,'src'],[HTMLMediaElement,'src'],[HTMLScriptElement,'src'],[HTMLSourceElement,'src']].forEach(function(a){var d=Object.getOwnPropertyDescriptor(a[0].prototype,a[1]);if(d&&d.set)Object.defineProperty(a[0].prototype,a[1],{get:d.get,set:function(v){d.set.call(this,R(v))},configurable:true})});
var SA=Element.prototype.setAttribute;Element.prototype.setAttribute=function(k,v){return SA.call(this,k,/^(src|href|poster)$/i.test(k)&&!(this instanceof HTMLAnchorElement)?R(v):v)};})();`;
      shim.after(rt);
      return '<!doctype html>' + d.documentElement.outerHTML;
    }
    async function fill(pane, e, url) {
      const st = pane.querySelector('.stage');
      if (!url || !st || PARTS[e.cid]) return;
      const want = e.path, x = ext(e.name);
      const still = () => st.isConnected && (!current() || current().path === want);
      const range = (n) => e.blob ? e.blob.slice(0, n).arrayBuffer() : head(url, n);
      const viewer = V && V.find(e, x);
      try {
        if (viewer) {
          const get = async (r, h, cache) => {
            const res = await fetch(url, Object.assign(h ? { headers: h } : {}, cache ? { cache } : {}));
            if (!res.ok) throw new Error(res.status === 404 ? (await res.text()) || 'This file is not in the pieces on this machine.' : res.statusText);
            return r(res);
          };
          const v = {
            e, x, url, still, base: await baseOf(e), esc, icon, bytes, num,
            head: (n) => range(n),
            all: async (max) => {
              if (e.bytes > max) throw new Error(`This file is ${bytes(e.bytes)}, too big to open here. Download it to see it.`);
              if (e.blob) return e.blob.arrayBuffer();
              // A short answer is a part the browser kept from a Range
              // request; ask the server for the whole file instead.
              const buf = await get((r) => r.arrayBuffer());
              return buf.byteLength >= e.bytes ? buf : get((r) => r.arrayBuffer(), undefined, 'reload');
            },
            range: (a, b) => e.blob ? e.blob.slice(a, b).arrayBuffer() : get((r) => r.arrayBuffer(), { Range: `bytes=${a}-${b - 1}` }).then((buf) => buf.byteLength > b - a ? buf.slice(a, b) : buf),
            tail: (n) => e.blob ? e.blob.slice(Math.max(0, e.blob.size - n)).arrayBuffer().then((buf) => [buf, e.blob.size]) : get(async (r) => { const buf = await r.arrayBuffer(); return r.status === 206 ? [buf, e.bytes] : [buf.slice(Math.max(0, buf.byteLength - n)), buf.byteLength]; }, { Range: `bytes=-${n}` })
          };
          await viewer.show(st, v);
          return;
        }
        if (e.kind === 'page') {
          const f = st.querySelector('iframe'), base = await baseOf(e, true);
          if (!still() || !f) return;
          // A page from another origin keeps it (its storage, its fonts); one
          // from this origin must not, or it could reach the explorer.
          const show = (src) => { if (new URL(src, location.href).origin !== location.origin) f.sandbox.add('allow-same-origin'); f.src = src; };
          if (LOCAL && base) {
            // (a static copy of the explorer, like the website's demo, has no _i2f/path)
            const src = base + encodeURIComponent(e.name), ok = await fetch((await baseOf(e)) + encodeURIComponent(e.name), { method: 'HEAD' }).then((r) => r.ok, () => false);
            if (!still()) return;
            if (ok) { show(src); return; }
          }
          // A static host may not call it HTML; then show it from its text,
          // its links resolving against its folder.
          const r = await fetch(url, { method: 'HEAD' }).catch(() => null);
          if (r && r.ok && /html/i.test(r.headers.get('content-type') || '')) { show(url); return; }
          const text = new TextDecoder().decode(await range(8 << 20));
          const html = HERE && base ? `<base href="${esc(new URL(base, location.href).href)}">` + text : await pageDoc(e, text);
          if (still()) f.srcdoc = html;
          return;
        }
        if (e.kind === 'text' || e.kind === 'code') {
          const buf = await range(256 << 10);
          if (current() && current().path !== want) return;
          const lines = new TextDecoder().decode(buf).split('\n');
          const cut = buf.byteLength >= 256 << 10 || e.bytes > buf.byteLength;
          st.innerHTML = `<pre class="code"><code>${lines.slice(0, 5000).map((l) => `<span>${esc(l)}</span>`).join('\n')}</code></pre>${cut || lines.length > 5000 ? '<p class="faint pad">Showing the start. Download for all of it.</p>' : ''}`;
          const lang = V && V.runs(x);
          if (lang) V.runner(st, { all: async () => { if (e.blob) return e.blob.arrayBuffer(); const r = await fetch(url); if (!r.ok) throw new Error(r.statusText); return r.arrayBuffer(); } }, lang);
        } else if (st.querySelector('.hex')) {
          const b = new Uint8Array(await range(512));
          if (current() && current().path !== want) return;
          let out = '';
          for (let i = 0; i < b.length; i += 16) {
            const row = Array.from(b.slice(i, i + 16));
            out += i.toString(16).padStart(6, '0') + '  ' + row.map((x) => x.toString(16).padStart(2, '0')).join(' ').padEnd(48) + '  ' + row.map((x) => x >= 32 && x < 127 ? String.fromCharCode(x) : '.').join('') + '\n';
          }
          st.innerHTML = `<pre class="hex">${esc(out)}</pre><p class="faint pad">The first ${num(b.length)} bytes. No preview for this kind of file.</p>`;
        }
      } catch (err) { if (st.isConnected) st.innerHTML = `<div class="none"><p>${esc(err.message)}</p></div>`; }
      const media = st.querySelector('img:not(.cover), video, audio');
      if (media) media.onerror = async () => {
        // Say why: the server's word when it has the file only in part, or
        // the browser's when it cannot play the format.
        let why = LOCAL ? 'This file is not in the pieces on this machine, or the browser cannot show it.' : 'The browser could not load or show this file.';
        try {
          const r = await fetch(url, { headers: { Range: 'bytes=0-0' } });
          // explore answers in plain text; a static copy (the website demo,
          // GitHub Pages) answers a left-out file with its own HTML 404 page.
          if (!r.ok) why = /^text\/plain/.test(r.headers.get('content-type') || '') ? ((await r.text()) || why) : missing(e, r);
          else if (AVTYPE[x] && !media.dataset.typed && /octet-stream/.test(r.headers.get('content-type') || '')) {
            // Static hosts send these untyped, which some browsers (Safari)
            // will not play; try once more from a blob with the right type.
            media.dataset.typed = '1';
            const b = await (await fetch(url)).blob();
            if (st.isConnected) media.src = URL.createObjectURL(new Blob([b], { type: AVTYPE[x] }));
            return;
          } else if (e.kind === 'video' || e.kind === 'audio') why = `This browser cannot play ${x ? '.' + x + ' files' : 'this file'}${media.error && media.error.message ? ' (' + media.error.message + ')' : ''}. Download it to play it.`;
        } catch (err) { /* keep the general reason */ }
        if (st.isConnected) st.innerHTML = `<div class="none">${icon(e.kind)}<p>${esc(why)}</p></div>`;
      };
    }
    async function folderPane(pane, di, p, here, e) {
      if (!di) { pane.innerHTML = ''; return; }
      const r = rootAt(p);
      const name = p ? (r && r.kind !== 'mfs' ? rootLabel(r) : p.split('/').pop()) : 'data';
      const typeRows = KINDS.filter((k) => k !== 'folder' && di.types[k]).map((k) => [k, di.types[k]]).sort((a, b) => b[1][1] - a[1][1]);
      pane.innerHTML = `<div class="ph"><strong title="${esc(name)}">${esc(name.length > 40 ? shortCID(name) : name)}/</strong>${here ? '' : `<a class="btn sm" href="${esc(browseHref(p))}">Open</a>`}</div>
<div class="stage folder"><div class="mosaic"></div></div>
<div class="facts2"><div><span>Items</span><b>${num(di.n)}</b></div><div><span>Size</span><b>${bytes(di.bytes)}</b></div><div><span>Files</span><b>${num(di.files)}</b></div><div><span>Folders</span><b>${num(di.folders)}</b></div></div>
${typeRows.length ? `<div class="pad2">${bar(typeRows.map((t) => [KIND[t[0]][3], t[1][1], KIND[t[0]][1]]), typeRows.map((t) => KIND[t[0]][3] + ' ' + bytes(t[1][1])).join(', '))}<div class="legend sm">${typeRows.map((t) => `<a href="browse.html?${qs({ p, kind: t[0], deep: '1' })}"><span class="sw" style="background:${KIND[t[0]][1]}"></span>${KIND[t[0]][3]} ${num(t[1][0])}</a>`).join('')}</div></div>` : ''}
<div class="facts dl">
  ${di.cid ? `<span>CID</span><span>${cidx(di.cid)} <button type="button" class="link" data-copy="${esc(di.cid)}">Copy</button></span>` : ''}
  ${di.cid ? `<span>Open in</span><span class="gw">${GATEWAYS.map((g) => `<a href="${esc(g[1] + di.cid)}" target="_blank" rel="noopener">${g[0]} ↗</a>`).join('')}<a href="${esc(gw(di.cid) + '?format=car')}" target="_blank" rel="noopener">CAR</a></span>` : ''}
  ${r ? `<span>Root</span><span>${esc(ROOTKIND[r.kind] || r.kind)}${r.kind !== 'mfs' ? ` · <a href="pin.html?cid=${encodeURIComponent(r.cid)}">details</a>` : ''}</span>` : ''}
  ${pieceAt(di.lo) >= 0 ? `<span>Pieces</span><a href="pieces.html?n=${pieceAt(di.lo)}">${di.lo === di.hi ? '#' + pieceAt(di.lo) : '#' + pieceAt(di.lo) + ' to #' + pieceAt(di.hi)}</a>` : ''}
</div>`;
      if (canShow() && di.types.image) {
        const ks = here ? kids : await listDir(di);
        const imgs = ks.filter((x) => x.kind === 'image').slice(0, 6);
        const mo = pane.querySelector('.mosaic');
        if (mo && imgs.length) mo.innerHTML = imgs.map((x) => `<img loading="lazy" alt="" src="${esc(thumbURL(x))}">`).join('');
      }
      if (!pane.querySelector('.mosaic img')) { const s = pane.querySelector('.stage'); if (s) s.remove(); }
    }

    // ---- events
    const sel = (i) => {
      const n = (S.view === 'tree' ? rows : shown).length;
      if (!n) return;
      const lim = S.limit;
      S.cur = Math.max(0, Math.min(n - 1, i));
      if (S.cur >= S.limit - 5 && n > S.limit) S.limit += 300;
      S.sel = (S.view === 'tree' ? rows : shown)[S.cur].path;
      // Moving the cursor only moves the highlight: drawing the list again
      // would reload every thumbnail in it, and it flickers.
      const was = S.limit === lim && S.view !== 'cols' && $(`.flist [data-i="${S.cur}"]`);
      if (was) {
        main().querySelectorAll('.flist [aria-selected="true"]').forEach((x) => x.setAttribute('aria-selected', 'false'));
        was.setAttribute('aria-selected', 'true');
        preview();
        settle();
      } else update();
      const el = $(`.flist [data-i="${S.cur}"]${S.view === 'cols' ? `[data-c="${COLS.length}"]` : ''}`);
      if (el) el.scrollIntoView({ block: 'nearest' });
    };
    function open(e) {
      if (!e) return;
      if (isDir(e)) { S.deep = false; S.q = ''; box.value = ''; S.kind = 'all'; enter(e.path).catch(fail); return; }
      if (e.kind === 'account') { location.href = accHref(e.acct); return; }
      if (S.ly !== 'file') { S.ly = 'file'; keep('layout', S.ly); update(true); }
    }
    function up() {
      if (!S.p) return false;
      const d = S.p.includes('/') ? S.p.slice(0, S.p.lastIndexOf('/')) : '';
      const from = S.p;
      S.deep = false; S.q = ''; box.value = '';
      enter(d).then(() => { const i = shown.findIndex((e) => e.path === from); if (i >= 0) sel(i); }).catch(fail);
      return true;
    }
    main().addEventListener('click', (ev) => {
      const t = ev.target;
      const b = t.closest('button, a, [data-i]');
      if (!b) return;
      if (b.dataset.view) { S.view = b.dataset.view; keep('view', S.view); S.cur = -1; update(true); return; }
      if (b.dataset.ly) { S.ly = b.dataset.ly; keep('layout', S.ly); update(); return; }
      if (b.dataset.k) { S.kind = b.dataset.k; S.cur = -1; S.limit = 300; update(); return; }
      if (b.hasAttribute('data-unscope')) { S.p = ''; enter('').catch(fail); return; }
      if (b.hasAttribute('data-more')) { S.limit += 300; update(); return; }
      if (b.hasAttribute('data-public')) { PUBLIC = true; update(true); return; }
      if (b.dataset.copy || b.classList.contains('cidx')) return;
      if (b.dataset.step) { sel((S.cur < 0 ? -1 : S.cur) + +b.dataset.step); return; }
      if (b.hasAttribute('data-full')) { full(); return; }
      if (b.dataset.tw != null) { const e = rows[+b.dataset.tw]; if (e) toggle(e); return; }
      if (b.dataset.open != null) { ev.preventDefault(); const e = shown[+b.dataset.open]; if (isDir(e)) open(e); else { pushSel = true; sel(+b.dataset.open); } return; }
      if (b.hasAttribute('data-enter')) { ev.preventDefault(); open(current()); return; }
      if (b.dataset.c != null && b.dataset.i != null) {
        const ci = +b.dataset.c, e = ci < COLS.length ? COLS[ci].items[+b.dataset.i] : shown[+b.dataset.i];
        if (!e) return;
        if (isDir(e)) { enter(e.path).catch(fail); return; }
        if (ci < COLS.length) { S.sel = e.path; enter(COLS[ci].dir, true).catch(fail); return; }
        pushSel = true; sel(+b.dataset.i); return;
      }
      if (b.dataset.i != null && b.tagName !== 'A') { pushSel = true; sel(+b.dataset.i); }
    });
    // Links to this page (the sidebar, path, folder links) and the back and
    // forward buttons change the folder in place, without a reload.
    function route(push, to) {
      if (push) { if (to !== location.href) history.pushState(null, '', to); else history.replaceState(null, '', to); }
      fromURL();
      box.value = S.q;
      lastURL = urlOf(); lastKey = navKey();
      enter(S.p, !!S.sel).catch(fail);
    }
    D.addEventListener('click', (ev) => {
      if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
      const a = ev.target.closest && ev.target.closest('a[href]');
      if (!a || (a.target && a.target !== '_self') || a.hasAttribute('download')) return;
      const to = new URL(a.href, location.href);
      // A viewer (?app=) replaces the file list, so it loads as a page.
      if (to.origin !== location.origin || to.pathname !== location.pathname || to.searchParams.get('app') || (to.hash && to.search === location.search)) return;
      ev.preventDefault();
      route(true, to.href);
    });
    addEventListener('popstate', () => route(false));
    main().addEventListener('dblclick', (ev) => {
      const r = ev.target.closest('[data-i]');
      if (r && !r.dataset.c) open(S.view === 'tree' ? rows[+r.dataset.i] : shown[+r.dataset.i]);
    });
    let typing;
    box.oninput = () => {
      clearTimeout(typing);
      typing = setTimeout(() => {
        S.q = box.value; S.cur = -1; S.limit = 300; S.sel = '';
        if (S.q.trim()) { S.deep = true; needAll(); }
        update();
      }, all ? 60 : 150);
    };
    $('#fdeep').onchange = (ev) => { S.deep = ev.target.checked; S.cur = -1; if (S.deep) needAll(); update(); };
    $('#fsize').onchange = (ev) => { S.size = ev.target.value; S.cur = -1; update(); };
    $('#fsort').onchange = (ev) => { S.sort = ev.target.value; S.dir = S.sort === 'size' ? -1 : 1; update(); };
    $('#fflip').onclick = () => { S.dir = -S.dir; update(); };
    $('.flist').addEventListener('scroll', (ev) => {
      const el = ev.target;
      if (el.scrollTop + el.clientHeight > el.scrollHeight - 400 && shown.length > S.limit) { S.limit += 300; update(); }
    });
    pageKeys.fromBox = (k) => {
      if (k === 'Enter') {
        const q = box.value.trim(), r = (m.roots || []).find((x) => x.cid === q);
        if (r) { location.href = 'pin.html?cid=' + encodeURIComponent(r.cid); return true; }
      }
      box.blur(); $('.flist').focus(); if (S.cur < 0) sel(0);
      return true;
    };
    const step = (d) => () => { sel((S.cur < 0 ? -1 : S.cur) + d); return true; };
    // Tiles per row in the grid, as laid out (a CSS auto-fill grid).
    const across = () => {
      if (S.view !== 'grid') return 1;
      const t = $('.flist .tiles');
      return t ? Math.max(1, getComputedStyle(t).gridTemplateColumns.split(' ').length) : 1;
    };
    // Up and down in the grid go to the tile above or below on screen: rows
    // are cut short where a folder's group ends, so it is not a fixed count.
    const vert = (d) => () => {
      if (S.view !== 'grid' || S.cur < 0) return step(d)();
      const at = (i) => { const el = $(`.flist .tile[data-i="${i}"]`); return el && el.getBoundingClientRect(); };
      const me = at(S.cur);
      if (!me) return step(d * across())();
      const x = me.left + me.width / 2;
      let best = -1, row = null, bestDx = Infinity;
      for (let i = S.cur + d; i >= 0 && i < Math.min(shown.length, S.limit); i += d) {
        const r = at(i);
        if (!r) break;
        if (row === null) { if (Math.abs(r.top - me.top) < 2) continue; row = r.top; }
        if (Math.abs(r.top - row) >= 2) break;
        const dx = Math.abs(r.left + r.width / 2 - x);
        if (dx < bestDx) { bestDx = dx; best = i; }
      }
      if (best >= 0) sel(best);
      return true;
    };
    // After a click in the sidebar, the up and down arrows go through its
    // items, opening each; the right arrow, or a click elsewhere, goes back
    // to the files. (Kept for the tab, as Bluesky App loads a page.)
    function navStep(d) {
      const links = Array.from($('.lib').querySelectorAll('a'));
      let i = links.indexOf(D.activeElement);
      if (i < 0) i = links.findIndex((a) => a.getAttribute('aria-current') === 'page');
      const to = links[i + d];
      if (!to) return true;
      to.focus({ preventScroll: true });
      to.click();
      return true;
    }
    Object.assign(pageKeys, {
      j: step(1), k: step(-1), ArrowDown: () => NAV ? navStep(1) : vert(1)(), ArrowUp: () => NAV ? navStep(-1) : vert(-1)(),
      ArrowRight: () => { if (NAV) { navOn(false); $('.flist').focus({ preventScroll: true }); return true; } if (S.view === 'grid') return step(1)(); const e = current(); if (S.view === 'tree' && e && e.branch && !e.open) { toggle(e); return true; } return false; },
      ArrowLeft: () => { if (S.view === 'grid') return step(-1)(); const e = current(); if (S.view === 'tree' && e && e.open) { toggle(e); return true; } return false; },
      Enter: () => { open(current()); return true; }, ' ': () => { S.ly = S.ly === 'file' ? 'split' : 'file'; keep('layout', S.ly); update(); return true; },
      u: up, Backspace: up, f: () => { box.focus(); box.select(); return true; }, '/': () => { box.focus(); box.select(); return true; },
      v: () => { S.view = VIEWS[(VIEWS.findIndex((v) => v[0] === S.view) + 1) % VIEWS.length][0]; keep('view', S.view); update(true); toast(VIEWS.find((v) => v[0] === S.view)[1] + ' view'); return true; },
      l: () => { S.ly = LAYOUTS[(LAYOUTS.findIndex((v) => v[0] === S.ly) + 1) % LAYOUTS.length][0]; keep('layout', S.ly); update(); return true; },
      d: () => { const e = current(), u = e && e.kind !== 'folder' && e.kind !== 'account' && contentURL(e, { dl: 1 }); if (u) location.href = u; return !!u; },
      s: () => { const ks = Object.keys(SORTS); S.sort = ks[(ks.indexOf(S.sort) + 1) % ks.length]; update(); toast('Sorted by ' + $('#fsort').selectedOptions[0].textContent); return true; },
      r: () => { S.dir = -S.dir; update(); return true; },
      F: () => { const e = current(); return isFull() || (e && e.kind !== 'folder' && contentURL(e)) ? full() : false; },
      esc: () => isFull() && !fsEl() ? full(false) : false
    });
    for (let i = 0; i <= 9; i++) pageKeys[String(i)] = () => { const c = main().querySelectorAll('.chips [data-k]')[i]; if (c) c.click(); return !!c; };
    await enter(S.p, !!S.sel);
  }

  // ---- A pin (or any root)
  async function pin() {
    const c = params.get('cid') || '';
    const [m, meta] = await Promise.all([manifest(), load('meta')]);
    const r = (m.roots || []).find((x) => x.cid === c);
    if (!r) throw new Error(`No root with CID ${c} in this archive.`);
    const info = await dirInfo(r.path);
    // A root whose blocks all sit in pieces another repo stored has no
    // pieces of its own (firstPiece is -1).
    const own = r.firstPiece >= 0, n = own ? r.lastPiece - r.firstPiece + 1 : 0, copies = m.copies || [];
    pageKeys.cid = () => r.cid;
    pageKeys.u = pageKeys.Backspace = () => { location.href = browseHref(r.path.includes('/') ? r.path.slice(0, r.path.lastIndexOf('/')) : ''); return true; };
    const ukind = info ? 'UnixFS directory' : r.cid.startsWith('bafyr') ? 'dag-cbor, not UnixFS' : 'File or single block';
    main().innerHTML = `
${crumbs(r.path)}
<section class="head">
  <div class="eyebrow"><span>${esc(ROOTKIND[r.kind] || r.kind)}</span><span>${own ? `In ${plural(n, 'piece')}${copies.length ? `, ${plural(copies.length, 'copy', 'copies')} each` : ''}` : 'Stored with another repo'}</span></div>
  <h1>${esc(r.name || shortCID(r.cid))}</h1>
  <div class="meta"><code class="chipcode">${esc(r.cid)}</code><button type="button" class="link" data-copy="${esc(r.cid)}">Copy CID</button></div>
  <p class="note">${r.name ? 'Older repos keep no pin names, so where there is none the explorer may guess one, such as an index page title. The CID is the real identity.' : 'This root has no name, so it is shown by its CID.'}</p>
</section>
<section class="stats" aria-label="Facts">
  <div class="card tight stat"><div class="k">Size</div><div class="v">${bytes(r.bytes)}</div></div>
  <div class="card tight stat"><div class="k">Blocks</div><div class="v">${count(r.blocks)}</div></div>
  <div class="card tight stat"><div class="k">Kind</div><div class="v" style="font-size:20px">${esc(ukind)}</div></div>
  <div class="card tight stat"><div class="k">Pieces</div><div class="v">${!own ? '—' : n === 1 ? r.firstPiece : r.firstPiece + ' to ' + r.lastPiece}</div></div>
</section>
<div class="split">
  <section class="card wide"><h2>Where it's stored</h2>${own ? `<div id="list"></div><a href="pieces.html?n=${r.firstPiece}">${n === 1 ? 'This piece' : 'These ' + num(n) + ' pieces'} among all pieces</a>` : `<p class="faint">Every block of it is also in a repo packed before this one, so it is stored in that repo's pieces and adds none here.</p>`}</section>
  <aside class="side">
    <section class="card"><h2>Open it</h2><div class="stack">
      ${info ? `<a href="${esc(browseHref(r.path))}">Browse its files</a>` : ''}
      ${HERE ? `<a href="../data/${esc(segs(r.path))}">Inside this archive</a>` : ''}
      ${GATEWAYS.concat(HERE ? [['this gateway', HERE]] : []).map((g) => `<a href="${esc(g[1] + r.cid)}" target="_blank" rel="noopener">Through ${g[0]} ↗</a>`).join('')}
      <a href="${esc(gw(r.cid) + '?format=car')}" target="_blank" rel="noopener">Download as CAR</a>
      <div><span class="faint">With kubo</span><br><code style="font-size:13px;overflow-wrap:anywhere">ipfs get ${esc(r.cid)}</code></div>
    </div></section>
    ${info ? '<section class="card"><h2>Top level</h2><div class="stack" id="top"></div></section>' : ''}
  </aside>
</div>`;
    const rows = [];
    for (let i = r.firstPiece; own && i <= r.lastPiece && i < (m.pieces || []).length; i++) rows.push(m.pieces[i]);
    if (own) new List(D.getElementById('list'), {
      label: 'Pieces of this root', step: 100, sort0: 'n', filter: null, sortSelect: false,
      cols: [{ key: 'n', label: 'Piece', sort: (p) => p.index, html: (p) => `<a href="pieces.html?n=${p.index}">#${p.index}</a>` },
        { key: 'cid', label: 'PieceCID', cls: 'cid', html: (p) => cidx(p.pieceCid, true) },
        { key: 'size', label: 'Size', cls: 'r', sort: (p) => p.carBytes, html: (p) => bytes(p.carBytes) },
        { key: 'copies', label: 'Copies', cls: 'hide-sm', html: (p) => `<span class="tags">${copies.filter((c) => c.pieceIds && c.pieceIds[p.index] != null).map((c) => `<span class="tag">${esc(c.provider)}</span>`).join('') || '<span class="faint">not stored yet</span>'}</span>` }],
      href: (p) => 'pieces.html?n=' + p.index, cid: (p) => p.pieceCid, gateway: false, statusOf: (p) => '#' + p.index + ' · ' + (meta.pieces[p.index] || {}).label
    }).setItems(rows);
    if (info) {
      const kids = await listDir(info);
      D.getElementById('top').innerHTML = kids.slice(0, 12).map((e) => `<a href="${esc(entryHref(e))}">${esc(e.name)}${e.kind === 'folder' ? '/' : ''} <span class="faint">· ${bytes(e.bytes)}</span></a>`).join('') + (kids.length > 12 ? `<a href="${esc(browseHref(r.path))}">${num(kids.length - 12)} more</a>` : '');
    }
    status(r.name || r.cid);
  }

  // ---- Pieces
  async function piecesPage() {
    const [m, meta] = await Promise.all([manifest(), load('meta')]);
    const pieces = m.pieces || [], copies = m.copies || [], snaps = m.snapshots || [];
    const providers = copies.map((c) => {
      const ids = Object.keys(c.pieceIds || {});
      return `<div class="card tight"><div style="font-weight:600">Provider ${esc(c.provider)}</div><div class="faint">Data set ${esc(c.dataSetId)} · ${plural(ids.length, 'piece')} · ${bytes(ids.reduce((a, i) => a + ((pieces[i] || {}).carBytes || 0), 0))}</div></div>`;
    }).join('');
    const full = Math.max(0, ...pieces.map((p) => p.carBytes)) * 0.99;
    const lastSnap = snaps.length - 1;
    main().innerHTML = `
<section class="head"><h1>${plural(pieces.length, 'piece')}${copies.length ? `, ${plural(copies.length, 'copy', 'copies')} each, on ${plural(new Set(copies.map((c) => c.provider)).size, 'provider')}` : ', not stored yet'}</h1>
<p class="lead">A piece is one CAR file that a provider stores and proves on chain. Pieces are cut at content-defined points, so the same repo always makes the same pieces, and a new snapshot reuses every piece that didn't change.</p></section>
${providers ? `<section class="stats" aria-label="Providers">${providers}</section>` : ''}
<section class="card"><div class="titled"><h2>Every piece</h2><a href="../index/manifest.json">manifest.json</a></div><div id="list"></div></section>`;
    const items = pieces.map((p) => ({ ...p, info: meta.pieces[p.index] || { where: 'index', kind: 'other', label: '', path: '' } }));
    const list = new List(D.getElementById('list'), {
      label: 'Pieces', step: 200, sort0: 'n', filter: 'Filter by PieceCID or contents',
      kinds: kindChips('folder'), kindOf: (p) => p.info.kind, text: (p) => p.pieceCid + ' ' + p.info.label,
      selects: [{ key: 'holds', label: 'Holds', options: [['any', 'Anything'], ['pins', 'Pins'], ['mfs', 'MFS'], ['orphans', 'Orphans'], ['index', 'Index and explorer']], test: (p, v) => p.info.where === v },
        { key: 'size', label: 'Size', options: [['any', 'Any'], ['full', 'Full'], ['part', 'Partly full']], test: (p, v) => (p.carBytes >= full) === (v === 'full') }],
      cols: [{ key: 'n', label: '#', sort: (p) => p.index, html: (p) => String(p.index) },
        { key: 'cid', label: 'PieceCID', cls: 'cid', html: (p) => cidx(p.pieceCid, true) },
        { key: 'size', label: 'Size', cls: 'r', sort: (p) => p.carBytes, html: (p) => bytes(p.carBytes) },
        { key: 'blocks', label: 'Blocks', cls: 'r hide-sm', sort: (p) => p.blocks, html: (p) => num(p.blocks) },
        { key: 'holds', label: 'Holds', sort: (p) => p.info.label.toLowerCase(), html: (p) => `<span class="nm">${icon(p.info.kind)}<a href="${esc(holdsHref(p))}">${esc(p.info.label)}</a></span>` },
        { key: 'snap', label: 'Snapshot', cls: 'hide-sm', sort: (p) => p.snapshot, html: (p) => snaps[p.snapshot] ? `<span class="${p.snapshot === lastSnap && lastSnap > 0 ? 'new' : 'muted'}">${day(snaps[p.snapshot].time)}</span>` : '' }],
      href: holdsHref, cid: (p) => p.pieceCid, gateway: false, statusOf: (p) => '#' + p.index + ' · ' + p.pieceCid
    });
    function holdsHref(p) { return p.info.cid ? 'pin.html?cid=' + encodeURIComponent(p.info.cid) : p.info.where === 'index' ? 'index.html' : browseHref(p.info.path); }
    list.setItems(items);
    const n = params.get('n');
    if (n != null && items[+n]) {
      list.s.limit = Math.max(list.s.limit, +n + 50);
      list.s.cur = +n;
      list.render();
      list.mark();
    }
    status(num(pieces.length) + ' pieces');
  }

  // ---- Posts: a Bluesky (atproto) account in the archive, shown the way
  // Bluesky shows it. Its repo is IPLD like any other; pack summarizes its
  // records into idx/at/<n>.js, and the images and video are blobs in the
  // archive, fetched by CID. Replies the account made to itself are threads;
  // posts by other accounts live in their repos, not here, and link out.
  const COLLS = { 'app.bsky.feed.post': 'posts', 'app.bsky.feed.like': 'likes', 'app.bsky.feed.repost': 'reposts', 'app.bsky.graph.follow': 'follows', 'app.bsky.graph.block': 'blocks', 'app.bsky.graph.list': 'lists', 'app.bsky.graph.listitem': 'list entries', 'app.bsky.feed.generator': 'feeds', 'app.bsky.graph.starterpack': 'starter packs', 'app.bsky.feed.threadgate': 'thread rules', 'app.bsky.feed.postgate': 'quote rules', 'app.bsky.actor.profile': 'profile', 'app.bsky.graph.verification': 'verifications' };
  const collName = (c) => COLLS[c] || c;
  const when = (t) => { const d = new Date(t); return isNaN(d) ? '' : d.toLocaleString('en-GB', { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit', timeZone: 'UTC' }) + ' UTC'; };
  // What an archive holds of an account, in Bluesky's terms. x is the
  // account's entry in meta.accounts, a its summary when loaded.
  function archivedHTML(x, a, files) {
    const b = x.blobs || {}, counts = (a && a.counts) || {};
    const colls = Object.keys(counts).sort((p, q) => counts[q] - counts[p]);
    const handle = x.handle || (a && a.handle) || x.name.replace(/^@/, '');
    const pds = (x.pds || (a && a.pds) || '').replace(/^https?:\/\//, '');
    const doc = x.didDoc || (a && a.didDoc), plc = x.plcLog || (a && a.plcLog);
    return `<dl class="at-held">
  <dt>Account</dt><dd><b>@${esc(handle)}</b> <code class="chipcode">${esc(x.did)}</code>${pds ? ` <span class="faint">on ${esc(pds)}</span>` : ''}</dd>
  <dt>As of</dt><dd>${x.time ? `the repo as it was at ${esc(when(x.time))}` : 'the repo when it was fetched'}${a ? `, revision <code>${esc(a.rev)}</code>, signed commit ${cidx(a.commit, true)}` : ''}</dd>
  <dt>Records</dt><dd>All ${num(x.records || 0)} public records${colls.length ? ': ' + colls.map((c) => `${num(counts[c])} ${esc(collName(c))}`).join(', ') : ''}${a && a.missing ? ` <span class="warn">(${plural(a.missing, 'record')} missing)</span>` : ''}${files ? ` · <a href="${esc(files)}">browse them as files</a>` : ''}</dd>
  <dt>Media</dt><dd>${b.linked ? `${b.held === b.linked ? 'All' : num(b.held) + ' of the'} ${plural(b.linked, 'blob')} the records link: ${plural(b.images || 0, 'image')} and ${plural(b.videos || 0, 'video')}, ${bytes(b.bytes || 0)}${b.held < b.linked ? ` <span class="warn">(${num(b.linked - b.held)} the server no longer had)</span>` : ''}` : 'No images or video'}</dd>
  <dt>Identity</dt><dd>${doc || plc ? `The DID document${plc ? ' and the PLC directory\'s signed log of every handle and key change' : ''}, so the commit's signature can be checked from the archive alone` : 'The DID, from the commit; no DID document was pinned with it'}</dd>
  <dt>Not in it</dt><dd class="faint">What the account's server keeps outside the repo (preferences, mutes, direct messages), and other accounts' replies, likes and reposts, which live in their own repos.</dd>
</dl>`;
  }

  // The Bluesky App viewer for account n, in the Content page's main pane;
  // a link to a post opens its thread there.
  const appHref = (n, k, extra) => 'browse.html?' + qs({ app: 'bsky', a: n, t: k || '', public: PUBLIC ? '1' : '', ...(extra || {}) });
  // posts.html, the old place of the Bluesky view, sends its links on.
  function posts() {
    location.replace(appHref(+params.get('a') || 0, params.get('t') || '', { q: params.get('q') || '', show: params.get('show') || '', public: params.get('public') || '' }) + location.hash);
  }
  // kit(n) draws account n's posts as Bluesky does. Loaded once each.
  const KITS = {};
  function bsky(n) {
    return KITS[n] || (KITS[n] = (async () => {
      const meta = await load('meta');
      return kit(n, meta.accounts[n], await load('at/' + n));
    })());
  }
  function kit(n, x0, a) {
    const blob = (c, name, extra) => !c ? null : LOCAL ? CONTENT + encodeURIComponent(c) + '?' + qs({ name: name || c, ...(extra || {}) }) : HERE || PUBLIC ? gw(c) : null;
    const handle = x0.handle || a.handle || (a.name.startsWith('@') ? a.name.slice(1) : '');
    const at = (uri) => (uri || '').match(/^at:\/\/([^/]+)\/([^/]+)\/([^/]+)$/);
    const web = (uri) => { const m = at(uri); return m && m[2] === 'app.bsky.feed.post' ? `https://bsky.app/profile/${m[1]}/post/${m[3]}` : ''; };
    const own = (uri) => { const m = at(uri); return m && (m[1] === a.did || (handle && m[1] === handle)) && m[2] === 'app.bsky.feed.post' ? m[3] : ''; };
    const p = a.profile || {};
    const all = a.posts || [];
    const byKey = new Map(all.map((x) => [x.k, x]));
    const kids = new Map(); // rkey -> replies to it in the archive, oldest first
    all.forEach((x) => { const k = own(x.r); if (k) { if (!kids.has(k)) kids.set(k, []); kids.get(k).unshift(x); } });
    const pathOf = (k) => x0.path ? x0.path + '/app.bsky.feed.post/' + k + '.json' : '';
    // A post's record: selected in Content, which shows it as JSON (a raw
    // link downloads it from a static copy). Else as JSON from explore or a
    // gateway.
    const recHref = (x) => pathOf(x.k) ? browseHref(pathOf(x.k)) : !x.c ? '' : LOCAL ? blob(x.c, x.k + '.json') : HERE || PUBLIC ? gw(x.c) + '?format=dag-json' : '';
    const self = (k) => appHref(n, k);
    const name = esc(p.displayName || handle || a.name);

    // Text with its links, mentions and tags, from the record's facets
    // (offsets in UTF-16 units); bare URLs are linked too.
    const bare = (t) => esc(t).replace(/(https?:\/\/[^\s<]+[^\s<.,;:!?)\]'"])/g, '<a href="$1" target="_blank" rel="noopener nofollow">$1</a>');
    function rich(text, fs) {
      if (!fs || !fs.length) return bare(text);
      let out = '', at = 0;
      for (const f of fs) {
        if (f[0] < at) continue;
        out += bare(text.slice(at, f[0]));
        const t = esc(text.slice(f[0], f[1]));
        const href = f[2] === 'link' ? f[3] : f[2] === 'mention' ? 'https://bsky.app/profile/' + f[3] : 'https://bsky.app/hashtag/' + encodeURIComponent(f[3]);
        out += `<a href="${esc(href)}" target="_blank" rel="noopener nofollow">${t}</a>`;
        at = f[1];
      }
      return out + bare(text.slice(at));
    }
    const img = (c, alt, cls, nm, w, h) => { const u = blob(c, nm); return u ? `<img class="${cls}" loading="lazy" src="${esc(u)}" alt="${esc(alt || '')}" title="${esc(alt || '')}"${w && h ? ` width="${w}" height="${h}"` : ''} data-full="${esc(u)}">` : `<span class="${cls} noblob" title="${esc(c)}">${icon('image')}</span>`; };
    const avatar = (cls) => p.avatar ? img(p.avatar, '', cls || 'at-av', 'avatar') : `<span class="${cls || 'at-av'} noblob">${BSKY}</span>`;
    function media(x) {
      let out = '';
      const ims = x.i || [];
      if (ims.length) {
        const one = ims.length === 1 && ims[0][3] ? ` style="aspect-ratio:${ims[0][3]}/${ims[0][4]}"` : '';
        out += `<div class="at-media n${Math.min(ims.length, 4)}"${one}>${ims.map((im, i) => img(im[0], im[1], 'at-img', x.k + '-' + (i + 1), im[3], im[4])).join('')}</div>`;
      }
      if (x.v) {
        const u = blob(x.v[0], x.k + '.mp4');
        out += u ? `<video class="at-video" controls preload="metadata" src="${esc(u)}" title="${esc(x.v[1] || '')}"${x.v[3] ? ` style="aspect-ratio:${x.v[3]}/${x.v[4]}"` : ''}></video>` : img(x.v[0], x.v[1], 'at-img');
      }
      if (x.l) {
        let host = '';
        try { host = new URL(x.l[0]).host; } catch (e) { /* not a URL */ }
        out += `<a class="at-card" href="${esc(x.l[0])}" target="_blank" rel="noopener nofollow">${x.l[3] ? img(x.l[3], '', 'at-thumb', 'thumb') : ''}<span><b>${esc(x.l[1] || x.l[0])}</b>${x.l[2] ? `<span class="at-desc">${esc(x.l[2])}</span>` : ''}<span class="faint">${esc(host)}</span></span></a>`;
      }
      if (x.q) out += quote(x.q);
      return out;
    }
    // A quoted post: shown from the archive when it is the account's own.
    function quote(uri) {
      const k = own(uri), q = k && byKey.get(k);
      if (q) return `<a class="at-quote" href="${esc(self(q.k))}"><span class="at-head">${avatar('at-av sm')}<b>${name}</b><span class="faint">@${esc(handle)} · ${esc(q.t ? day(q.t) : '')}</span></span>${q.x ? `<span class="at-text">${esc(q.x)}</span>` : ''}${(q.i || []).length ? `<span class="at-media n${Math.min(q.i.length, 4)}">${q.i.map((im, i) => img(im[0], im[1], 'at-img', q.k + '-' + (i + 1))).join('')}</span>` : ''}</a>`;
      const m = at(uri), w = web(uri);
      return `<a class="at-quote other" href="${esc(w || '#')}" target="_blank" rel="noopener"><span class="faint">${m && m[2] === 'app.bsky.feed.post' ? 'A post by another account' : esc(m ? collName(m[2]) : 'A record')}, not in this archive · Open on Bluesky ↗</span></a>`;
    }
    // One post, as Bluesky draws it: avatar, name, time, text, embeds. line
    // joins it to the next post of a thread.
    function card(x, opt) {
      opt = opt || {};
      const pk = own(x.r), parentHere = pk && byKey.has(pk);
      const ctx = x.r && !opt.inThread ? (pk ? (parentHere ? `<a class="faint" href="${esc(self(pk))}">↳ Replying to ${name}</a>` : '<span class="faint">↳ Replying to a post not in this archive</span>') : `<a class="faint" href="${esc(web(x.r))}" target="_blank" rel="noopener">↳ Replying to another account ↗</a>`) : '';
      const nk = (kids.get(x.k) || []).length, rh = recHref(x);
      return `<article class="at-post${opt.focus ? ' focus' : ''}${opt.line ? ' line' : ''}" id="p-${esc(x.k)}">
  <div class="at-left">${avatar()}</div>
  <div class="at-body">
    <div class="at-head"><b>${name}</b><span class="faint">@${esc(handle)}</span><a class="faint at-time" href="${esc(self(x.k))}" title="${esc(x.t)}">${x.t ? (opt.focus ? esc(when(x.t)) : day(x.t)) : 'undated'}</a></div>
    ${ctx ? `<div class="at-ctx">${ctx}</div>` : ''}
    ${x.x ? `<div class="at-text${opt.focus ? ' big' : ''}">${rich(x.x, x.f)}</div>` : ''}
    ${media(x)}
    <div class="at-foot faint">${nk && !opt.inThread ? `<a href="${esc(self(x.k))}">${plural(nk, 'reply', 'replies')} in this archive</a>` : ''}${x.lg && x.lg.length ? `<span>${esc(x.lg.join(', '))}</span>` : ''}<span class="at-links">${rh ? `<a href="${esc(rh)}"${pathOf(x.k) ? '' : ' target="_blank" rel="noopener"'}>Record</a>` : ''}${cidx(x.c, true)}${a.did ? `<a href="https://bsky.app/profile/${esc(a.did)}/post/${esc(x.k)}" target="_blank" rel="noopener">Bluesky ↗</a>` : ''}</span></div>
  </div>
</article>`;
    }
    // In the feed, a reply to the account's own post comes with that post
    // above it, as Bluesky shows it.
    function feedItem(x) {
      const pk = own(x.r), parent = pk && byKey.get(pk);
      return `<li>${parent ? card(parent, { line: true }) : ''}${card(x, { inThread: !!parent })}</li>`;
    }
    // A post with the posts above it and the replies below, as far as the
    // archive holds them.
    function thread(x) {
      const up = [];
      for (let k = own(x.r); k && byKey.has(k) && up.length < 200; k = own(byKey.get(k).r)) up.unshift(byKey.get(k));
      const down = [];
      const walk = (k, d) => (kids.get(k) || []).forEach((y) => { down.push([y, d]); if (down.length < 2000) walk(y.k, d + 1); });
      walk(x.k, 0);
      return { n: up.length + 1 + down.length, html: `${x.r && !own(x.r) ? `<a class="faint" href="${esc(web(x.r))}" target="_blank" rel="noopener">This replies to another account's post, which is not in this archive ↗</a>` : ''}
  ${x.r && own(x.r) && !byKey.has(own(x.r)) ? '<span class="faint">The post this replies to is not in this archive.</span>' : ''}
  ${up.map((y) => card(y, { line: true, inThread: true })).join('')}
  ${card(x, { focus: true, inThread: true })}
  ${down.length ? `<h3 class="at-replies">Replies</h3>${down.map(([y, d]) => `<div class="at-reply" style="margin-left:${Math.min(d, 6) * 24}px">${card(y, { inThread: true })}</div>`).join('')}` : ''}` };
    }
    const nOf = (c) => (a.counts || {})[c] || 0;
    const profile = () => `<div class="at-banner">${p.banner ? img(p.banner, '', 'at-banner-img', 'banner') : ''}</div>
  <div class="at-who">${avatar('at-avatar')}<div class="at-names">
    <h1>${name}</h1>
    <div class="meta"><span>@${esc(handle)}</span>${a.did ? `<a href="https://bsky.app/profile/${esc(a.did)}" target="_blank" rel="noopener">On Bluesky ↗</a>` : ''}</div>
  </div></div>
  <div class="at-counts"><span><b>${num(nOf('app.bsky.feed.post'))}</b> posts</span><span><b>${num(nOf('app.bsky.graph.follow'))}</b> following</span><span><b>${num(nOf('app.bsky.feed.like'))}</b> likes</span>${p.createdAt ? `<span class="faint">Joined ${esc(day(p.createdAt))}</span>` : ''}</div>
  ${p.description ? `<p class="at-bio">${bare(p.description)}</p>` : ''}`;
    // For lists: a post's first line, and a picture for its tile.
    const snippet = (x) => ((x.x || '').split('\n').find((l) => l.trim()) || '').slice(0, 140) || ((x.i || []).length ? 'Photo' : x.v ? 'Video' : x.l ? x.l[1] || 'Link' : x.q ? 'Quote' : 'Post');
    const picture = (x, w) => { const c = (x.i || [])[0] ? x.i[0][0] : x.l && x.l[3]; return c ? blob(c, x.k + '.jpg', LOCAL ? { w } : null) : null; };
    return { n, x0, a, p, blob, all, byKey, handle, name, own, web, self, pathOf, card, feedItem, thread, profile, snippet, picture, bare };
  }

  // The Bluesky App: the account's profile and feed, or one thread, in el.
  async function bskyApp(el, n) {
    const meta = await load('meta');
    const accts = meta.accounts || [];
    if (V) V.use({ esc, icon });
    LOCAL = await probeLocal();
    PUBLIC = params.get('public') === '1';
    const K = await bsky(n), x0 = K.x0, a = K.a, all = K.all, byKey = K.byKey, self = K.self;
    const S = { q: params.get('q') || '', show: params.get('show') || 'posts', limit: 40 };
    const isReply = (x) => !!x.r, hasMedia = (x) => !!((x.i && x.i.length) || x.v);
    const tabs = [['posts', 'Posts', (x) => !isReply(x) || !!K.own(x.r)], ['replies', 'Replies', isReply], ['media', 'Media', hasMedia], ['all', 'Everything', () => true]];
    const counts = Object.fromEntries(tabs.map((t) => [t[0], all.filter(t[2]).length]));

    pageKeys.cid = () => a.commit;
    const t = params.get('t');
    const head = `
${accts.length > 1 ? `<nav class="chips" aria-label="Accounts">${accts.map((x, i) => `<a class="pill" aria-pressed="${i === n}" href="${esc(appHref(i))}">${esc(x.name)}</a>`).join('')}</nav>` : ''}
${!LOCAL && !HERE && !PUBLIC ? `<div class="card none"><p class="faint">Open this explorer with <code>ipfs2fil explore</code> or through an IPFS gateway to see images and video here.</p><a class="btn sm" href="${esc(appHref(n, t || '', { public: 1 }))}">Load them from ${GATEWAYS[0][0]}</a></div>` : ''}`;

    if (t) {
      const x = byKey.get(t);
      if (!x) { el.innerHTML = head + `<section class="card"><h2>No post ${esc(t)} in this archive</h2><a href="${esc(appHref(n))}">All posts</a></section>`; return; }
      const th = K.thread(x);
      el.innerHTML = head + `<section class="card at-thread">
  <div class="titled"><a href="${esc(appHref(n))}">‹ ${K.name}'s posts</a><span class="faint">Thread · ${plural(th.n, 'post')} in this archive</span></div>
  ${th.html}
</section>`;
      status('thread of ' + plural(th.n, 'post'));
      lightbox(el);
      return;
    }

    const recs = Object.keys(a.records || {}).sort();
    el.innerHTML = head + `
<section class="card at-profile">${K.profile()}</section>
<div class="at-cols">
<section class="card at-feedcard">
  <div class="at-tabs" role="tablist">${tabs.map((c) => `<button type="button" role="tab" data-show="${c[0]}" aria-selected="${c[0] === S.show}">${c[1]} <span class="n">${num(counts[c[0]])}</span></button>`).join('')}</div>
  <div class="filter"><svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" aria-hidden="true"><circle cx="7" cy="7" r="5"></circle><path d="M11 11l3.5 3.5"></path></svg><input type="search" data-q placeholder="Filter posts by text" aria-label="Filter posts by text" autocomplete="off" spellcheck="false" value="${esc(S.q)}"><kbd>f</kbd></div>
  <ol class="at-feed"></ol>
</section>
<aside class="card at-side">
  <div class="titled"><h2>What this archive holds</h2></div>
  ${archivedHTML(x0, a, x0.path ? browseHref(x0.path) : '')}
</aside>
</div>
${recs.length ? `<section class="card"><h2>Other records</h2><p class="faint">Collections that are not posts, likes, reposts, follows and the like, as stored (up to 500 each). Every record is under <a href="${esc(x0.path ? browseHref(x0.path) : 'browse.html?kind=record&deep=1')}">Records</a> too.</p>${recs.map((c) => `<details class="at-coll"><summary><b>${esc(c)}</b> <span class="faint">${num((a.counts || {})[c] || 0)}</span></summary>${a.records[c].map((r) => `<div class="at-rec"><div class="faint"><code>${esc(r[0])}</code> ${cidx(r[1], true)}</div><pre class="code"><code>${(V ? V.hl(JSON.stringify(r[2], null, 2), 'json') : esc(JSON.stringify(r[2], null, 2)).split('\n')).map((l) => `<span>${l}</span>`).join('\n')}</code></pre></div>`).join('')}</details>`).join('')}</section>` : ''}`;
    const feed = el.querySelector('.at-feed');
    const shown = () => { const q = S.q.toLowerCase(), f = tabs.find((c) => c[0] === S.show)[2]; return all.filter((x) => f(x) && (!q || (x.x || '').toLowerCase().includes(q) || (x.l && (x.l[1] || '').toLowerCase().includes(q)))); };
    function render() {
      const list = shown();
      feed.innerHTML = list.slice(0, S.limit).map(K.feedItem).join('') + (list.length > S.limit ? `<li class="at-more"><button type="button" class="btn sm" data-more>Show ${num(Math.min(100, list.length - S.limit))} more of ${num(list.length - S.limit)}</button></li>` : '') + (!list.length ? '<li class="faint">No posts match.</li>' : '');
      status(plural(list.length, 'post') + (list.length !== all.length ? ' of ' + num(all.length) : ''));
    }
    const q = el.querySelector('[data-q]');
    // The tab and filter go in the URL; a tab is a step back and forward.
    const toURL = (push) => {
      const u = new URL(location.href);
      u.searchParams.set('show', S.show);
      if (S.q) u.searchParams.set('q', S.q); else u.searchParams.delete('q');
      if (u.href !== location.href) history[push ? 'pushState' : 'replaceState'](null, '', u.href);
    };
    const tab = (v) => { S.show = tabs.some((c) => c[0] === v) ? v : 'posts'; S.limit = 40; el.querySelectorAll('[data-show]').forEach((o) => o.setAttribute('aria-selected', o.dataset.show === S.show)); render(); };
    q.oninput = () => { S.q = q.value; S.limit = 40; toURL(false); render(); };
    el.querySelectorAll('[data-show]').forEach((b) => { b.onclick = () => { tab(b.dataset.show); toURL(true); }; });
    addEventListener('popstate', () => { const P = new URLSearchParams(location.search); q.value = S.q = P.get('q') || ''; tab(P.get('show') || 'posts'); });
    feed.addEventListener('click', (ev) => { if (ev.target.closest('[data-more]')) { S.limit += 100; render(); } });
    // Show more as the end comes into view.
    new IntersectionObserver((es) => { if (es.some((e) => e.isIntersecting) && feed.querySelector('[data-more]')) { S.limit += 100; render(); } }, { root: el, rootMargin: '600px' }).observe(feed);
    pageKeys.f = () => { q.focus(); return true; };
    // A link to a post (#p-<rkey>) opens its thread.
    const h = location.hash.match(/^#p-(.+)$/);
    if (h && byKey.has(decodeURIComponent(h[1]))) { location.replace(self(decodeURIComponent(h[1]))); return; }
    render();
    lightbox(el);
  }
  // Images open full size over the page; Esc or a click closes them.
  function lightbox(root) {
    if (root.dataset.lightbox) return;
    root.dataset.lightbox = '1';
    root.addEventListener('click', (ev) => {
      const im = ev.target.closest('img[data-full]');
      if (!im || ev.target.closest('a')) return;
      ev.preventDefault();
      const d = D.createElement('div');
      d.className = 'at-lightbox';
      d.innerHTML = `<img src="${esc(im.dataset.full)}" alt="${esc(im.alt)}">${im.alt ? `<p>${esc(im.alt)}</p>` : ''}`;
      const close = () => { d.remove(); D.removeEventListener('keydown', key, true); };
      const key = (e) => { if (e.key === 'Escape') { e.stopPropagation(); close(); } };
      d.onclick = close;
      D.addEventListener('keydown', key, true);
      D.body.appendChild(d);
    });
  }

  // ---- Search: now part of Files, which searches under any folder.
  function search() {
    const q = params.get('q') || '', w = params.get('where');
    location.replace('browse.html?' + qs({ p: w && w !== 'any' ? w : '', q, kind: params.get('kind') || '', deep: '1' }));
  }

  // ---- Start
  frame();
  D.querySelector('footer').before(Object.assign(D.createElement('main'), { innerHTML: '<p class="muted">Loading…</p>' }));
  const pages = { overview, browse: files, pin, pieces: piecesPage, search, posts };
  manifest().then((m) => {
    ROOTS = {};
    PIECE = {};
    let bundle = null;
    (m.pieces || []).forEach((p) => {
      PIECE[p.pieceCid] = p.index;
      if (p.part === 'bundle' && p.blockList && (!bundle || p.snapshot >= bundle.snapshot)) bundle = p;
    });
    if (bundle) PIECE[''] = bundle.index;
    (m.roots || []).forEach((r) => { ROOTS[r.path] = r; });
    return load('meta').then((meta) => {
      REPOS = meta.repos || null;
      ATPATHS = (meta.accounts || []).map((a) => a.path).filter(Boolean);
      if (meta.format > 2) throw new Error('This archive was made by a newer ipfs2fil; its index format is ' + meta.format + '.');
      return (pages[PAGE] || overview)();
    });
  }).catch(fail);
})();
