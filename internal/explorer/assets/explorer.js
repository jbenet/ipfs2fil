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
      s.src = 'idx/' + key + '.js';
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
  function entryOf(s, r) {
    const dir = s.dirs[r[0]], seg = r.length > 6 ? r[6] : r[1];
    return { dir, name: r[1], kind: r[2], bytes: r[3], cid: r[4], piece: r[5], path: dir ? dir + '/' + seg : seg };
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
  const gw = (cid) => (HERE || GATEWAYS[0][1]) + cid;
  const gwLinks = (cid) => `<span class="gw">${HERE ? `<a href="${esc(HERE + cid)}" target="_blank" rel="noopener">this gateway ↗</a>` : ''}${GATEWAYS.map((g) => `<a href="${esc(g[1] + cid)}" target="_blank" rel="noopener">${g[0]} ↗</a>`).join('')}</span>`;

  // ---- Names for roots: pins live at pins/<mode>/<cid>; show their names instead.
  let ROOTS = {};
  function rootAt(p) { return ROOTS[p]; }
  const rootLabel = (r) => r.name || (r.kind === 'mfs' ? 'mfs' : r.cid);
  function showPath(p) {
    const parts = p ? p.split('/') : [];
    const out = [];
    for (let i = 0; i < parts.length; i++) {
      const r = rootAt(parts.slice(0, i + 1).join('/'));
      if (r && r.kind !== 'mfs') { out.length = 0; out.push(rootLabel(r)); } else out.push(parts[i]);
    }
    return out.join('/');
  }
  function browseHref(p) { return 'browse.html' + (p ? '?p=' + encodeURIComponent(p) : ''); }
  function entryHref(e) {
    const r = rootAt(e.path);
    if (r && r.kind !== 'mfs') return 'pin.html?cid=' + encodeURIComponent(r.cid);
    if (e.kind === 'folder') return browseHref(e.path);
    if (HERE) return '../data/' + segs(e.path);
    return e.cid ? gw(e.cid) : browseHref(e.dir);
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
  const openTab = (url) => url && window.open(url, '_blank', 'noopener');

  // ---- Page frame: header, bottom rail, shortcuts dialog
  const LOGO = '<svg width="22" height="22" viewBox="0 0 22 22" fill="none" stroke="#1B5FD6" stroke-width="1.8" aria-hidden="true"><rect x="2" y="2" width="8" height="8" rx="1.5"></rect><rect x="12" y="2" width="8" height="8" rx="1.5"></rect><rect x="2" y="12" width="8" height="8" rx="1.5"></rect><rect x="12" y="12" width="8" height="8" rx="1.5" fill="#1B5FD6"></rect></svg>';
  const kbd = (...ks) => ks.map((k) => `<kbd>${esc(k)}</kbd>`).join('');
  const RAIL = {
    overview: [[['j', 'k'], 'move'], [['↵'], 'open'], [['/'], 'search'], [['g', 'b'], 'browse'], [['g', 'p'], 'pieces'], [['y'], 'copy root']],
    browse: [[['j', 'k'], 'move'], [['↵'], 'open'], [['u'], 'up a level'], [['f'], 'filter'], [['s'], 'sort by'], [['r'], 'reverse'], [['o'], 'open in gateway'], [['y'], 'copy CID']],
    pin: [[['j', 'k'], 'move'], [['↵'], 'open'], [['u'], 'up a level'], [['o'], 'open in gateway'], [['y'], 'copy CID'], [['g', 'b'], 'browse']],
    pieces: [[['j', 'k'], 'move'], [['↵'], 'open'], [['f'], 'filter'], [['s'], 'sort by'], [['r'], 'reverse'], [['y'], 'copy PieceCID'], [['g', 'o'], 'overview']],
    search: [[['↓'], 'into results'], [['j', 'k'], 'move'], [['↵'], 'open'], [['1', '…', '9'], 'kind'], [['0'], 'all kinds'], [['esc'], 'clear'], [['y'], 'copy CID']]
  };
  const GROUPS = [
    ['Anywhere', [['Search', ['/']], ['This list', ['?']], ['Go to overview', ['g', 'o']], ['Go to browse', ['g', 'b']], ['Go to pieces', ['g', 'p']], ['Close or cancel', ['esc']]]],
    ['Lists', [['Next or previous row', ['j', 'k']], ['Same, with arrows', ['↓', '↑']], ['Open the row', ['↵']], ['Up a level', ['u', '⌫']], ['Filter the list', ['f']], ['Copy the CID', ['y']], ['Open in a gateway', ['o']], ['Sort by the next column', ['s']], ['Reverse the order', ['r']]]],
    ['Search', [['Move into results', ['↓']], ['Filter by kind', ['1', '…', '9']], ['Show all kinds', ['0']], ['Clear the query', ['esc']], ['Open the result', ['↵']]]]
  ];
  let statusEl;
  function frame() {
    const nav = [['overview', 'index.html', 'Overview'], ['browse', 'browse.html', 'Browse'], ['pieces', 'pieces.html', 'Pieces']]
      .map((n) => `<a href="${n[1]}"${n[0] === PAGE ? ' aria-current="page"' : ''}>${n[2]}</a>`).join('');
    const q = PAGE === 'search' ? esc(params.get('q') || '') : '';
    D.body.insertAdjacentHTML('afterbegin', `<header class="top"><div class="bar"><a class="brand" href="index.html">${LOGO}<span>ipfs2fil</span></a><nav class="pages" aria-label="Explorer">${nav}</nav><form class="search" action="search.html" role="search"><label class="sr" for="q">Search names and CIDs</label><input id="q" name="q" type="search" value="${q}" placeholder="Search names and CIDs" autocomplete="off" spellcheck="false"><kbd>/</kbd></form></div></header>`);
    const rail = (RAIL[PAGE] || []).map((r) => `<span>${kbd(...r[0])} ${r[1]}</span>`).join('');
    D.body.insertAdjacentHTML('beforeend', `<footer class="rail-wrap"><div class="rail">${rail}<button type="button" data-act="help">${kbd('?')} all shortcuts</button><span class="status"></span></div></footer>`);
    statusEl = D.querySelector('.rail .status');
    D.querySelector('[data-act=help]').onclick = () => help(true);
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
    const typing = t.matches && t.matches('input, select, textarea');
    if (k === 'Escape') {
      if (D.getElementById('help')) { help(false); ev.preventDefault(); return; }
      if (typing) {
        if (t.value && t.type === 'search') { t.value = ''; t.dispatchEvent(new Event('input')); } else t.blur();
        ev.preventDefault();
      }
      return;
    }
    if (typing) {
      if (t.type === 'search' && (k === 'ArrowDown' || k === 'Enter') && active && pageKeys.fromBox) { if (pageKeys.fromBox(k, t)) ev.preventDefault(); }
      return;
    }
    if (D.getElementById('help') && k !== '?') return;
    if (pendingG && Date.now() - pendingG < 1500) {
      pendingG = 0;
      const to = { o: 'index.html', b: 'browse.html', p: 'pieces.html', s: 'search.html' }[k];
      if (to) { location.href = to; ev.preventDefault(); }
      return;
    }
    pendingG = 0;
    let done = true;
    if (k === '/') { const q = D.getElementById('q'); q.focus(); q.select(); }
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
  const cidCol = { key: 'cid', label: 'CID', cls: 'cid hide-sm', html: (e) => e.cid ? `${esc(shortCID(e.cid))}<a href="${esc(gw(e.cid))}" target="_blank" rel="noopener" title="Open in an IPFS gateway" aria-label="Open ${esc(e.name)} in an IPFS gateway">↗</a>` : '' };
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
    let out = `<a href="index.html">archive</a><span aria-hidden="true">/</span>` + (parts.length ? `<a href="${browseHref('')}">data</a>` : '<span aria-current="page">data</span>');
    let start = 0;
    for (let i = 0; i < parts.length; i++) {
      const sub = parts.slice(0, i + 1).join('/'), r = rootAt(sub);
      if (r && r.kind !== 'mfs') start = i;
    }
    for (let i = 0; i < parts.length; i++) {
      const sub = parts.slice(0, i + 1).join('/'), r = rootAt(sub);
      const label = r && r.kind !== 'mfs' && i === start ? rootLabel(r) : parts[i];
      const shown = label.length > 40 ? shortCID(label) : label;
      out += '<span aria-hidden="true">/</span>' + (i === parts.length - 1 ? `<span aria-current="page">${esc(shown)}</span>` : `<a href="${esc(r && r.kind !== 'mfs' ? 'pin.html?cid=' + encodeURIComponent(r.cid) : browseHref(sub))}">${esc(shown)}</a>`);
    }
    return `<nav class="crumbs" aria-label="Path">${out}</nav>`;
  }
  const main = () => D.querySelector('main');
  function fail(err) {
    main().innerHTML = `<div class="err"><h2>Something is missing</h2><p class="note">${esc(err.message || err)}</p><p class="note">If you opened this from disk, open explorer/index.html inside the archive, or run <code>ipfs2fil explore --serve</code>.</p></div>`;
  }

  // ---- Overview
  async function overview() {
    const [m, meta] = await Promise.all([manifest(), load('meta')]);
    const roots = m.roots || [], pieces = m.pieces || [], copies = m.copies || [];
    const sum = (f) => roots.filter(f).reduce((a, r) => a + r.bytes, 0);
    const isPin = (r) => r.kind === 'pin-recursive' || r.kind === 'pin-direct';
    const pins = roots.filter(isPin), rec = pins.filter((r) => r.kind === 'pin-recursive').length;
    const orphans = roots.filter((r) => r.kind === 'orphan');
    const parts = meta.parts || {};
    const indexBytes = pieces.filter((p, i) => (meta.pieces[i] || {}).where === 'index').reduce((a, p) => a + p.carBytes, 0);
    const snaps = m.snapshots || [], last = snaps[snaps.length - 1];
    const total = m.source.bytes;
    const stored = copies.length ? `stored on Filecoin in ${plural(copies.length, 'copy', 'copies')}` : 'not on Filecoin yet';
    pageKeys.cid = () => m.root;
    const types = KINDS.filter((k) => meta.types[k]).map((k) => [k, meta.types[k][1], KIND[k][1], KIND[k][3]]).sort((a, b) => b[1] - a[1]);
    const partBars = [['pins', sum(isPin)], ['mfs', sum((r) => r.kind === 'mfs')], ['orphans', sum((r) => r.kind === 'orphan')], ['index', indexBytes]].map((p) => [PART[p[0]][0], p[1], PART[p[0]][1]]);
    main().innerHTML = `
<section class="head">
  <div class="eyebrow">Archive of a kubo repo · fs-repo ${esc(m.source.repoVersion)}</div>
  <h1>${bytes(total)}, ${plural(pins.length, 'pin')}, ${stored}</h1>
  <div class="meta">
    <span>Archive root <code class="chipcode" title="${esc(m.root)}">${esc(shortCID(m.root))}</code> <button type="button" class="link" data-copy="${esc(m.root)}">Copy</button></span>
    <span>Data root <code class="mono" title="${esc(m.dataRoot)}">${esc(shortCID(m.dataRoot))}</code> <button type="button" class="link" data-copy="${esc(m.dataRoot)}">Copy</button></span>
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
    <div class="legend">${types.map((t) => `<a href="search.html?kind=${t[0]}"><span class="sw" style="background:${t[2]}"></span>${t[3]} ${bytes(t[1])}</a>`).join('')}</div></div>
</section>
<div class="split">
  <section class="card wide"><div class="titled"><h2>Largest roots</h2><a href="${browseHref('')}">Browse all</a></div><div id="roots"></div></section>
  <section class="card side" style="gap:12px"><div class="titled"><h2>Pieces</h2><a href="pieces.html">All ${num(pieces.length)}</a></div>
    <div class="toggle" role="group" aria-label="Color pieces by"><button type="button" class="pill" data-by="part" aria-pressed="true">Part of repo</button><button type="button" class="pill" data-by="type" aria-pressed="false">File type</button></div>
    <div class="pmap" role="img" id="pmap"></div>
    <div class="faint" id="pnote"></div></section>
</div>`;
    function stat(k, v, s) { return `<div class="card tight stat"><div class="k">${k}</div><div class="v">${v}</div><div class="s">${s}</div></div>`; }
    D.querySelectorAll('[data-copy]').forEach((b) => { b.onclick = () => copy(b.dataset.copy); });
    const largest = roots.slice().sort((a, b) => b.bytes - a.bytes).slice(0, 10).map((r) => ({ ...r, name: rootLabel(r), path: r.path, dir: '', piece: r.firstPiece, kind: kindAt(r), root: r }));
    function kindAt(r) { return r.kind === 'mfs' ? 'folder' : 'other'; }
    const where = (r) => ({ 'pin-recursive': 'pin', 'pin-direct': 'direct pin', mfs: 'MFS', orphan: 'orphan' }[r.root.kind] + ' · ' + shortCID(r.cid));
    const list = new List(D.getElementById('roots'), {
      label: 'Largest roots', step: 10, sortSelect: false, sort0: null, filter: null,
      cols: [nameCol(where), { key: 'pieces', label: 'Pieces', cls: 'r', html: (r) => num(r.root.lastPiece - r.root.firstPiece + 1) }, sizeCol],
      href: entryHref, cid: (r) => r.cid
    });
    list.setItems(largest);
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

  // ---- Browse a directory
  async function browse() {
    const p = params.get('p') || '';
    const [m, info] = await Promise.all([manifest(), dirInfo(p)]);
    if (!info) throw new Error(`There is no folder data/${p} in this archive.`);
    const r = rootAt(p);
    const title = p ? (r && r.kind !== 'mfs' ? rootLabel(r) : p.split('/').pop()) : 'data';
    const kindLabel = r ? ROOTKIND[r.kind] : p === '' ? 'Everything in the archive' : whereOf(p) === 'mfs' ? 'MFS folder' : 'Folder';
    const cid = info.cid;
    const up = () => { if (p) location.href = browseHref(p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : ''); return !!p; };
    pageKeys.u = pageKeys.Backspace = up;
    pageKeys.cid = () => cid;
    const typeRows = KINDS.filter((k) => k !== 'folder' && info.types[k]).map((k) => [k, info.types[k]]).sort((a, b) => b[1][1] - a[1][1]);
    main().innerHTML = `
${crumbs(p)}
<div class="titlebar"><div class="head" style="gap:4px"><h1>${esc(title)}</h1><div class="muted" style="font-size:14px">${plural(info.n, 'entry', 'entries')} · ${bytes(info.bytes)} · ${esc(kindLabel)}</div></div>
${cid ? `<div class="actions"><a class="btn" href="${esc(gw(cid))}" target="_blank" rel="noopener">Open in gateway ↗ ${kbd('o')}</a><a class="btn" href="${esc(gw(cid) + '?format=car')}" target="_blank" rel="noopener">Download as CAR</a></div>` : ''}</div>
<div class="split">
  <section class="card wide" id="list"></section>
  <aside class="side">
    <section class="card"><h2>This folder</h2><div class="facts">
      ${cid ? `<span>CID</span><span><code>${esc(cid)}</code>${gwLinks(cid)}</span>` : ''}
      <span>Kind</span><span>${esc(kindLabel)}</span>
      <span>Files</span><span>${num(info.files)}</span>
      <span>Folders</span><span>${num(info.folders)}</span>
      ${info.lo >= 0 ? `<span>Spread over</span><a href="pieces.html?n=${info.lo}">${info.lo === info.hi ? 'piece ' + info.lo : 'pieces ' + info.lo + ' to ' + info.hi}</a>` : ''}
      ${r && r.kind !== 'mfs' ? `<span>Pin</span><a href="pin.html?cid=${encodeURIComponent(r.cid)}">Details and pieces</a>` : ''}
    </div></section>
    ${typeRows.length ? `<section class="card"><h2>By file type</h2>${bar(typeRows.map((t) => [KIND[t[0]][3], t[1][1], KIND[t[0]][1]]), typeRows.map((t) => KIND[t[0]][3] + ' ' + bytes(t[1][1])).join(', '))}
      <div>${typeRows.map((t) => `<button type="button" class="typerow" data-kind="${t[0]}"><span class="sw" style="background:${KIND[t[0]][1]}"></span><span>${KIND[t[0]][3]}</span><span class="n">${num(t[1][0])} · ${bytes(t[1][1])}</span></button>`).join('')}</div>
      <span class="faint">Click a type to filter the list.</span></section>` : ''}
  </aside>
</div>`;
    const list = new List(D.getElementById('list'), {
      label: 'Folder entries', step: 200, sort0: 'name', group: folderFirst, filter: 'Filter this folder',
      kinds: kindChips(), kindOf: (e) => e.kind, text: (e) => e.name + ' ' + e.cid,
      selects: [{ key: 'size', label: 'Size', options: SIZES, test: sizeTest }],
      cols: [nameCol(), kindCol, { ...sizeCol, html: (e) => e.kind === 'folder' && !e.bytes ? '' : bytes(e.bytes) }, cidCol, pieceCol],
      href: entryHref, cid: (e) => e.cid, empty: 'This folder is empty.'
    });
    D.querySelectorAll('[data-kind]').forEach((b) => { b.onclick = () => list.set({ kind: b.dataset.kind }); });
    list.setItems(await listDir(info));
    status(showPath(p) || 'data');
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
    D.querySelectorAll('[data-copy]').forEach((b) => { b.onclick = () => copy(b.dataset.copy); });
    const rows = [];
    for (let i = r.firstPiece; i <= r.lastPiece && i < (m.pieces || []).length; i++) rows.push(m.pieces[i]);
    const list = new List(D.getElementById('list'), {
      label: 'Pieces of this root', step: 100, sort0: 'n', filter: null, sortSelect: false,
      cols: [{ key: 'n', label: 'Piece', sort: (p) => p.index, html: (p) => `<a href="pieces.html?n=${p.index}">#${p.index}</a>` },
        { key: 'cid', label: 'PieceCID', cls: 'cid', html: (p) => esc(shortCID(p.pieceCid)) },
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
        { key: 'cid', label: 'PieceCID', cls: 'cid', html: (p) => `<span title="${esc(p.pieceCid)}">${esc(shortCID(p.pieceCid))}</span>` },
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

  // ---- Search
  async function search() {
    const [m, meta] = await Promise.all([manifest(), load('meta')]);
    const box = D.getElementById('q');
    D.querySelector('form.search').onsubmit = (ev) => ev.preventDefault();
    main().innerHTML = `<div class="head" style="gap:6px"><h1 id="title"></h1><span class="faint">Results update as you type, from the name index inside this archive. Nothing leaves your browser. Paste a CID to jump to it.</span></div><section class="card" id="list"></section>`;
    const kinds = kindChips();
    const list = new List(D.getElementById('list'), {
      label: 'Results', step: 100, sort0: 'name', filter: null, digits: true,
      kinds, kindOf: (e) => e.kind, text: (e) => e.name + ' ' + e.cid,
      selects: [{ key: 'where', label: 'Where', value: params.get('where') || 'any', options: [['any', 'Anywhere'], ['pins', 'Pins'], ['mfs', 'MFS'], ['orphans', 'Orphans']], test: (e, v) => whereOf(e.path) === v },
        { key: 'size', label: 'Size', options: SIZES, test: sizeTest }],
      cols: [nameCol(), kindCol, sizeCol, { key: 'path', label: 'Where', cls: 'muted', sort: (e) => e.dir, html: (e) => `<a class="muted" href="${esc(browseHref(e.dir))}">${esc(showPath(e.dir) || 'data')}/</a>` }, pieceCol],
      href: entryHref, cid: (e) => e.cid, empty: 'No names match.', onChange: title
    });
    list.s.q = box.value;
    if (params.get('kind')) list.s.kind = params.get('kind');
    function title() {
      const q = list.s.q.trim();
      D.getElementById('title').textContent = q ? `${num(list.shown.length)}${list.loading ? '+' : ''} ${list.shown.length === 1 ? 'match' : 'matches'} for “${q}”` : `Search ${num(meta.entries)} names`;
      const u = new URL(location.href);
      u.searchParams.set('q', q);
      history.replaceState(null, '', u.search ? u : location.pathname);
    }
    box.oninput = () => list.set({ q: box.value });
    pageKeys.fromBox = (k) => {
      if (k === 'Enter') {
        const q = box.value.trim(), r = (m.roots || []).find((x) => x.cid === q);
        if (r) { location.href = 'pin.html?cid=' + encodeURIComponent(r.cid); return true; }
        if (list.shown.length === 1) { location.href = entryHref(list.shown[0]); return true; }
      }
      box.blur(); list.el.querySelector('.scroll').focus(); active = list; list.mark();
      return true;
    };
    if (!params.has('q') && !params.has('kind')) box.focus();
    // Stream every shard; results show as they arrive.
    const items = [];
    let next = 0, last = 0;
    const work = async () => {
      while (next < meta.shards) {
        const s = await load('s/' + next++);
        for (const r of s.rows) items.push(entryOf(s, r));
        if (Date.now() - last > 250) { last = Date.now(); list.setItems(items.slice(), true); title(); }
      }
    };
    await Promise.all([work(), work(), work(), work()]);
    list.setItems(items, false);
    title();
    status(num(items.length) + ' names');
  }

  // ---- Start
  frame();
  D.querySelector('footer').before(Object.assign(D.createElement('main'), { innerHTML: '<p class="muted">Loading…</p>' }));
  const pages = { overview, browse, pin, pieces: piecesPage, search };
  manifest().then((m) => {
    ROOTS = {};
    (m.roots || []).forEach((r) => { ROOTS[r.path] = r; });
    return load('meta').then((meta) => {
      if (meta.format > 1) throw new Error('This archive was made by a newer ipfs2fil; its index format is ' + meta.format + '.');
      return (pages[PAGE] || overview)();
    });
  }).catch(fail);
})();
