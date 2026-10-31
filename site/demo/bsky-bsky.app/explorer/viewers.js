// ipfs2fil explorer: viewers and in-browser thumbnails for the Files page.
// Plain JavaScript, no libraries. Each viewer shows one sort of file in the
// preview pane; to show another sort, add one with view({...}) below. Photos,
// videos, sound, web pages, PDFs and EPUBs are shown by explorer.js itself;
// explore makes most thumbnails (internal/explorer/thumbnails.go) and the
// ones it cannot are made here. docs/viewers.md says how it fits together.
//
// A viewer is { name, test(e, x), show(st, v) }: test says if it takes the
// entry e (x is its lower-case extension), and show fills the stage element
// st. v holds what a viewer needs:
//
//   v.e, v.x, v.url   the entry, its extension, where its bytes are
//   v.head(n)         the first n bytes, as an ArrayBuffer
//   v.all(max)        all of it (an error when over max bytes)
//   v.tail(n)         the last n bytes, and the file's size
//   v.range(a, b)     bytes a to b-1
//   v.still()         whether the entry is still the one shown
//   v.base            where the file's folder is served, for relative links, or ''
//   v.esc, v.icon, v.bytes, v.num   explorer.js's formatting helpers
'use strict';
(() => {
  const D = document;
  const viewers = [];
  const view = (v) => viewers.push(v);
  const set = (s) => new Set(s.split(' '));
  const dec = (b) => new TextDecoder().decode(b);
  const MB = 1 << 20;
  let H; // explorer.js's helpers, set by use()

  // ---- Zip files, for Office documents, EPUBs and zip archives: the
  // directory, and a function that reads one entry.
  function unzip(buf) {
    const v = new DataView(buf), files = new Map();
    let end = -1;
    for (let i = buf.byteLength - 22; i >= Math.max(0, buf.byteLength - 65557); i--) if (v.getUint32(i, true) === 0x06054b50) { end = i; break; }
    if (end < 0) throw new Error('This file is not a zip file.');
    let p = v.getUint32(end + 16, true);
    for (let n = v.getUint16(end + 10, true); n > 0 && p + 46 <= buf.byteLength && v.getUint32(p, true) === 0x02014b50; n--) {
      const nl = v.getUint16(p + 28, true);
      files.set(dec(new Uint8Array(buf, p + 46, nl)), [v.getUint16(p + 10, true), v.getUint32(p + 20, true), v.getUint32(p + 42, true), v.getUint32(p + 24, true)]);
      p += 46 + nl + v.getUint16(p + 30, true) + v.getUint16(p + 32, true);
    }
    const get = async (name) => {
      const f = files.get(name);
      if (!f) return null;
      const [method, size, off] = f, start = off + 30 + v.getUint16(off + 26, true) + v.getUint16(off + 28, true);
      const raw = new Uint8Array(buf, start, Math.min(size, buf.byteLength - start));
      if (method === 0) return raw;
      if (method !== 8 || !('DecompressionStream' in window)) return null;
      return new Uint8Array(await new Response(new Blob([raw]).stream().pipeThrough(new DecompressionStream('deflate-raw'))).arrayBuffer());
    };
    get.files = files;
    return get;
  }
  const xml = (b) => b ? new DOMParser().parseFromString(dec(b), 'application/xml') : null;
  const tags = (d, n) => d ? Array.from(d.getElementsByTagNameNS('*', n)) : [];

  const note = (st, msg) => { st.innerHTML = `<div class="none">${H.icon('doc')}<p>${H.esc(msg)}</p></div>`; };
  const table = (rows, head) => `<div class="vtable"><table>${head ? `<thead><tr>${head.map((c) => `<th>${H.esc(c)}</th>`).join('')}</tr></thead>` : ''}<tbody>${rows.map((r) => `<tr>${r.map((c) => `<td>${H.esc(c)}</td>`).join('')}</tr>`).join('')}</tbody></table></div>`;
  const codeHTML = (text) => `<pre class="code"><code>${text.split('\n').slice(0, 5000).map((l) => `<span>${H.esc(l)}</span>`).join('\n')}</code></pre>`;

  // ---- Markdown, without raw HTML: everything is escaped first.
  function md(src, base) {
    const out = [], lines = src.replace(/\r\n?/g, '\n').split('\n');
    const link = (u) => {
      u = u.trim();
      if (/^(https?:|mailto:|#)/i.test(u)) return u;
      if (/^[a-z][a-z0-9+.-]*:/i.test(u)) return '#';
      return base ? base + u : u;
    };
    const inline = (s) => {
      const codes = [];
      s = s.replace(/`([^`]+)`/g, (_, c) => { codes.push(c); return '\u0000' + (codes.length - 1) + '\u0000'; });
      s = H.esc(s)
        .replace(/!\[([^\]]*)\]\(([^)\s]+)(?:\s+&quot;[^&]*&quot;)?\)/g, (_, a, u) => `<img alt="${a}" src="${H.esc(link(u.replace(/&amp;/g, '&')))}">`)
        .replace(/\[([^\]]+)\]\(([^)\s]+)(?:\s+&quot;[^&]*&quot;)?\)/g, (_, t, u) => `<a href="${H.esc(link(u.replace(/&amp;/g, '&')))}" target="_blank" rel="noopener">${t}</a>`)
        .replace(/&lt;(https?:\/\/[^\s&]+)&gt;/g, '<a href="$1" target="_blank" rel="noopener">$1</a>')
        .replace(/\*\*([^*]+)\*\*|__([^_]+)__/g, (_, a, b) => `<strong>${a || b}</strong>`)
        .replace(/(^|[^*\w])\*([^*\s][^*]*)\*/g, '$1<em>$2</em>').replace(/(^|\W)_([^_\s][^_]*)_(?=\W|$)/g, '$1<em>$2</em>')
        .replace(/~~([^~]+)~~/g, '<del>$1</del>');
      return s.replace(/\u0000(\d+)\u0000/g, (_, i) => `<code>${H.esc(codes[+i])}</code>`);
    };
    let i = 0;
    const isBlockStart = (l) => /^(#{1,6}\s|```|~~~|>|\s*([-*+]|\d+[.)])\s|\s*(\*\s*){3,}$|\s*(-\s*){3,}$|\|)/.test(l);
    while (i < lines.length) {
      const l = lines[i];
      let m;
      if (!l.trim()) { i++; continue; }
      if ((m = l.match(/^(```|~~~)\s*(\S*)/))) {
        const fence = m[1], body = [];
        for (i++; i < lines.length && !lines[i].startsWith(fence); i++) body.push(lines[i]);
        i++;
        out.push(`<pre class="code"><code>${H.esc(body.join('\n'))}</code></pre>`);
      } else if ((m = l.match(/^(#{1,6})\s+(.*?)\s*#*\s*$/))) {
        out.push(`<h${m[1].length}>${inline(m[2])}</h${m[1].length}>`); i++;
      } else if (/^\s*((\*\s*){3,}|(-\s*){3,}|(_\s*){3,})$/.test(l)) {
        out.push('<hr>'); i++;
      } else if (/^>/.test(l)) {
        const body = [];
        for (; i < lines.length && /^>/.test(lines[i]); i++) body.push(lines[i].replace(/^>\s?/, ''));
        out.push(`<blockquote>${md(body.join('\n'), base)}</blockquote>`);
      } else if (/^\s*([-*+]|\d+[.)])\s/.test(l)) {
        const ordered = /^\s*\d/.test(l), items = [];
        for (; i < lines.length && (/^\s*([-*+]|\d+[.)])\s/.test(lines[i]) || (/^\s{2,}\S/.test(lines[i]) && items.length)); i++) {
          if (/^\s*([-*+]|\d+[.)])\s/.test(lines[i]) && !/^\s{2,}/.test(lines[i])) items.push([lines[i].replace(/^\s*([-*+]|\d+[.)])\s+/, '')]);
          else items[items.length - 1].push(lines[i].replace(/^\s{2,4}/, ''));
        }
        const li = (it) => {
          const t = it.join('\n'), box = t.match(/^\[([ xX])\]\s+/);
          const body = it.length > 1 ? md(t.replace(/^\[[ xX]\]\s+/, ''), base) : inline(t.replace(/^\[[ xX]\]\s+/, ''));
          return `<li>${box ? `<input type="checkbox" disabled${box[1] !== ' ' ? ' checked' : ''}> ` : ''}${body}</li>`;
        };
        out.push(`<${ordered ? 'ol' : 'ul'}>${items.map(li).join('')}</${ordered ? 'ol' : 'ul'}>`);
      } else if (/^\|/.test(l) && i + 1 < lines.length && /^\|?\s*:?-+/.test(lines[i + 1])) {
        const cells = (r) => r.replace(/^\||\|\s*$/g, '').split('|').map((c) => inline(c.trim()));
        const head = cells(l), rows = [];
        for (i += 2; i < lines.length && /^\|/.test(lines[i]); i++) rows.push(cells(lines[i]));
        out.push(`<table><thead><tr>${head.map((c) => `<th>${c}</th>`).join('')}</tr></thead><tbody>${rows.map((r) => `<tr>${r.map((c) => `<td>${c}</td>`).join('')}</tr>`).join('')}</tbody></table>`);
      } else {
        const body = [l];
        for (i++; i < lines.length && lines[i].trim() && !isBlockStart(lines[i]); i++) body.push(lines[i]);
        if (i < lines.length && /^(=+|-+)\s*$/.test(lines[i]) && body.length === 1) { out.push(`<h${lines[i][0] === '=' ? 1 : 2}>${inline(body[0])}</h${lines[i][0] === '=' ? 1 : 2}>`); i++; continue; }
        out.push(`<p>${inline(body.join('\n')).replace(/ {2,}\n|\\\n/g, '<br>')}</p>`);
      }
    }
    return out.join('\n');
  }
  // A rendered view with a switch to the source.
  function twoWay(st, v, html, text) {
    st.innerHTML = `<div class="vbar"><button type="button" class="pill" data-vw="r" aria-pressed="true">Rendered</button><button type="button" class="pill" data-vw="s" aria-pressed="false">Source</button></div><div class="vbody"></div>`;
    const body = st.querySelector('.vbody');
    const show = (r) => {
      st.querySelectorAll('[data-vw]').forEach((b) => b.setAttribute('aria-pressed', (b.dataset.vw === 'r') === r));
      body.className = 'vbody' + (r ? ' doc' : '');
      body.innerHTML = r ? html : codeHTML(text);
    };
    st.querySelectorAll('[data-vw]').forEach((b) => { b.onclick = () => show(b.dataset.vw === 'r'); });
    show(true);
  }

  view({
    name: 'markdown', test: (e, x) => set('md markdown mdown mkd').has(x),
    async show(st, v) {
      const text = dec(await v.head(4 * MB));
      if (v.still()) twoWay(st, v, md(text, v.base), text);
    }
  });

  // ---- Tables: CSV and TSV.
  function csv(text, sep) {
    const rows = [];
    let row = [], cell = '', q = false;
    for (let i = 0; i < text.length && rows.length < 2000; i++) {
      const c = text[i];
      if (q) {
        if (c === '"') { if (text[i + 1] === '"') { cell += '"'; i++; } else q = false; } else cell += c;
      } else if (c === '"' && !cell) q = true;
      else if (c === sep) { row.push(cell); cell = ''; }
      else if (c === '\n' || c === '\r') { if (c === '\r' && text[i + 1] === '\n') i++; row.push(cell); rows.push(row); row = []; cell = ''; }
      else cell += c;
    }
    if (cell || row.length) { row.push(cell); rows.push(row); }
    return rows;
  }
  view({
    name: 'table', test: (e, x) => x === 'csv' || x === 'tsv',
    async show(st, v) {
      const buf = await v.head(8 * MB), text = dec(buf);
      if (!v.still()) return;
      const rows = csv(text, v.x === 'tsv' ? '\t' : text.split('\n')[0].split(';').length > text.split('\n')[0].split(',').length ? ';' : ',');
      const cut = rows.length >= 2000 || v.e.bytes > buf.byteLength;
      twoWay(st, v, table(rows.slice(1), rows[0]) + (cut ? `<p class="faint pad">The first ${v.num(rows.length)} rows. Download for all of it.</p>` : ''), text.slice(0, 256 << 10));
    }
  });

  // ---- Syntax highlighting: one small tokenizer per family of languages,
  // enough to tell comments, strings, numbers, keywords and calls apart.
  const LANG = {};
  const langs = (fam, exts) => exts.split(' ').forEach((x) => { LANG[x] = fam; });
  langs('c', 'js mjs cjs jsx ts tsx go rs c h cc cpp hpp cs swift kt kts java scala dart php css scss less proto zig groovy');
  langs('hash', 'py rb sh bash zsh fish yaml yml toml pl r ps1 conf ini cfg env dockerfile makefile mk nix tf');
  langs('dash', 'sql lua hs elm');
  langs('json', 'json geojson webmanifest jsonl ndjson ipynb');
  langs('xml', 'xml html htm xhtml svg plist vue');
  const KW = new Set(('abstract and as async await break case catch class const continue def defer del do elif else enum export extends ' +
    'extern false final finally fn for from func function go goto if impl import in instanceof interface is lambda let loop match mod module ' +
    'mut new nil none not null of or package pass private protected pub public raise return select self static struct super switch then this ' +
    'throw throws trait true try type typeof undefined unless until use var void while with yield begin end local elseif require echo fi esac ' +
    'done select from where insert update delete into values create table join left right inner outer on group by order having limit ' +
    'True False None chan map range string int bool float').split(' '));
  const STR = '"(?:\\\\.|[^"\\\\\\n])*"|\'(?:\\\\.|[^\'\\\\\\n])*\'';
  const NUM = '\\b(?:0x[0-9a-fA-F_]+|\\d[\\d_]*(?:\\.\\d+)?(?:[eE][+-]?\\d+)?)\\b';
  const ID = '[A-Za-z_$][\\w$]*';
  const RX = {
    c: new RegExp(`(\\/\\/[^\\n]*|\\/\\*[\\s\\S]*?\\*\\/)|(${STR}|\`(?:\\\\.|[^\`\\\\])*\`)|(${NUM})|(${ID})`, 'g'),
    hash: new RegExp(`(#[^\\n]*)|("""[\\s\\S]*?"""|\'\'\'[\\s\\S]*?\'\'\'|${STR})|(${NUM})|(${ID})`, 'g'),
    dash: new RegExp(`(--[^\\n]*|\\/\\*[\\s\\S]*?\\*\\/)|(${STR})|(${NUM})|(${ID})`, 'g'),
    json: new RegExp(`()(${STR})|(-?${NUM})|(${ID})`, 'g'),
    xml: /(<!--[\s\S]*?-->)|("[^"]*"|'[^']*')|(<\/?[\w:.-]+|\/?>)|([\w:.-]+(?==))/g
  };
  // hl returns the lines of text as HTML, each in its own span (for line
  // numbers), a token cut across lines closed and reopened on each.
  function hl(text, fam) {
    const rx = RX[fam], out = [];
    const put = (cls, t) => { const parts = t.split('\n'); parts.forEach((p, i) => { if (i) out.push('\n'); if (p) out.push(cls ? `<i class="t-${cls}">${H.esc(p)}</i>` : H.esc(p)); }); };
    if (!rx) put('', text);
    else {
      let at = 0, m;
      rx.lastIndex = 0;
      while ((m = rx.exec(text))) {
        if (m[0] === '') { rx.lastIndex++; continue; }
        put('', text.slice(at, m.index));
        let cls = '';
        if (m[1]) cls = 'com';
        else if (m[2]) cls = fam === 'json' && /^\s*:/.test(text.slice(rx.lastIndex, rx.lastIndex + 40)) ? 'key' : 'str';
        else if (m[3]) cls = fam === 'xml' ? 'tag' : 'num';
        else if (m[4]) cls = fam === 'xml' ? 'attr' : KW.has(m[4]) ? 'kw' : text[rx.lastIndex] === '(' ? 'fn' : '';
        put(cls, m[0]);
        at = rx.lastIndex;
      }
      put('', text.slice(at));
    }
    return out.join('').split('\n');
  }
  const famOf = (x) => LANG[x] || '';
  // A code view with its options: highlighted or plain (pretty or the exact
  // bytes for JSON), lines wrapped or not. The choices are kept.
  const pref = (k, d) => { try { return localStorage.getItem('i2f.' + k) || d; } catch (e) { return d; } };
  const keep = (k, v) => { try { localStorage.setItem('i2f.' + k, v); } catch (e) { /* private mode */ } };
  function codeView(st, v, text, opts) {
    const modes = opts.modes, key = 'code.' + opts.kind;
    let mode = pref(key, modes[0][0]);
    if (!modes.some((m) => m[0] === mode)) mode = modes[0][0];
    let wrap = pref('code.wrap', '0') === '1';
    st.innerHTML = `<div class="vbar">${modes.length > 1 ? modes.map((m) => `<button type="button" class="pill" data-m="${m[0]}">${m[1]}</button>`).join('') : ''}<button type="button" class="pill" data-wrap>Wrap</button><button type="button" class="pill" data-copy-text>Copy</button>${opts.extra || ''}<span class="faint">${H.esc(opts.note || '')}</span></div><div class="vbody"></div>`;
    const body = st.querySelector('.vbody');
    const draw = () => {
      st.querySelectorAll('[data-m]').forEach((b) => b.setAttribute('aria-pressed', b.dataset.m === mode));
      st.querySelector('[data-wrap]').setAttribute('aria-pressed', wrap);
      const m = modes.find((x) => x[0] === mode), t = m[2](text), fam = m[3];
      const lines = fam ? hl(t.length > 2 * MB ? t.slice(0, 2 * MB) : t, fam) : t.split('\n').map(H.esc);
      body.innerHTML = `<pre class="code${wrap ? ' wrap' : ''}"><code>${lines.slice(0, 20000).map((l) => `<span>${l}</span>`).join('\n')}</code></pre>${lines.length > 20000 || opts.cut ? '<p class="faint pad">Showing the start. Download for all of it.</p>' : ''}`;
    };
    st.querySelectorAll('[data-m]').forEach((b) => { b.onclick = () => { mode = b.dataset.m; keep(key, mode); draw(); }; });
    st.querySelector('[data-wrap]').onclick = () => { wrap = !wrap; keep('code.wrap', wrap ? '1' : '0'); draw(); };
    st.querySelector('[data-copy-text]').onclick = async (ev) => {
      const m = modes.find((x) => x[0] === mode);
      try { await navigator.clipboard.writeText(m[2](text)); ev.target.textContent = 'Copied'; } catch (err) { ev.target.textContent = 'Could not copy'; }
      setTimeout(() => { ev.target.textContent = 'Copy'; }, 1200);
    };
    draw();
  }

  // ---- JSON: pretty printed and highlighted, or exactly as stored.
  // Bluesky records come as JSON too; a post links to its place in Posts.
  view({
    name: 'json', test: (e, x) => set('json geojson webmanifest').has(x),
    async show(st, v) {
      const buf = await v.head(8 * MB);
      if (!v.still()) return;
      const text = dec(buf), cut = v.e.bytes > buf.byteLength;
      let parsed;
      try { if (!cut) parsed = JSON.parse(text); } catch (err) { /* not JSON after all */ }
      const pretty = (t) => parsed === undefined ? t : JSON.stringify(parsed, null, 2);
      const type = parsed && typeof parsed === 'object' && typeof parsed.$type === 'string' ? parsed.$type : '';
      const post = type && H.recordHref ? H.recordHref(v.e, parsed) : '';
      codeView(st, v, text, {
        kind: 'json', cut,
        modes: [['pretty', 'Pretty', pretty, 'json'], ['raw', 'Exact', (t) => t, 'json'], ['plain', 'Plain', pretty, '']],
        note: type ? 'Bluesky record · ' + type : parsed === undefined && !cut ? 'Not valid JSON' : '',
        extra: post ? `<a class="pill" href="${H.esc(post)}">Show as a post</a>` : ''
      });
    }
  });
  view({
    name: 'notebook', test: (e, x) => x === 'ipynb',
    async show(st, v) {
      const nb = JSON.parse(dec(await v.all(32 * MB)));
      if (!v.still()) return;
      const src = (s) => Array.isArray(s) ? s.join('') : (s || '');
      const lang = ((nb.metadata || {}).kernelspec || {}).language || 'python';
      const out = (o) => {
        const d = o.data || {};
        if (d['image/png']) return `<img alt="" src="data:image/png;base64,${H.esc(src(d['image/png']).replace(/\s/g, ''))}">`;
        if (d['image/jpeg']) return `<img alt="" src="data:image/jpeg;base64,${H.esc(src(d['image/jpeg']).replace(/\s/g, ''))}">`;
        const t = o.text || d['text/plain'] || (o.traceback ? o.traceback.join('\n').replace(/\u001b\[[0-9;]*m/g, '') : '');
        return t ? `<pre class="code out"><code>${H.esc(src(t))}</code></pre>` : '';
      };
      const cells = (nb.cells || (nb.worksheets || [{}])[0].cells || []).map((c) => c.cell_type === 'markdown'
        ? `<div class="cell md">${md(src(c.source), v.base)}</div>`
        : c.cell_type === 'code' ? `<div class="cell"><pre class="code"><code>${H.esc(src(c.source || c.input))}</code></pre>${(c.outputs || []).map(out).join('')}</div>` : '');
      twoWay(st, v, `<p class="faint">Notebook · ${H.esc(lang)} · ${v.num(cells.length)} cells</p>` + cells.join(''), JSON.stringify(nb, null, 1).slice(0, 256 << 10));
    }
  });

  // ---- Fonts: a specimen.
  let fontN = 0;
  view({
    name: 'font', test: (e, x) => set('ttf otf woff woff2').has(x),
    async show(st, v) {
      const buf = await v.all(32 * MB);
      if (!v.still()) return;
      const fam = 'i2f-font-' + (++fontN);
      try {
        const f = new FontFace(fam, buf);
        await f.load();
        D.fonts.add(f);
      } catch (err) { return note(st, 'The browser cannot read this font.'); }
      const s = (px, t) => `<p style="font:${px}px/1.25 '${fam}';margin:0 0 12px">${t}</p>`;
      st.innerHTML = `<div class="vbody doc specimen">${s(44, 'Aa Bb Cc 0123')}${s(28, 'The quick brown fox jumps over the lazy dog')}${s(18, 'ABCDEFGHIJKLMNOPQRSTUVWXYZ<br>abcdefghijklmnopqrstuvwxyz<br>0123456789 !?&amp;@#%(){}[]')}${s(14, 'Sphinx of black quartz, judge my vow. Pack my box with five dozen liquor jugs.')}</div>`;
    }
  });

  // ---- Archives: what is in a zip (read from its directory at the end, so
  // only the end is fetched) or a tar (read as it streams in).
  async function zipList(v) {
    const [tail, size] = await v.tail(66000);
    const t = new DataView(tail);
    let end = -1;
    for (let i = tail.byteLength - 22; i >= 0; i--) if (t.getUint32(i, true) === 0x06054b50) { end = i; break; }
    if (end < 0) throw new Error('This file is not a zip file the viewer can read.');
    const n = t.getUint16(end + 10, true), cdSize = t.getUint32(end + 12, true), cdOff = t.getUint32(end + 16, true);
    const tailStart = size - tail.byteLength;
    const cd = cdOff >= tailStart ? tail.slice(cdOff - tailStart, cdOff - tailStart + cdSize) : await v.range(cdOff, cdOff + cdSize);
    const c = new DataView(cd), out = [];
    for (let p = 0, k = 0; k < n && p + 46 <= cd.byteLength && c.getUint32(p, true) === 0x02014b50; k++) {
      const nl = c.getUint16(p + 28, true);
      out.push([dec(new Uint8Array(cd, p + 46, nl)), c.getUint32(p + 24, true)]);
      p += 46 + nl + c.getUint16(p + 30, true) + c.getUint16(p + 32, true);
    }
    return [out, n];
  }
  async function tarList(v, gz) {
    const r = await fetch(v.url);
    if (!r.ok) throw new Error(/^text\/plain/.test(r.headers.get('content-type') || '') ? (await r.text() || r.statusText) : r.status === 404 ? 'This copy of the archive does not include this file.' : r.statusText);
    let s = r.body;
    if (gz) s = s.pipeThrough(new DecompressionStream('gzip'));
    const rd = s.getReader(), out = [];
    let buf = new Uint8Array(0), skip = 0, long = '';
    try {
      while (out.length < 3000) {
        const { done, value } = await rd.read();
        if (done || !v.still()) break;
        const b = new Uint8Array(buf.length + value.length);
        b.set(buf); b.set(value, buf.length); buf = b;
        while (true) {
          if (skip) { const k = Math.min(skip, buf.length); buf = buf.subarray(k); skip -= k; if (skip) break; }
          if (buf.length < 512) break;
          const h = buf.subarray(0, 512);
          if (h.every((x) => x === 0)) { buf = new Uint8Array(0); break; }
          const str = (a, n) => dec(h.subarray(a, a + n)).replace(/\0.*$/s, '');
          const size = parseInt(str(124, 12).trim() || '0', 8) || 0, type = String.fromCharCode(h[156] || 48);
          const name = long || ((str(345, 155) ? str(345, 155) + '/' : '') + str(0, 100));
          long = '';
          buf = buf.subarray(512);
          const pad = Math.ceil(size / 512) * 512;
          if (type === 'L' && buf.length >= size) { long = dec(buf.subarray(0, size)).replace(/\0.*$/s, ''); }
          if (type === '0' || type === '\0' || type === '5') out.push([name + (type === '5' && !name.endsWith('/') ? '/' : ''), size]);
          skip = pad;
        }
      }
    } finally { rd.cancel().catch(() => {}); }
    return [out, out.length];
  }
  function listing(st, v, files, n) {
    const shown = files.slice(0, 3000), total = files.reduce((a, f) => a + f[1], 0);
    st.innerHTML = `<p class="faint pad">${v.num(n)} entries${files.length >= n ? ' · ' + v.bytes(total) + ' unpacked' : ''}${shown.length < n ? `, the first ${v.num(shown.length)} shown` : ''}</p>` +
      `<div class="vtable"><table><thead><tr><th>Name</th><th class="r">Size</th></tr></thead><tbody>${shown.map((f) => `<tr><td>${H.esc(f[0])}</td><td class="r">${f[0].endsWith('/') ? '' : v.bytes(f[1])}</td></tr>`).join('')}</tbody></table></div>`;
  }
  view({
    name: 'zip', test: (e, x) => set('zip jar apk war ipa xpi whl nupkg cbz').has(x),
    async show(st, v) { const [f, n] = await zipList(v); if (v.still()) listing(st, v, f, n); }
  });
  view({
    name: 'tar', test: (e, x) => x === 'tar' || x === 'tgz' || (x === 'gz' && /\.tar\.gz$/i.test(e.name)),
    async show(st, v) {
      if (v.x !== 'tar' && !('DecompressionStream' in window)) return note(st, 'This browser cannot unpack gzip.');
      st.innerHTML = '<p class="muted pad">Reading…</p>';
      const [f, n] = await tarList(v, v.x !== 'tar');
      if (v.still()) listing(st, v, f, n);
    }
  });

  // ---- Office documents: the text of a Word file, the cells of a
  // spreadsheet, the words on each slide.
  const office = async (v) => unzip(await v.all(64 * MB));
  const W = 'http://schemas.openxmlformats.org/wordprocessingml/2006/main';
  view({
    name: 'word', test: (e, x) => x === 'docx',
    async show(st, v) {
      const z = await office(v), d = xml(await z('word/document.xml'));
      if (!v.still()) return;
      if (!d) return note(st, 'This Word file has no text the viewer can read.');
      const run = (p) => tags(p, 't').map((t) => H.esc(t.textContent)).join('');
      const out = [];
      for (const el of tags(d, 'body')[0] ? tags(d, 'body')[0].children : []) {
        if (el.localName === 'p') {
          const st0 = tags(el, 'pStyle')[0], style = st0 ? st0.getAttributeNS(W, 'val') || st0.getAttribute('w:val') || '' : '';
          const h = style.match(/^(?:Heading|heading)\s*(\d)/) || (style === 'Title' ? [0, 1] : null);
          const t = run(el);
          if (t) out.push(h ? `<h${Math.min(6, +h[1] + 1)}>${t}</h${Math.min(6, +h[1] + 1)}>` : `<p>${t}</p>`);
        } else if (el.localName === 'tbl') {
          out.push(`<table>${tags(el, 'tr').map((r) => `<tr>${tags(r, 'tc').map((c) => `<td>${tags(c, 'p').map(run).join('<br>')}</td>`).join('')}</tr>`).join('')}</table>`);
        }
      }
      st.innerHTML = `<div class="vbody doc">${out.join('') || '<p class="muted">No text.</p>'}</div>`;
    }
  });
  view({
    name: 'sheet', test: (e, x) => x === 'xlsx',
    async show(st, v) {
      const z = await office(v);
      const strings = tags(xml(await z('xl/sharedStrings.xml')), 'si').map((s) => tags(s, 't').map((t) => t.textContent).join(''));
      const wb = xml(await z('xl/workbook.xml')), rels = xml(await z('xl/_rels/workbook.xml.rels'));
      const target = (rid) => { const r = tags(rels, 'Relationship').find((x) => x.getAttribute('Id') === rid); const t = r ? r.getAttribute('Target') : ''; return t.startsWith('/') ? t.slice(1) : 'xl/' + t; };
      const sheets = tags(wb, 'sheet').map((s) => [s.getAttribute('name'), target(s.getAttributeNS('http://schemas.openxmlformats.org/officeDocument/2006/relationships', 'id') || s.getAttribute('r:id'))]);
      if (!v.still()) return;
      if (!sheets.length) return note(st, 'This spreadsheet has no sheets the viewer can read.');
      const col = (ref) => { let n = 0; for (const ch of ref.replace(/\d+$/, '')) n = n * 26 + ch.charCodeAt(0) - 64; return n - 1; };
      const show = async (i) => {
        const d = xml(await z(sheets[i][1])), rows = [];
        let width = 0;
        for (const r of tags(d, 'row').slice(0, 1000)) {
          const row = [];
          for (const c of tags(r, 'c')) {
            const k = col(c.getAttribute('r') || 'A1');
            if (k > 60) continue;
            const t = c.getAttribute('t'), val = (tags(c, 'v')[0] || {}).textContent || '';
            row[k] = t === 's' ? strings[+val] || '' : t === 'inlineStr' ? tags(c, 't').map((x) => x.textContent).join('') : t === 'b' ? (val === '1' ? 'TRUE' : 'FALSE') : val;
          }
          width = Math.max(width, row.length);
          rows.push(row);
        }
        const letters = Array.from({ length: width }, (_, k) => (k >= 26 ? String.fromCharCode(64 + Math.floor(k / 26)) : '') + String.fromCharCode(65 + k % 26));
        body.innerHTML = table(rows.map((r) => Array.from({ length: width }, (_, k) => r[k] == null ? '' : r[k])), letters);
      };
      st.innerHTML = `${sheets.length > 1 ? `<div class="vbar">${sheets.map((s, i) => `<button type="button" class="pill" data-sh="${i}" aria-pressed="${i === 0}">${H.esc(s[0])}</button>`).join('')}</div>` : ''}<div class="vbody"></div>`;
      const body = st.querySelector('.vbody');
      st.querySelectorAll('[data-sh]').forEach((b) => { b.onclick = () => { st.querySelectorAll('[data-sh]').forEach((x) => x.setAttribute('aria-pressed', x === b)); show(+b.dataset.sh); }; });
      await show(0);
    }
  });
  view({
    name: 'slides', test: (e, x) => x === 'pptx',
    async show(st, v) {
      const z = await office(v);
      const names = [...z.files.keys()].filter((n) => /^ppt\/slides\/slide\d+\.xml$/.test(n)).sort((a, b) => parseInt(a.match(/\d+/)[0], 10) - parseInt(b.match(/\d+/)[0], 10));
      const slides = [];
      for (const n of names.slice(0, 300)) slides.push(tags(xml(await z(n)), 'p').map((p) => tags(p, 't').map((t) => t.textContent).join('')).filter(Boolean));
      if (!v.still()) return;
      st.innerHTML = `<div class="vbody doc">${slides.map((s, i) => `<section class="slide"><div class="faint">Slide ${i + 1}</div>${s.length ? `<h3>${H.esc(s[0])}</h3>${s.slice(1).map((t) => `<p>${H.esc(t)}</p>`).join('')}` : '<p class="muted">No text.</p>'}</section>`).join('') || '<p class="muted">No slides.</p>'}</div>`;
    }
  });

  // ---- WebAssembly: what a module needs and what it gives.
  view({
    name: 'wasm', test: (e, x) => x === 'wasm',
    async show(st, v) {
      const buf = await v.all(64 * MB);
      if (!v.still()) return;
      let m;
      try { m = await WebAssembly.compile(buf); } catch (err) { return note(st, 'The browser cannot read this module: ' + err.message); }
      const im = WebAssembly.Module.imports(m), ex = WebAssembly.Module.exports(m);
      const wasi = im.some((i) => /^wasi/.test(i.module));
      st.innerHTML = `<div class="vbody doc"><p class="muted">A WebAssembly module${wasi ? ' built for WASI (a command-line program)' : ''}: ${v.num(ex.length)} exports, ${v.num(im.length)} imports.</p><h4>Exports</h4>${table(ex.map((x) => [x.name, x.kind]), ['Name', 'Kind'])}<h4>Imports</h4>${table(im.map((x) => [x.module, x.name, x.kind]), ['Module', 'Name', 'Kind'])}</div>`;
    }
  });

  // ---- Running code. JavaScript runs in a sandboxed frame, an origin of
  // its own with no access to the explorer or the archive; what it logs
  // shows below the code. Python runs the same way on Pyodide (CPython built
  // for WebAssembly), which the frame downloads from a CDN, only when asked.
  const PYODIDE = 'https://cdn.jsdelivr.net/pyodide/v0.27.2/full/';
  const harness = (lang, code) => `<!doctype html><meta charset="utf-8"><script>
const send = (k, t) => parent.postMessage({ i2f: 1, k, t: String(t) }, '*');
const fmt = (a) => a.map((x) => typeof x === 'string' ? x : (() => { try { return JSON.stringify(x); } catch (e) { return String(x); } })()).join(' ');
['log', 'info', 'warn', 'error', 'debug'].forEach((k) => { console[k] = (...a) => send(k === 'error' || k === 'warn' ? 'err' : 'out', fmt(a)); });
addEventListener('error', (e) => send('err', e.message));
addEventListener('unhandledrejection', (e) => send('err', 'Unhandled rejection: ' + (e.reason && e.reason.message || e.reason)));
const CODE = ${JSON.stringify(code).replace(/</g, '\\u003c')};
${lang === 'py' ? `send('note', 'Loading Python (Pyodide)…');
const s = document.createElement('script'); s.src = ${JSON.stringify(PYODIDE + 'pyodide.js')};
s.onerror = () => send('err', 'Could not load Pyodide from ${PYODIDE}');
s.onload = async () => {
  try {
    const py = await loadPyodide({ indexURL: ${JSON.stringify(PYODIDE)}, stdout: (t) => send('out', t), stderr: (t) => send('err', t) });
    send('note', 'Running…');
    await py.loadPackagesFromImports(CODE, { messageCallback: (m) => send('note', m) });
    await py.runPythonAsync(CODE);
    send('done', '');
  } catch (e) { send('err', e.message); send('done', ''); }
};
document.head.appendChild(s);` : `const t0 = performance.now();
const s = document.createElement('script');
if (/^\\s*(import|export)\\s/m.test(CODE)) s.type = 'module';
s.textContent = CODE;
document.head.appendChild(s);
setTimeout(() => send('done', Math.round(performance.now() - t0) + ' ms'), 0);`}
<\/script>`;
  let runN = 0;
  function runner(st, v, lang) {
    const bar = D.createElement('div');
    bar.className = 'vbar run';
    bar.innerHTML = `<button type="button" class="btn sm" data-run>▶ Run</button><span class="faint">${lang === 'py' ? 'Runs on Pyodide (about 10 MB from cdn.jsdelivr.net), sandboxed in this page' : 'Runs sandboxed in this page, with no access to the explorer or your files'}</span>`;
    st.prepend(bar);
    bar.querySelector('[data-run]').onclick = async () => {
      const code = dec(await v.all(4 * MB));
      let out = st.querySelector('.runout');
      if (!out) { out = D.createElement('div'); out.className = 'runout'; st.append(out); }
      out.innerHTML = '<pre class="code out"><code></code></pre>';
      const pre = out.querySelector('code'), id = ++runN;
      const add = (k, t) => { const s = D.createElement('span'); s.className = 'o-' + k; s.textContent = t; pre.append(s, '\n'); out.scrollIntoView({ block: 'nearest' }); };
      const f = D.createElement('iframe');
      f.setAttribute('sandbox', 'allow-scripts');
      f.hidden = true;
      const on = (ev) => {
        if (ev.source !== f.contentWindow || !ev.data || !ev.data.i2f || runN !== id || !f.isConnected) return;
        if (ev.data.k === 'done') { add('note', 'Done' + (ev.data.t ? ' in ' + ev.data.t : '') + '.'); return; }
        add(ev.data.k, ev.data.t);
      };
      addEventListener('message', on);
      const old = st.querySelector('iframe.runner');
      if (old) old.remove();
      f.className = 'runner';
      f.srcdoc = harness(lang, code);
      st.append(f);
    };
  }
  const RUN = { js: 'js', mjs: 'js', cjs: 'js', py: 'py' };

  // ---- Thumbnails made in the browser: a frame of a video, for when
  // explore has no ffmpeg (it keeps what is sent back) and from gateways;
  // the first lines of a text file, for grid tiles.
  const frames = new Map(); // url -> promise of a blob URL or null
  let busy = 0;
  const queue = [];
  const next = () => { while (busy < 2 && queue.length) { busy++; queue.shift()().finally(() => { busy--; next(); }); } };
  function frame(url, w) {
    if (frames.has(url)) return frames.get(url);
    const p = new Promise((res) => queue.push(() => new Promise((done) => {
      const vid = D.createElement('video');
      let over = false;
      const end = (b) => { if (over) return; over = true; clearTimeout(t); vid.removeAttribute('src'); vid.load(); res(b); done(); };
      const t = setTimeout(() => end(null), 20000);
      vid.muted = true; vid.preload = 'metadata'; vid.crossOrigin = 'anonymous';
      vid.onloadedmetadata = () => { vid.currentTime = Math.min(3, (vid.duration || 0) / 3); };
      vid.onseeked = () => {
        try {
          const s = Math.min(1, w / Math.max(vid.videoWidth, vid.videoHeight));
          const c = D.createElement('canvas');
          c.width = Math.max(1, Math.round(vid.videoWidth * s)); c.height = Math.max(1, Math.round(vid.videoHeight * s));
          c.getContext('2d').drawImage(vid, 0, 0, c.width, c.height);
          c.toBlob((b) => end(b), 'image/jpeg', 0.8);
        } catch (err) { end(null); }
      };
      vid.onerror = () => end(null);
      vid.src = url;
    })));
    next();
    frames.set(url, p);
    return p;
  }
  async function lines(head) {
    const t = dec(await head(2048));
    if (/[\u0000-\u0008\u000e-\u001f\ufffd]/.test(t.slice(0, 2000))) return null; // not text after all
    return t.split('\n').slice(0, 14).map((l) => l.slice(0, 60)).join('\n');
  }

  // ---- Code and plain text: highlighted, with line numbers. Scripts that
  // can run (JavaScript, Python) get a Run bar too.
  view({
    name: 'code', test: (e) => e.kind === 'code' || e.kind === 'text',
    async show(st, v) {
      const buf = await v.head(4 * MB);
      if (!v.still()) return;
      const text = dec(buf), fam = famOf(v.x) || (v.e.kind === 'code' ? 'c' : '');
      const modes = fam ? [['hl', 'Highlighted', (t) => t, fam], ['plain', 'Plain', (t) => t, '']] : [['plain', 'Plain', (t) => t, '']];
      codeView(st, v, text, { kind: fam ? 'code' : 'text', modes, cut: v.e.bytes > buf.byteLength });
      const lang = RUN[v.x];
      if (lang) runner(st, v, lang);
    }
  });

  window.I2FV = {
    viewers, view, unzip, md,
    use(helpers) { H = helpers; },
    find(e, x) { return viewers.find((v) => v.test(e, x)) || null; },
    runs: (x) => RUN[x] || null,
    runner, frame, lines, hl
  };
})();
