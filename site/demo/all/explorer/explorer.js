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
    return b[p] || null;
  }
  // Shards name pieces by PieceCID; PIECE maps them to their index here.
  let PIECE = {};
  const pieceAt = (cid) => (cid && PIECE[cid] != null ? PIECE[cid] : -1);
  function entryOf(s, r) {
    const dir = s.dirs[r[0]], seg = r.length > 6 ? r[6] : r[1];
    return { dir, name: r[1], kind: r[2], bytes: r[3], cid: r[4], piece: r[5] >= 0 ? pieceAt(s.pieces[r[5]]) : -1, path: dir ? dir + '/' + seg : seg };
  }
  async function listDir(info) {
    const parts = await Promise.all(info.chunks.map(async (c) => {
      const s = await load('s/' + c[0]);
      return s.rows.slice(c[1], c[1] + c[2]).map((r) => entryOf(s, r));
    }));
    return [].concat(...parts);
  }

  // ---- Formatting
  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const num = (n) => Number(n).toLocaleString('en-US');
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
  const segs = (p) => p.split('/').map(encodeURIComponent).join('/');

  // Kinds: icon path, color, label, plural label. From the boards.
  const KIND = {
    folder: ['M1.5 4.5h4l1.5 1.5h7.5v7.5h-13z', '#1B5FD6', 'Folder', 'Folders'],
    image: ['M2 2.5h12v11h-12z M2 11l3.5-3.5 3 3 2-2 3.5 3.5', '#2F6FB0', 'Photo', 'Photos'],
    video: ['M1.5 3.5h9v9h-9z M10.5 7l4-2.5v7l-4-2.5', '#0B3B8C', 'Video', 'Videos'],
    audio: ['M6 12V3.5l7-1.5v8.5 M6 12a2 2 0 1 1-4 0a2 2 0 0 1 4 0z M13 10.5a2 2 0 1 1-4 0a2 2 0 0 1 4 0z', '#6D28D9', 'Audio', 'Audio'],
    page: ['M1.5 2.5h13v11h-13z M1.5 5.5h13', '#4B5260', 'Web page', 'Web pages'],
    doc: ['M4 1.5h5l3 3v10h-8z M9 1.5v3h3 M5.5 9h5', '#B91C1C', 'Document', 'Documents'],
    text: ['M4 1.5h5l3 3v10h-8z M9 1.5v3h3 M6 8.5h4 M6 11h4', '#9A3412', 'Text', 'Text'],
    code: ['M5.5 4.5l-3.5 3.5 3.5 3.5 M10.5 4.5l3.5 3.5-3.5 3.5', '#B45309', 'Code', 'Code'],
    archive: ['M3 1.5h10v13h-10z M8 1.5v1.5 M8 4.5v1.5 M7 7.5h2v2.5h-2z', '#047857', 'Archive', 'Archives'],
    disk: ['M8 1.5a6.5 6.5 0 1 0 0 13a6.5 6.5 0 0 0 0-13z M8 6.5a1.5 1.5 0 1 0 0 3a1.5 1.5 0 0 0 0-3z', '#0E7490', 'Disk image', 'Disk images'],
    other: ['M4 1.5h5l3 3v10h-8z M9 1.5v3h3', '#8A919E', 'Other', 'Other']
  };
  const kindOf = (k) => KIND[k] || KIND.other;
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
    overview: [[['j', 'k'], 'move'], [['↵'], 'open'], [['/'], 'search'], [['g', 'f'], 'files'], [['g', 'p'], 'pieces'], [['y'], 'copy root']],
    browse: [[['j', 'k'], 'move'], [['↵'], 'open'], [['u'], 'up a level'], [['/'], 'search here'], [['v'], 'view'], [['l'], 'layout'], [['space'], 'focus the file'], [['d'], 'download'], [['y'], 'copy CID']],
    pin: [[['j', 'k'], 'move'], [['↵'], 'open'], [['u'], 'up a level'], [['o'], 'open in gateway'], [['y'], 'copy CID'], [['g', 'f'], 'files']],
    pieces: [[['j', 'k'], 'move'], [['↵'], 'open'], [['f'], 'filter'], [['s'], 'sort by'], [['r'], 'reverse'], [['y'], 'copy PieceCID'], [['g', 'o'], 'overview']],
    search: [[['↓'], 'into results'], [['j', 'k'], 'move'], [['↵'], 'open'], [['1', '…', '9'], 'kind'], [['0'], 'all kinds'], [['esc'], 'clear'], [['y'], 'copy CID']]
  };
  const GROUPS = [
    ['Anywhere', [['Search', ['/']], ['This list', ['?']], ['Go to overview', ['g', 'o']], ['Go to files', ['g', 'f']], ['Go to pieces', ['g', 'p']], ['Close or cancel', ['esc']]]],
    ['Lists', [['Next or previous row', ['j', 'k']], ['Same, with arrows', ['↓', '↑']], ['Open the row', ['↵']], ['Up a level', ['u', '⌫']], ['Filter the list', ['f']], ['Copy the CID', ['y']], ['Open in a gateway', ['o']], ['Sort by the next column', ['s']], ['Reverse the order', ['r']]]],
    ['Files', [['Search this folder and below', ['/']], ['Move into results', ['↓']], ['Filter by kind', ['1', '…', '9']], ['Show all kinds', ['0']], ['List, grid, tree, columns', ['v']], ['Wide list, split, wide file', ['l']], ['Give the file the room', ['space']], ['Download the file', ['d']], ['Open a tree folder', ['→']], ['Close a tree folder', ['←']]]]
  ];
  let statusEl;
  function frame() {
    const nav = [['overview', 'index.html', 'Overview'], ['browse', 'browse.html', 'Files'], ['pieces', 'pieces.html', 'Pieces']]
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
  <div class="eyebrow">${REPOS ? `All ${plural(REPOS.length, 'repo')} on this machine, together` : `Archive of a kubo repo · fs-repo ${esc(m.source.repoVersion)}`}</div>
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
${REPOS ? `<section class="card"><h2>Repos</h2><div id="repos"></div></section>` : ''}
<div class="split">
  <section class="card wide"><div class="titled"><h2>Largest roots</h2><a href="${browseHref('')}">Browse all</a></div><div id="roots"></div></section>
  <section class="card side" style="gap:12px"><div class="titled"><h2>Pieces</h2><a href="pieces.html">All ${num(pieces.length)}</a></div>
    <div class="toggle" role="group" aria-label="Color pieces by"><button type="button" class="pill" data-by="part" aria-pressed="true">Part of repo</button><button type="button" class="pill" data-by="type" aria-pressed="false">File type</button></div>
    <div class="pmap" role="img" id="pmap"></div>
    <div class="faint" id="pnote"></div></section>
</div>`;
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
        label: 'Repos', step: 50, sortSelect: false, sort0: null, filter: null,
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
  let LOCAL = false, PUBLIC = false;
  async function probeLocal() {
    if (HERE || !/^https?:$/.test(location.protocol)) return false;
    try { const r = await fetch('../_i2f/info'); return r.ok && !!(await r.json()).content; } catch (e) { return false; }
  }
  const qs = (o) => Object.keys(o).filter((k) => o[k] !== '' && o[k] != null).map((k) => k + '=' + encodeURIComponent(o[k])).join('&');
  function contentURL(e, extra) {
    if (!e || !e.cid) return null;
    if (LOCAL) return '../_i2f/content/' + encodeURIComponent(e.cid) + '?' + qs({ name: e.name, ...extra });
    if (HERE) return '../data/' + segs(e.path) + (extra && extra.dl ? '?download=true&filename=' + encodeURIComponent(e.name) : '');
    if (PUBLIC) return gw(e.cid) + '?filename=' + encodeURIComponent(e.name) + (extra && extra.dl ? '&download=true' : '');
    return null;
  }
  const thumbURL = (e, w) => contentURL(e, LOCAL ? { w: w || 320 } : null);
  const canShow = () => LOCAL || HERE || PUBLIC;

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
  const LIB = [['all', 'Everything'], ['image', 'Photos'], ['video', 'Videos'], ['audio', 'Audio'], ['doc', 'Documents'], ['page', 'Web pages'], ['text', 'Text'], ['code', 'Code'], ['archive', 'Archives'], ['disk', 'Disk images'], ['other', 'Other']];
  const pref = (k, d) => { try { return localStorage.getItem('i2f.' + k) || d; } catch (e) { return d; } };
  const keep = (k, v) => { try { localStorage.setItem('i2f.' + k, v); } catch (e) { /* private mode */ } };
  const svg = (d, w) => `<svg width="${w || 16}" height="16" viewBox="0 0 ${w || 16} 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round" aria-hidden="true"><path d="${d}"></path></svg>`;
  const ext = (n) => { const i = n.lastIndexOf('.'); return i > 0 ? n.slice(i + 1).toLowerCase() : ''; };

  async function files() {
    const [m, meta] = await Promise.all([manifest(), load('meta')]);
    LOCAL = await probeLocal();
    const S = {
      p: params.get('p') || '', q: params.get('q') || '', kind: params.get('kind') || 'all', deep: params.get('deep') === '1' || !!params.get('q'),
      size: params.get('size') || 'any', sort: params.get('sort') || 'name', dir: params.get('dir') === '-1' ? -1 : 1,
      view: pref('view', 'list'), ly: pref('layout', 'split'), sel: params.get('sel') || '', cur: -1, limit: 300, open: new Set()
    };
    if (S.kind !== 'all' && !params.has('deep')) S.deep = true;
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
      const libOn = S.deep && !S.q && !S.p;
      $('.lib').innerHTML = `<div class="h">Library</div>${LIB.filter((k) => k[0] === 'all' || t[k[0]]).map((k) => `<a href="${libHref(k[0])}"${libOn && S.kind === k[0] ? ' aria-current="page"' : ''}><span style="color:${k[0] === 'all' ? 'var(--muted)' : kindOf(k[0])[1]}">${k[0] === 'all' ? svg(VIEWS[1][2]) : icon(k[0])}</span>${k[1]}<span class="n">${k[0] === 'all' ? count(meta.entries) : count(t[k[0]][0])}</span></a>`).join('')}
<div class="h">${REPOS ? 'Repos' : 'Places'}</div><a href="browse.html"${!S.p && !S.deep ? ' aria-current="page"' : ''}>${icon('folder')}${REPOS ? 'All repos' : 'All of data/'}</a>${(REPOS ? REPOS.map((r) => [r.name, r.name]) : ['pins', 'mfs', 'orphans'].filter((w) => parts[w]).map((w) => [w, PART[w][0]])).map((w) => `<a href="${browseHref(w[0])}"${S.p === w[0] && !S.deep ? ' aria-current="page"' : ''}>${icon('folder')}${esc(w[1])}</a>`).join('')}
<div class="h">Smart</div><a href="browse.html?${qs({ deep: '1', sort: 'size', dir: '-1' })}">${svg('M2 13h12M4 13V8M8 13V3M12 13V6')}Largest files</a>`;
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
      const src = S.deep ? (all || []).filter(under(S.p)) : kids;
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
      // a grid of results groups them by folder
      if (S.view === 'grid' && S.deep && S.sort === 'name') shown = shown.map((e, i) => [e, i]).sort((a, b) => a[0].dir < b[0].dir ? -1 : a[0].dir > b[0].dir ? 1 : a[1] - b[1]).map((x) => x[0]);
      if (S.sel) { const i = shown.findIndex((e) => e.path === S.sel || (e.also && e.also.some((o) => o.path === S.sel))); if (i >= 0) { S.cur = i; S.limit = Math.max(S.limit, i + 50); } }
    }
    // A search shows each CID once; the other places it sits go in .also.
    function once(es) {
      const by = new Map(), out = [];
      for (const e of es) {
        const k = e.kind + ' ' + e.cid, first = by.get(k);
        if (first) { first.also.push(e); continue; }
        const r = Object.assign({}, e, { also: [] });
        by.set(k, r); out.push(r);
      }
      return out;
    }
    const current = () => (S.view === 'tree' ? rows[S.cur] : shown[S.cur]) || null;

    // ---- render
    let lastURL = '';
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
      preview(full);
      const u = 'browse.html?' + qs({ p: S.p, q: S.q, kind: S.kind === 'all' ? '' : S.kind, deep: S.deep && !S.q ? '1' : '', size: S.size === 'any' ? '' : S.size, sort: S.sort === 'name' ? '' : S.sort, dir: S.dir < 0 ? '-1' : '', sel: S.sel });
      if (u !== lastURL) { history.replaceState(null, '', u); lastURL = u; }
      status((S.deep ? `${num(shown.length)}${loadingAll ? '+' : ''} found` : showPath(S.p) || 'data') + (current() ? ' · ' + current().name : ''));
    }
    const more = () => shown.length > S.limit ? `<div class="foot"><button type="button" class="more" data-more>Show ${num(Math.min(300, shown.length - S.limit))} more</button><span>${num(S.limit)} of ${num(shown.length)}</span></div>` : '';
    const empty = () => `<p class="muted pad">${S.deep && loadingAll ? 'Searching…' : S.q ? 'No names match.' : S.kind !== 'all' ? `No ${esc(kindOf(S.kind)[3].toLowerCase())} here.` : 'This folder is empty.'}</p>`;
    const thumbOr = (e, cls) => e.kind === 'image' && canShow() ? `<img class="${cls}" loading="lazy" alt="" src="${esc(thumbURL(e, cls === 'tt' ? 320 : 96))}">` : `<span class="${cls} ic">${icon(e.kind)}</span>`;
    const where = (e) => showPath(e.dir) || 'data';
    function listView() {
      const deepCols = S.deep;
      $('.flist').innerHTML = shown.length ? `<table><thead><tr><th>Name</th>${deepCols ? '<th class="w">Folder</th>' : ''}<th class="k">Kind</th><th class="r">Size</th><th class="r pc">Piece</th></tr></thead><tbody>${shown.slice(0, S.limit).map((e, i) => `<tr data-i="${i}" aria-selected="${i === S.cur}"><td><span class="nm">${thumbOr(e, 'th')}<a href="${esc(e.kind === 'folder' ? browseHref(e.path) : '#')}" data-open="${i}">${esc(e.name)}${e.kind === 'folder' ? '/' : ''}</a></span></td>${deepCols ? `<td class="w"><a class="muted" href="${esc(browseHref(e.dir))}">${esc(where(e))}/</a>${e.also && e.also.length ? ` <span class="faint">+${num(e.also.length)}</span>` : ''}</td>` : ''}<td class="k muted">${esc(kindOf(e.kind)[2])}</td><td class="r">${e.kind === 'folder' && !e.bytes ? '' : bytes(e.bytes)}</td><td class="r pc">${e.piece >= 0 ? `<a href="pieces.html?n=${e.piece}">${e.piece}</a>` : ''}</td></tr>`).join('')}</tbody></table>${more()}` : empty();
    }
    function grid() {
      let out = '', lastDir = null;
      const group = S.deep && S.sort === 'name', list = shown;
      list.slice(0, S.limit).forEach((e, i) => {
        if (group && e.dir !== lastDir) { out += `${lastDir !== null ? '</div>' : ''}<div class="gh"><a href="${esc(browseHref(e.dir))}">${esc(where(e))}</a></div><div class="tiles">`; lastDir = e.dir; }
        else if (!group && i === 0) out += '<div class="tiles">';
        out += `<button type="button" class="tile" data-i="${i}" aria-selected="${i === S.cur}" title="${esc(e.name)}">${thumbOr(e, 'tt')}<span class="tn">${esc(e.name)}</span></button>`;
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
          rows.push({ ...e, depth, open, branch: e.kind === 'folder' });
          if (open && TREE[e.path]) walk(TREE[e.path], depth + 1);
        });
        walk(shown, 0);
      }
      if (S.sel) { const i = rows.findIndex((e) => e.path === S.sel); if (i >= 0) S.cur = i; }
      $('.flist').innerHTML = rows.length ? `<div class="tree" role="tree">${rows.slice(0, S.limit + 200).map((e, i) => `<div class="tr" role="treeitem" data-i="${i}" aria-selected="${i === S.cur}"${e.branch ? ` aria-expanded="${e.open}"` : ''} style="padding-left:${8 + e.depth * 18}px"><button type="button" class="tw" data-tw="${i}" tabindex="-1" aria-hidden="true">${e.branch ? (e.open ? '▾' : '▸') : ''}</button>${thumbOr(e, 'th')}<span class="tnm">${esc(e.name)}</span><span class="n">${e.n ? num(e.n) : e.kind === 'folder' && !e.bytes ? '' : bytes(e.bytes)}</span></div>`).join('')}</div>` : empty();
    }
    const TREE = {};
    async function toggle(e) {
      if (e.kind !== 'folder' || S.deep) return;
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
      $('.flist').innerHTML = `<div class="cols">${all.map((c, ci) => `<div class="col" data-c="${ci}">${c.items.slice(0, ci === all.length - 1 ? S.limit : 2000).map((e, i) => `<div class="ci" data-c="${ci}" data-i="${i}" aria-selected="${i === c.cur}">${icon(e.kind)}<span class="tnm">${esc(e.name)}</span>${e.kind === 'folder' ? '<span class="n">›</span>' : ''}</div>`).join('') || '<p class="muted pad">Empty</p>'}</div>`).join('')}</div>`;
      const c = $('.cols');
      c.scrollLeft = c.scrollWidth;
    }

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
      const url = contentURL(e), copies = m.copies || [];
      const piece = e.piece >= 0 ? (m.pieces || [])[e.piece] : null;
      const stored = piece ? copies.filter((c) => c.pieceIds && c.pieceIds[e.piece] != null) : [];
      const nav = `<button type="button" class="ib" data-step="-1" aria-label="Previous (k)">‹</button><button type="button" class="ib" data-step="1" aria-label="Next (j)">›</button>`;
      pane.innerHTML = `<div class="ph"><strong title="${esc(e.name)}">${esc(e.name)}</strong>${nav}${url ? `<a class="btn sm" href="${esc(contentURL(e, { dl: 1 }))}" download="${esc(e.name)}">Download</a>` : ''}</div>
<div class="stage" data-kind="${esc(e.kind)}">${stage(e, url)}</div>
<div class="facts2">
  <div><span>Size</span><b>${bytes(e.bytes)}</b></div><div><span>Kind</span><b>${esc(kindOf(e.kind)[2])}${ext(e.name) ? ' · ' + esc(ext(e.name).toUpperCase()) : ''}</b></div>
  <div><span>Piece</span><b>${e.piece >= 0 ? `<a href="pieces.html?n=${e.piece}">#${e.piece}</a>` : '—'}</b></div><div><span>Copies</span><b>${stored.length || (piece ? '0' : '—')}</b></div>
</div>
<div class="facts dl">
  <span>CID</span><span>${cidx(e.cid)} <button type="button" class="link" data-copy="${esc(e.cid)}">Copy</button></span>
  <span>Open in</span><span class="gw">${HERE ? `<a href="${esc(HERE + e.cid)}" target="_blank" rel="noopener">this gateway ↗</a>` : ''}${GATEWAYS.map((g) => `<a href="${esc(g[1] + e.cid + '?filename=' + encodeURIComponent(e.name))}" target="_blank" rel="noopener">${g[0]} ↗</a>`).join('')}<a href="${esc(gw(e.cid) + '?format=car')}" target="_blank" rel="noopener">CAR</a></span>
  <span>In${e.also && e.also.length ? ` (${num(e.also.length + 1)})` : ''}</span>${places(e)}
  <span>Folder</span><span class="pdir faint">…</span>
  ${piece ? `<span>Stored</span><span>${cidx(piece.pieceCid, true)} ${stored.length ? `<span class="tags">${stored.map((c) => `<span class="tag">${esc(c.provider)}</span>`).join('')}</span>` : '<span class="faint">not stored yet</span>'}</span>` : ''}
</div>`;
      fill(pane, e, url);
      if (isPDF(e) && url) deck.show(url, pane.querySelector('.stage'), e.name);
      warmAround();
      const di = await dirInfo(e.dir), pd = pane.querySelector('.pdir');
      if (pd && current() === e) pd.outerHTML = di && di.cid ? `<span>${cidx(di.cid, true)} <span class="gw">${GATEWAYS.map((g) => `<a href="${esc(g[1] + di.cid)}" target="_blank" rel="noopener">${g[0]} ↗</a>`).join('')}</span></span>` : '<span class="faint">—</span>';
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
      switch (e.kind) {
        case 'image': return `<img src="${esc(url)}" alt="${esc(e.name)}">`;
        case 'video': return `<video src="${esc(url)}" controls preload="metadata"></video>`;
        case 'audio': return `<div class="none">${icon('audio')}<audio src="${esc(url)}" controls preload="metadata"></audio></div>`;
        case 'page': return `<iframe sandbox="" src="${esc(url)}" title="${esc(e.name)}"></iframe>`;
        case 'doc': return x === 'pdf' || !x ? '' : `<div class="none">${icon('doc')}<p>No preview for .${esc(x)} files.</p></div>`;
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
        p = fetch(url, { headers: { Range: 'bytes=0-' + (n - 1) } }).then((r) => {
          if (!r.ok) throw new Error(r.status === 404 ? 'This file is not in the pieces on this machine.' : r.statusText);
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
        deck.keep(offs.map((d) => shown[S.cur + d]).filter((e) => isPDF(e) && e.bytes <= 64 << 20 && contentURL(e)).slice(0, 4).map((e) => [contentURL(e), e.name]));
        for (const d of offs) {
          const e = shown[S.cur + d], url = e && contentURL(e);
          if (!url || e.kind === 'folder' || warmed.has(url)) continue;
          if (isPDF(e)) continue;
          const doc = e.kind === 'page';
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
      function frame(url, title) {
        let f = frames.get(url);
        if (f) { frames.delete(url); frames.set(url, f); return f; }
        f = D.createElement('iframe');
        f.src = url; f.title = title || ''; f.tabIndex = -1;
        el().appendChild(f);
        frames.set(url, f);
        return f;
      }
      return {
        show(url, st, title) {
          const d = el();
          if (!d || !st) return;
          stageEl = st;
          if (ro) { ro.disconnect(); ro.observe(st); }
          place();
          on = frame(url, title);
          on.tabIndex = 0;
          frames.forEach((f) => f.classList.toggle('on', f === on));
          d.hidden = false;
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
          frames.forEach((f, u) => { if (f !== on && !want.has(u)) { f.remove(); frames.delete(u); } });
        },
      };
    })();
    async function fill(pane, e, url) {
      const st = pane.querySelector('.stage');
      if (!url || !st) return;
      const want = e.path;
      const range = (n) => head(url, n);
      try {
        if (e.kind === 'text' || e.kind === 'code') {
          const buf = await range(256 << 10);
          if (current() && current().path !== want) return;
          const lines = new TextDecoder().decode(buf).split('\n');
          const cut = buf.byteLength >= 256 << 10 || e.bytes > buf.byteLength;
          st.innerHTML = `<pre class="code"><code>${lines.slice(0, 5000).map((l) => `<span>${esc(l)}</span>`).join('\n')}</code></pre>${cut || lines.length > 5000 ? '<p class="faint pad">Showing the start. Download for all of it.</p>' : ''}`;
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
      } catch (err) { st.innerHTML = `<div class="none"><p>${esc(err.message)}</p></div>`; }
      const media = st.querySelector('img, video, audio');
      if (media) media.onerror = () => { st.innerHTML = `<div class="none"><p>${LOCAL ? 'This file is not in the pieces on this machine, or the browser cannot show it.' : 'The browser could not load or show this file.'}</p></div>`; };
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
      S.cur = Math.max(0, Math.min(n - 1, i));
      if (S.cur >= S.limit - 5 && n > S.limit) S.limit += 300;
      S.sel = (S.view === 'tree' ? rows : shown)[S.cur].path;
      update();
      const el = $(`.flist [data-i="${S.cur}"]${S.view === 'cols' ? `[data-c="${COLS.length}"]` : ''}`);
      if (el) el.scrollIntoView({ block: 'nearest' });
    };
    function open(e) {
      if (!e) return;
      if (e.kind === 'folder') { S.deep = false; S.q = ''; box.value = ''; S.kind = 'all'; enter(e.path).catch(fail); return; }
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
      if (b.dataset.tw != null) { const e = rows[+b.dataset.tw]; if (e) toggle(e); return; }
      if (b.dataset.open != null) { ev.preventDefault(); const e = shown[+b.dataset.open]; if (e && e.kind === 'folder') open(e); else sel(+b.dataset.open); return; }
      if (b.dataset.c != null && b.dataset.i != null) {
        const ci = +b.dataset.c, e = ci < COLS.length ? COLS[ci].items[+b.dataset.i] : shown[+b.dataset.i];
        if (!e) return;
        if (e.kind === 'folder') { enter(e.path).catch(fail); return; }
        if (ci < COLS.length) { S.sel = e.path; enter(COLS[ci].dir, true).catch(fail); return; }
        sel(+b.dataset.i); return;
      }
      if (b.dataset.i != null && b.tagName !== 'A') { sel(+b.dataset.i); }
    });
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
    const across = () => S.view === 'grid' ? Math.max(1, Math.round($('.flist').clientWidth / 150)) : 1;
    Object.assign(pageKeys, {
      j: step(1), k: step(-1), ArrowDown: () => step(across())(), ArrowUp: () => step(-across())(),
      ArrowRight: () => { if (S.view === 'grid') return step(1)(); const e = current(); if (S.view === 'tree' && e && e.branch && !e.open) { toggle(e); return true; } return false; },
      ArrowLeft: () => { if (S.view === 'grid') return step(-1)(); const e = current(); if (S.view === 'tree' && e && e.open) { toggle(e); return true; } return false; },
      Enter: () => { open(current()); return true; }, ' ': () => { S.ly = S.ly === 'file' ? 'split' : 'file'; keep('layout', S.ly); update(); return true; },
      u: up, Backspace: up, f: () => { box.focus(); box.select(); return true; }, '/': () => { box.focus(); box.select(); return true; },
      v: () => { S.view = VIEWS[(VIEWS.findIndex((v) => v[0] === S.view) + 1) % VIEWS.length][0]; keep('view', S.view); update(true); toast(VIEWS.find((v) => v[0] === S.view)[1] + ' view'); return true; },
      l: () => { S.ly = LAYOUTS[(LAYOUTS.findIndex((v) => v[0] === S.ly) + 1) % LAYOUTS.length][0]; keep('layout', S.ly); update(); return true; },
      d: () => { const e = current(), u = e && e.kind !== 'folder' && contentURL(e, { dl: 1 }); if (u) location.href = u; return !!u; },
      s: () => { const ks = Object.keys(SORTS); S.sort = ks[(ks.indexOf(S.sort) + 1) % ks.length]; update(); toast('Sorted by ' + $('#fsort').selectedOptions[0].textContent); return true; },
      r: () => { S.dir = -S.dir; update(); return true; }
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
    const n = r.lastPiece - r.firstPiece + 1, copies = m.copies || [];
    pageKeys.cid = () => r.cid;
    pageKeys.u = pageKeys.Backspace = () => { location.href = browseHref(r.path.includes('/') ? r.path.slice(0, r.path.lastIndexOf('/')) : ''); return true; };
    const ukind = info ? 'UnixFS directory' : r.cid.startsWith('bafyr') ? 'dag-cbor, not UnixFS' : 'File or single block';
    main().innerHTML = `
${crumbs(r.path)}
<section class="head">
  <div class="eyebrow"><span>${esc(ROOTKIND[r.kind] || r.kind)}</span><span>In ${plural(n, 'piece')}${copies.length ? `, ${plural(copies.length, 'copy', 'copies')} each` : ''}</span></div>
  <h1>${esc(r.name || shortCID(r.cid))}</h1>
  <div class="meta"><code class="chipcode">${esc(r.cid)}</code><button type="button" class="link" data-copy="${esc(r.cid)}">Copy CID</button></div>
  <p class="note">${r.name ? 'Older repos keep no pin names, so where there is none the explorer may guess one, such as an index page title. The CID is the real identity.' : 'This root has no name, so it is shown by its CID.'}</p>
</section>
<section class="stats" aria-label="Facts">
  <div class="card tight stat"><div class="k">Size</div><div class="v">${bytes(r.bytes)}</div></div>
  <div class="card tight stat"><div class="k">Blocks</div><div class="v">${count(r.blocks)}</div></div>
  <div class="card tight stat"><div class="k">Kind</div><div class="v" style="font-size:20px">${esc(ukind)}</div></div>
  <div class="card tight stat"><div class="k">Pieces</div><div class="v">${n === 1 ? r.firstPiece : r.firstPiece + ' to ' + r.lastPiece}</div></div>
</section>
<div class="split">
  <section class="card wide"><h2>Where it's stored</h2><div id="list"></div><a href="pieces.html?n=${r.firstPiece}">${n === 1 ? 'This piece' : 'These ' + num(n) + ' pieces'} among all pieces</a></section>
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
    for (let i = r.firstPiece; i <= r.lastPiece && i < (m.pieces || []).length; i++) rows.push(m.pieces[i]);
    const list = new List(D.getElementById('list'), {
      label: 'Pieces of this root', step: 100, sort0: 'n', filter: null, sortSelect: false,
      cols: [{ key: 'n', label: 'Piece', sort: (p) => p.index, html: (p) => `<a href="pieces.html?n=${p.index}">#${p.index}</a>` },
        { key: 'cid', label: 'PieceCID', cls: 'cid', html: (p) => cidx(p.pieceCid, true) },
        { key: 'size', label: 'Size', cls: 'r', sort: (p) => p.carBytes, html: (p) => bytes(p.carBytes) },
        { key: 'copies', label: 'Copies', cls: 'hide-sm', html: (p) => `<span class="tags">${copies.filter((c) => c.pieceIds && c.pieceIds[p.index] != null).map((c) => `<span class="tag">${esc(c.provider)}</span>`).join('') || '<span class="faint">not stored yet</span>'}</span>` }],
      href: (p) => 'pieces.html?n=' + p.index, cid: (p) => p.pieceCid, gateway: false, statusOf: (p) => '#' + p.index + ' · ' + (meta.pieces[p.index] || {}).label
    });
    list.setItems(rows);
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

  // ---- Search: now part of Files, which searches under any folder.
  function search() {
    const q = params.get('q') || '', w = params.get('where');
    location.replace('browse.html?' + qs({ p: w && w !== 'any' ? w : '', q, kind: params.get('kind') || '', deep: '1' }));
  }

  // ---- Start
  frame();
  D.querySelector('footer').before(Object.assign(D.createElement('main'), { innerHTML: '<p class="muted">Loading…</p>' }));
  const pages = { overview, browse: files, pin, pieces: piecesPage, search };
  manifest().then((m) => {
    ROOTS = {};
    PIECE = {};
    (m.pieces || []).forEach((p) => { PIECE[p.pieceCid] = p.index; });
    (m.roots || []).forEach((r) => { ROOTS[r.path] = r; });
    return load('meta').then((meta) => {
      REPOS = meta.repos || null;
      if (meta.format > 2) throw new Error('This archive was made by a newer ipfs2fil; its index format is ' + meta.format + '.');
      return (pages[PAGE] || overview)();
    });
  }).catch(fail);
})();
