// ipfs2fil explorer: read a git archive's packs in the browser, so the Git
// viewer can show the files of any commit. Plain JavaScript, no libraries.
//
// A git archive stores each snapshot's objects as a pack (the first one full,
// later ones thin: their deltas may name bases in earlier packs) and its .idx,
// both UnixFS files. The reader fetches every .idx whole, then only the byte
// ranges of the packs that hold the objects it is asked for (HTTP Range, from
// `ipfs2fil explore` or a gateway), inflates them with DecompressionStream,
// applies ofs and ref deltas, and checks each object it hands out against its
// id: SHA-1 (or SHA-256) over git's header and the bytes. That check is plain
// SHA-1, not SHA-1DC: the collision detection git-ipfs-archive and git run is
// not done here, so a shattered-style pair would not be told apart. The CLI's
// verify and restore do check it.
//
//   const repo = I2FGit.open({ packs: [{ pack: url, idx: url }, ...], format: 'sha1' });
//   await repo.read(id)   -> { type: 'blob', data: Uint8Array }
//   await repo.tree(id)   -> [{ mode, name, id, kind }]   kind: dir, file, exec, link, module
//   await repo.commit(id) -> { tree, parents }
//   repo.stats            -> requests, bytes fetched, objects, deltas, ...
//
// Everything a hostile archive could make large is bounded (LIMITS): object
// and idx sizes, delta chain depth, the bytes one read may inflate or build,
// and what the caches hold.
'use strict';
(() => {
  const MB = 1 << 20;
  const LIMITS = {
    object: 64 * MB, // largest object read, inflated
    depth: 1024, // longest delta chain followed (git writes at most 4095, 50 by default)
    work: 512 * MB, // bytes inflated and built by one read, across its chain
    idx: 128 * MB, // every .idx together, fetched whole
    whole: 32 * MB, // a pack a server sends whole (no Range) is kept if no larger
    chunk: 256 << 10, // packs are fetched in chunks of this many bytes...
    direct: 1 * MB, // ...except spans larger than this, fetched exactly
    chunks: 48 * MB, // chunks kept
    cache: 64 * MB, // objects kept
    packs: 4096,
    parallel: 6, // requests at once
  };
  const TYPES = [null, 'commit', 'tree', 'blob', 'tag'];
  const enc = new TextEncoder();
  const hexOf = (b) => { let s = ''; for (let i = 0; i < b.length; i++) s += (b[i] < 16 ? '0' : '') + b[i].toString(16); return s; };
  function unhex(s, n) {
    if (typeof s !== 'string' || s.length !== 2 * n || !/^[0-9a-f]+$/.test(s)) throw new Error(`"${String(s).slice(0, 80)}" is not a git object id`);
    const b = new Uint8Array(n);
    for (let i = 0; i < n; i++) b[i] = parseInt(s.slice(2 * i, 2 * i + 2), 16);
    return b;
  }
  const cmp = (a, ap, b) => { for (let i = 0; i < b.length; i++) { const d = a[ap + i] - b[i]; if (d) return d; } return 0; };

  // ---- Hashes. crypto.subtle only exists on secure origins (https,
  // localhost, file:); elsewhere SHA-1 is done here. SHA-256 repos off a
  // secure origin are read unchecked, and say so.
  function sha1(m) {
    const l = m.length, nb = ((l + 8) >> 6 << 6) + 64;
    const b = new Uint8Array(nb);
    b.set(m);
    b[l] = 0x80;
    const v = new DataView(b.buffer);
    v.setUint32(nb - 4, (l * 8) >>> 0);
    v.setUint32(nb - 8, Math.floor(l / 0x20000000));
    let h0 = 0x67452301, h1 = 0xefcdab89 | 0, h2 = 0x98badcfe | 0, h3 = 0x10325476, h4 = 0xc3d2e1f0 | 0;
    const w = new Int32Array(80);
    for (let o = 0; o < nb; o += 64) {
      for (let i = 0; i < 16; i++) w[i] = v.getInt32(o + 4 * i);
      for (let i = 16; i < 80; i++) { const x = w[i - 3] ^ w[i - 8] ^ w[i - 14] ^ w[i - 16]; w[i] = (x << 1) | (x >>> 31); }
      let a = h0, bb = h1, c = h2, d = h3, e = h4;
      for (let i = 0; i < 80; i++) {
        const f = i < 20 ? ((bb & c) | (~bb & d)) + 0x5a827999 : i < 40 ? (bb ^ c ^ d) + 0x6ed9eba1 : i < 60 ? ((bb & c) | (bb & d) | (c & d)) - 0x70e44324 : (bb ^ c ^ d) - 0x359d3e2a;
        const t = (((a << 5) | (a >>> 27)) + f + e + w[i]) | 0;
        e = d; d = c; c = (bb << 30) | (bb >>> 2); bb = a; a = t;
      }
      h0 = (h0 + a) | 0; h1 = (h1 + bb) | 0; h2 = (h2 + c) | 0; h3 = (h3 + d) | 0; h4 = (h4 + e) | 0;
    }
    const out = new Uint8Array(20), ov = new DataView(out.buffer);
    [h0, h1, h2, h3, h4].forEach((h, i) => ov.setInt32(4 * i, h));
    return out;
  }
  const subtle = () => (typeof crypto !== 'undefined' && crypto.subtle) || null;
  async function digest(format, buf) {
    const s = subtle();
    if (s) return new Uint8Array(await s.digest(format === 'sha256' ? 'SHA-256' : 'SHA-1', buf));
    return format === 'sha256' ? null : sha1(buf);
  }

  // ---- Inflate: a zlib stream to exactly size bytes, or an error.
  async function inflate(z, size) {
    if (typeof DecompressionStream === 'undefined') throw new Error('This browser cannot inflate git objects (no DecompressionStream); try a current one.');
    const ds = new DecompressionStream('deflate');
    const w = ds.writable.getWriter();
    w.write(z).catch(() => {});
    w.close().catch(() => {});
    const rd = ds.readable.getReader(), out = new Uint8Array(size);
    let n = 0;
    try {
      for (;;) {
        const { done, value } = await rd.read();
        if (done) break;
        if (n + value.length > size) { rd.cancel().catch(() => {}); throw Object.assign(new Error('an object inflates to more than its header says'), { bomb: true }); }
        out.set(value, n);
        n += value.length;
      }
    } catch (err) {
      // Bytes after the stream (an idx whose offsets leave gaps) do not matter
      // once the object is whole: its id is checked anyway.
      if (err.bomb) throw err;
      if (n !== size) throw new Error('an object does not inflate: ' + err.message);
    }
    if (n !== size) throw new Error(`an object inflates to ${n} bytes, not the ${size} its header says`);
    return out;
  }

  // ---- Deltas: git's copy and insert instructions against a base.
  function applyDelta(base, d, cap) {
    let p = 0;
    const size = () => {
      let v = 0, s = 0, c;
      do {
        if (p >= d.length || s > 49) throw new Error('a delta is cut short');
        c = d[p++];
        v += (c & 0x7f) * 2 ** s;
        s += 7;
      } while (c & 0x80);
      return v;
    };
    const bs = size(), ts = size();
    if (bs !== base.length) throw new Error(`a delta expects a base of ${bs} bytes, not ${base.length}`);
    if (ts > cap) throw tooBig(ts);
    const out = new Uint8Array(ts);
    let o = 0;
    while (p < d.length) {
      const c = d[p++];
      if (c & 0x80) {
        let off = 0, n = 0;
        for (let i = 0; i < 4; i++) if (c & (1 << i)) off += d[p++] * 2 ** (8 * i);
        for (let i = 0; i < 3; i++) if (c & (0x10 << i)) n += d[p++] * 2 ** (8 * i);
        if (p > d.length) throw new Error('a delta is cut short');
        if (n === 0) n = 0x10000;
        if (off + n > base.length || o + n > ts) throw new Error('a delta copies from outside its base');
        out.set(base.subarray(off, off + n), o);
        o += n;
      } else if (c) {
        if (p + c > d.length || o + c > ts) throw new Error('a delta inserts past its end');
        out.set(d.subarray(p, p + c), o);
        p += c;
        o += c;
      } else throw new Error('a delta has an unknown instruction');
    }
    if (o !== ts) throw new Error(`a delta builds ${o} bytes, not the ${ts} it says`);
    return out;
  }
  // The size a delta says it builds.
  function deltaTarget(d) {
    let p = 0, v = 0;
    for (let k = 0; k < 2; k++) {
      let s = 0, c;
      v = 0;
      do {
        if (p >= d.length || s > 49) throw new Error('a delta is cut short');
        c = d[p++];
        v += (c & 0x7f) * 2 ** s;
        s += 7;
      } while (c & 0x80);
    }
    return v;
  }
  const tooBig = (n) => Object.assign(new Error(`too large to read here (${Math.round(n / MB)} MB; the limit is ${LIMITS.object / MB} MB)`), { tooBig: n });

  // ---- An idx (version 2): ids sorted, with their pack offsets.
  async function parseIdx(buf, format) {
    const hs = format === 'sha256' ? 32 : 20, b = new Uint8Array(buf), v = new DataView(buf);
    if (b.length < 8 + 1024 + 2 * hs || v.getUint32(0) !== 0xff744f63 || v.getUint32(4) !== 2) throw new Error('not a version 2 pack index');
    const sum = await digest(format, b.subarray(0, b.length - hs));
    if (sum && cmp(b, b.length - hs, sum)) throw new Error('pack index checksum mismatch');
    const fan = new Uint32Array(256);
    for (let i = 0; i < 256; i++) {
      fan[i] = v.getUint32(8 + 4 * i);
      if (i && fan[i] < fan[i - 1]) throw new Error('pack index fan-out is not sorted');
    }
    const n = fan[255], ids = 8 + 1024, offs = ids + n * (hs + 4), large = offs + 4 * n;
    if (large + 2 * hs > b.length) throw new Error('pack index truncated');
    const nlarge = Math.floor((b.length - 2 * hs - large) / 8);
    for (let i = 1; i < n; i++) if (cmp(b, ids + i * hs, b.subarray(ids + (i - 1) * hs, ids + i * hs)) <= 0) throw new Error('pack index ids are not sorted');
    const off = new Float64Array(n);
    for (let i = 0; i < n; i++) {
      const x = v.getUint32(offs + 4 * i);
      if (!(x & 0x80000000)) { off[i] = x; continue; }
      const k = x & 0x7fffffff;
      if (k >= nlarge) throw new Error('pack index large offset out of range');
      const hi = v.getUint32(large + 8 * k), lo = v.getUint32(large + 8 * k + 4);
      if (hi >= 0x200000) throw new Error('pack index offset too large');
      off[i] = hi * 2 ** 32 + lo;
    }
    const sorted = Float64Array.from(off).sort();
    return { b, hs, fan, n, ids, off, sorted };
  }
  function idxFind(ix, id) {
    let lo = id[0] ? ix.fan[id[0] - 1] : 0, hi = ix.fan[id[0]];
    while (lo < hi) {
      const m = (lo + hi) >>> 1, c = cmp(ix.b, ix.ids + m * ix.hs, id);
      if (!c) return ix.off[m];
      if (c < 0) lo = m + 1; else hi = m;
    }
    return -1;
  }
  // The offset of the object after the one at off, in pack order, or -1.
  function idxNext(ix, off) {
    let lo = 0, hi = ix.sorted.length;
    while (lo < hi) { const m = (lo + hi) >>> 1; if (ix.sorted[m] <= off) lo = m + 1; else hi = m; }
    return lo < ix.sorted.length ? ix.sorted[lo] : -1;
  }

  function open(opts) {
    const format = opts.format === 'sha256' ? 'sha256' : 'sha1', hs = format === 'sha256' ? 32 : 20;
    const L = Object.assign({}, LIMITS, opts.limits || {});
    const F = opts.fetch || ((u, o) => fetch(u, o));
    const stats = { requests: 0, fetched: 0, idxBytes: 0, objects: 0, deltas: 0, crossPack: 0, checked: 0, unchecked: 0, cacheHits: 0 };
    if (!Array.isArray(opts.packs) || !opts.packs.length) throw new Error('This archive lists no packs.');
    if (opts.packs.length > L.packs) throw new Error(`This archive has ${opts.packs.length} packs; the browser reads at most ${L.packs}.`);
    const packs = opts.packs.map((p, i) => ({ i, url: p.pack, idxURL: p.idx, ix: null, size: null, whole: null }));

    // a few requests at a time
    let active = 0;
    const waiting = [];
    async function req(url, init) {
      if (active >= L.parallel) await new Promise((r) => waiting.push(r));
      active++;
      try { stats.requests++; return await F(url, init); } finally { active--; if (waiting.length) waiting.shift()(); }
    }
    async function body(r, cap) {
      const len = +(r.headers.get('Content-Length') || 0);
      if (len > cap) { r.body && r.body.cancel().catch(() => {}); throw new Error(`a response of ${len} bytes is larger than the ${cap} expected`); }
      if (!r.body || !r.body.getReader) { const a = new Uint8Array(await r.arrayBuffer()); if (a.length > cap) throw new Error('response too large'); return a; }
      const rd = r.body.getReader(), parts = [];
      let n = 0;
      for (;;) {
        const { done, value } = await rd.read();
        if (done) break;
        n += value.length;
        if (n > cap) { rd.cancel().catch(() => {}); throw new Error(`a response is larger than the ${cap} bytes expected`); }
        parts.push(value);
      }
      stats.fetched += n;
      if (parts.length === 1) return parts[0];
      const out = new Uint8Array(n);
      let o = 0;
      for (const p of parts) { out.set(p, o); o += p.length; }
      return out;
    }

    // ---- idx files: all of them, once, before the first lookup.
    let idxP = null, idxLeft = L.idx;
    function loadIdx() {
      return idxP || (idxP = Promise.all(packs.map(async (pk) => {
        const r = await req(pk.idxURL);
        if (!r.ok) throw new Error(`pack index ${pk.i + 1} of ${packs.length}: HTTP ${r.status}`);
        const b = await body(r, idxLeft);
        idxLeft -= b.length; // (fetched in parallel: each may take what was left when it began)
        if (idxLeft < 0) throw new Error(`the archive's pack indexes come to more than ${L.idx / MB} MB, more than the browser reads`);
        stats.idxBytes += b.length;
        try { pk.ix = await parseIdx(b.buffer.byteLength === b.length ? b.buffer : b.slice().buffer, format); } catch (err) { throw new Error(`pack index ${pk.i + 1}: ${err.message}`); }
      })).catch((err) => { idxP = null; idxLeft = L.idx; throw err; }));
    }
    function locate(id) {
      for (let i = packs.length - 1; i >= 0; i--) {
        const off = idxFind(packs[i].ix, id);
        if (off >= 0) return { pk: packs[i], off };
      }
      return null;
    }

    // ---- pack bytes: chunks, kept (LRU), or exact spans for large objects
    const chunks = new Map();
    let chunkBytes = 0;
    async function range(pk, a, b) {
      const r = await req(pk.url, { headers: { Range: `bytes=${a}-${b === Infinity ? '' : b - 1}` }, cache: 'no-store' });
      if (r.status === 206) {
        const m = /^bytes (\d+)-(\d+)\/(\d+|\*)$/.exec(r.headers.get('Content-Range') || '');
        if (m && +m[1] !== a) { r.body && r.body.cancel().catch(() => {}); throw new Error('the server sent other bytes than those asked for'); }
        if (m && m[3] !== '*') pk.size = +m[3];
        return body(r, b === Infinity ? L.object * 2 : b - a);
      }
      if (r.status === 200) {
        // No ranges from this server: a small pack is kept whole.
        const got = await body(r, L.whole).catch(() => { throw new Error('the server does not send byte ranges, and this pack is too large to fetch whole'); });
        pk.whole = got;
        pk.size = got.length;
        return got.subarray(a, b === Infinity ? got.length : b);
      }
      if (r.body) r.body.cancel().catch(() => {});
      throw new Error(r.status === 416 ? 'a pack is shorter than its index says' : `pack ${pk.i + 1}: HTTP ${r.status}`);
    }
    function chunk(pk, k) {
      const key = pk.i + ':' + k;
      let c = chunks.get(key);
      if (c) { chunks.delete(key); chunks.set(key, c); return c.p; }
      c = { n: 0, p: null };
      c.p = range(pk, k * L.chunk, (k + 1) * L.chunk).then((b) => {
        c.n = b.length;
        chunkBytes += b.length;
        for (const [k2, c2] of chunks) { if (chunkBytes <= L.chunks) break; if (c2 !== c && c2.n) { chunks.delete(k2); chunkBytes -= c2.n; } }
        return b;
      }, (err) => { chunks.delete(key); throw err; });
      chunks.set(key, c);
      return c.p;
    }
    async function bytes(pk, a, b) {
      // the last object: where it ends comes from the pack's size
      if (b === Infinity && pk.size == null && !pk.whole) await chunk(pk, Math.floor(a / L.chunk)).catch(() => {});
      if (b === Infinity && pk.size != null) b = pk.size - hs;
      if (pk.whole) {
        if (b > pk.whole.length || b <= a) throw new Error('a pack is shorter than its index says');
        return pk.whole.subarray(a, b);
      }
      if (b === Infinity) return range(pk, a, Infinity);
      if (b <= a) throw new Error('a pack is shorter than its index says');
      if (b - a > L.direct) return range(pk, a, b);
      const k0 = Math.floor(a / L.chunk), k1 = Math.floor((b - 1) / L.chunk);
      const got = await Promise.all(Array.from({ length: k1 - k0 + 1 }, (_, j) => chunk(pk, k0 + j)));
      if (got.length === 1) {
        const s = got[0].subarray(a - k0 * L.chunk, b - k0 * L.chunk);
        if (s.length !== b - a) throw new Error('a pack is shorter than its index says');
        return s;
      }
      const out = new Uint8Array(b - a);
      let o = 0;
      got.forEach((g, j) => { const s = g.subarray(j ? 0 : a - k0 * L.chunk, Math.min(g.length, b - (k0 + j) * L.chunk)); out.set(s, o); o += s.length; });
      if (o !== b - a) throw new Error('a pack is shorter than its index says');
      return out;
    }

    // ---- objects
    function header(raw, off) {
      let p = 0, c = raw[p++];
      const type = (c >> 4) & 7;
      let size = c & 15, shift = 4;
      while (c & 0x80) {
        if (p >= raw.length || shift > 49) throw new Error('a pack object header is cut short');
        c = raw[p++];
        size += (c & 0x7f) * 2 ** shift;
        shift += 7;
      }
      const h = { type, size, p };
      if (type === 6) {
        c = raw[p++];
        let ofs = c & 0x7f;
        while (c & 0x80) {
          if (p >= raw.length || ofs > 2 ** 45) throw new Error('a delta base offset is cut short');
          c = raw[p++];
          ofs = (ofs + 1) * 128 + (c & 0x7f);
        }
        if (!(ofs > 0 && ofs <= off - 12)) throw new Error('a delta names a base outside its pack');
        h.base = off - ofs;
      } else if (type === 7) {
        if (p + hs > raw.length) throw new Error('a delta base id is cut short');
        h.ref = raw.slice(p, p + hs);
        p += hs;
      } else if (!TYPES[type]) throw new Error(`a pack object has unknown type ${type}`);
      h.p = p;
      return h;
    }
    const cache = new Map();
    let cacheBytes = 0;
    const keep = (key, o) => {
      if (o.data.length > L.cache / 4) return;
      cache.set(key, o);
      cacheBytes += o.data.length;
      for (const [k, c] of cache) { if (cacheBytes <= L.cache) break; cache.delete(k); cacheBytes -= c.data.length; }
    };
    const fromCache = (key) => { const o = cache.get(key); if (o) { cache.delete(key); cache.set(key, o); stats.cacheHits++; } return o; };
    // The object at off in pk, deltas applied.
    async function resolve(pk, off) {
      const chain = [];
      let work = 0, base = null, at = { pk, off };
      for (let depth = 0; ; depth++) {
        if (depth > L.depth) throw new Error(`a delta chain is longer than ${L.depth}`);
        const key = at.pk.i + ':' + at.off;
        const hit = fromCache(key);
        if (hit) { base = hit; break; }
        const next = idxNext(at.pk.ix, at.off);
        const raw = await bytes(at.pk, at.off, next < 0 ? Infinity : next);
        const h = header(raw, at.off);
        if (h.size > (h.type >= 6 ? L.object + 1024 + L.object / 8 : L.object)) throw tooBig(h.size);
        work += h.size;
        if (work > L.work) throw new Error('a delta chain builds more than ' + L.work / MB + ' MB');
        const data = await inflate(raw.subarray(h.p), h.size);
        stats.objects++;
        if (h.type < 6) { base = { type: TYPES[h.type], data }; keep(key, base); break; }
        // what applying it will build counts now, before anything is built
        const ts = deltaTarget(data);
        if (ts > L.object) throw tooBig(ts);
        work += ts;
        if (work > L.work) throw new Error('a delta chain builds more than ' + L.work / MB + ' MB');
        chain.push({ key, data });
        if (h.type === 6) { at = { pk: at.pk, off: h.base }; continue; }
        const loc = locate(h.ref);
        if (!loc) throw new Error(`a delta's base ${hexOf(h.ref)} is in none of this archive's packs`);
        if (loc.pk !== at.pk) stats.crossPack++;
        at = loc;
      }
      for (let i = chain.length - 1; i >= 0; i--) {
        const data = applyDelta(base.data, chain[i].data, L.object);
        stats.deltas++;
        base = { type: base.type, data };
        keep(chain[i].key, base);
      }
      return base;
    }
    const checked = new Set();
    async function read(idHex) {
      const id = unhex(idHex, hs);
      await loadIdx();
      const loc = locate(id);
      if (!loc) throw Object.assign(new Error(`object ${idHex.slice(0, 12)} is in none of this archive's packs`), { missing: true });
      const o = await resolve(loc.pk, loc.off);
      const key = loc.pk.i + ':' + loc.off;
      if (!checked.has(key)) {
        const head = enc.encode(`${o.type} ${o.data.length}\0`), buf = new Uint8Array(head.length + o.data.length);
        buf.set(head);
        buf.set(o.data, head.length);
        const sum = await digest(format, buf);
        if (sum && cmp(sum, 0, id)) throw new Error(`object ${idHex.slice(0, 12)} does not match its id: the pack is damaged or altered`);
        if (sum) { stats.checked++; checked.add(key); } else stats.unchecked++;
      }
      return { type: o.type, data: o.data, checked: checked.has(key) };
    }
    const dec = new TextDecoder();
    async function tree(idHex) {
      const o = await read(idHex);
      if (o.type !== 'tree') throw new Error(`${idHex.slice(0, 12)} is a ${o.type}, not a tree`);
      const d = o.data, out = [];
      let p = 0;
      while (p < d.length) {
        const sp = d.indexOf(32, p), nul = sp < 0 ? -1 : d.indexOf(0, sp + 1);
        if (sp < 0 || nul < 0 || nul + 1 + hs > d.length) throw new Error(`tree ${idHex.slice(0, 12)} is damaged`);
        const mode = dec.decode(d.subarray(p, sp));
        const kind = mode === '40000' ? 'dir' : mode === '160000' ? 'module' : mode === '120000' ? 'link' : mode === '100755' ? 'exec' : 'file';
        out.push({ mode, name: dec.decode(d.subarray(sp + 1, nul)), id: hexOf(d.subarray(nul + 1, nul + 1 + hs)), kind });
        p = nul + 1 + hs;
      }
      return out;
    }
    async function commit(idHex) {
      const o = await read(idHex);
      if (o.type !== 'commit') throw new Error(`${idHex.slice(0, 12)} is a ${o.type}, not a commit`);
      const text = dec.decode(o.data), end = text.indexOf('\n\n'), head = end < 0 ? text : text.slice(0, end);
      const tree = (/^tree ([0-9a-f]+)$/m.exec(head) || [])[1] || '';
      return { tree, parents: [...head.matchAll(/^parent ([0-9a-f]+)$/gm)].map((m) => m[1]) };
    }
    return { read, tree, commit, stats, ready: loadIdx, limits: L, format };
  }

  const api = { open, LIMITS, sha1, applyDelta };
  if (typeof window !== 'undefined') window.I2FGit = api;
  if (typeof globalThis !== 'undefined') globalThis.I2FGit = api;
})();
