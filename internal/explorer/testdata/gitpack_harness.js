// Runs gitpack.js under node for gitpack_test.go.
//   node gitpack_harness.js <gitpack.js> walk '<json>'    list every commit's files
//   node gitpack_harness.js <gitpack.js> hostile           hostile packs, each must fail cleanly
'use strict';
const fs = require('fs'), vm = require('vm'), zlib = require('zlib'), nodeCrypto = require('crypto');
if (typeof globalThis.crypto === 'undefined') globalThis.crypto = nodeCrypto.webcrypto;
vm.runInThisContext(fs.readFileSync(process.argv[2], 'utf8'));
const G = globalThis.I2FGit;
const mode = process.argv[3];

async function walk(arg) {
  const repo = G.open({ packs: arg.packs, format: arg.format });
  const out = [];
  for (const c of arg.commits) {
    const cm = await repo.commit(c.id);
    if (cm.tree !== c.tree) throw new Error(`commit ${c.id}: tree ${cm.tree}, want ${c.tree}`);
    out.push('== ' + c.id);
    const rec = async (id, pre) => {
      for (const e of await repo.tree(id)) {
        const type = e.kind === 'dir' ? 'tree' : e.kind === 'module' ? 'commit' : 'blob';
        out.push(`${e.mode.padStart(6, '0')} ${type} ${e.id}\t${pre}${e.name}`);
        if (type === 'tree') await rec(e.id, pre + e.name + '/');
        else if (type === 'blob') {
          const o = await repo.read(e.id);
          if (o.type !== 'blob' || !o.checked) throw new Error(`${pre}${e.name}: ${o.type}, checked ${o.checked}`);
        }
      }
    };
    await rec(cm.tree, '');
  }
  if (arg.cold) {
    // a fresh reader: what showing one folder costs
    const r2 = G.open({ packs: arg.packs, format: arg.format });
    await r2.tree(arg.cold);
    out.push('COLD ' + JSON.stringify(r2.stats));
  }
  out.push('STATS ' + JSON.stringify(repo.stats));
  console.log(out.join('\n'));
}

// What each step costs on a fresh reader: { packs, format, steps: [[op, id]] },
// op tree (one folder), read (one object) or walk (every file under a tree).
async function probe(arg) {
  const repo = G.open({ packs: arg.packs, format: arg.format });
  for (const [op, id] of arg.steps) {
    const before = Object.assign({}, repo.stats), t0 = Date.now();
    let n = 0;
    const rec = async (tid) => { for (const e of await repo.tree(tid)) { n++; if (e.kind === 'dir') await rec(e.id); else if (e.kind !== 'module') await repo.read(e.id); } };
    if (op === 'tree') n = (await repo.tree(id)).length;
    else if (op === 'read') n = (await repo.read(id)).data.length;
    else await rec(id);
    const d = {};
    for (const k in repo.stats) d[k] = repo.stats[k] - before[k];
    console.log(JSON.stringify({ op, id: id.slice(0, 10), n, ms: Date.now() - t0, ...d }));
  }
}

// ---- hostile packs, served from memory
function obj(type, data) { // a full pack entry
  let n = data.length, c = (type << 4) | (n & 15);
  const h = [];
  n = Math.floor(n / 16);
  while (n) { h.push(c | 0x80); c = n & 0x7f; n = Math.floor(n / 128); }
  h.push(c);
  return Buffer.concat([Buffer.from(h), zlib.deflateSync(data)]);
}
function hdr(type, size) {
  let n = size, c = (type << 4) | (n & 15);
  const h = [];
  n = Math.floor(n / 16);
  while (n) { h.push(c | 0x80); c = n & 0x7f; n = Math.floor(n / 128); }
  h.push(c);
  return Buffer.from(h);
}
const vint = (n) => { const b = []; do { let c = n & 0x7f; n = Math.floor(n / 128); if (n) c |= 0x80; b.push(c); } while (n); return Buffer.from(b); };
const gitID = (type, data) => nodeCrypto.createHash('sha1').update(`${type} ${data.length}\0`).update(data).digest();
// objects: [{id, bytes}] laid out in order after the 12-byte pack header.
function build(objects) {
  let off = 12;
  const parts = [Buffer.from('PACK\0\0\0\x02'), Buffer.alloc(4)];
  parts[1].writeUInt32BE(objects.length);
  const ents = objects.map((o) => { const e = { id: o.id, off }; parts.push(o.bytes); off += o.bytes.length; return e; });
  let pack = Buffer.concat(parts);
  pack = Buffer.concat([pack, nodeCrypto.createHash('sha1').update(pack).digest()]);
  ents.sort((a, b) => Buffer.compare(a.id, b.id));
  const fan = Buffer.alloc(1024);
  for (let i = 0; i < 256; i++) fan.writeUInt32BE(ents.filter((e) => e.id[0] <= i).length, 4 * i);
  const offs = Buffer.alloc(4 * ents.length), crcs = Buffer.alloc(4 * ents.length);
  ents.forEach((e, i) => offs.writeUInt32BE(e.off, 4 * i));
  let idx = Buffer.concat([Buffer.from([0xff, 0x74, 0x4f, 0x63, 0, 0, 0, 2]), fan, ...ents.map((e) => e.id), crcs, offs, pack.subarray(pack.length - 20)]);
  idx = Buffer.concat([idx, nodeCrypto.createHash('sha1').update(idx).digest()]);
  return { pack, idx };
}
function server(files, opts) {
  const stats = { requests: 0, served: 0 };
  const fetch = async (url, init) => {
    stats.requests++;
    const b = files[url];
    if (!b) return new Response('no', { status: 404 });
    const r = init && init.headers && init.headers.Range;
    if (!r || (opts && opts.noRange)) { stats.served += b.length; return new Response(b, { status: 200, headers: { 'Content-Length': String(b.length) } }); }
    const m = /^bytes=(\d+)-(\d*)$/.exec(r);
    const a = +m[1], e = m[2] === '' ? b.length - 1 : Math.min(+m[2], b.length - 1);
    if (a >= b.length) return new Response('', { status: 416 });
    const s = b.subarray(a, e + 1);
    stats.served += s.length;
    return new Response(s, { status: 206, headers: { 'Content-Range': `bytes ${a}-${e}/${b.length}`, 'Content-Length': String(s.length) } });
  };
  return { fetch, stats };
}
async function attempt(name, objects, id, opts) {
  const { pack, idx } = build(objects);
  const srv = server({ p: pack, i: idx }, opts);
  const repo = G.open({ packs: [{ pack: 'p', idx: 'i' }], format: 'sha1', fetch: srv.fetch, limits: opts && opts.limits });
  let res;
  try { const o = await (opts && opts.tree ? repo.tree(id) : repo.read(id)); res = 'ok ' + o.type + ' ' + o.data.length; } catch (err) { res = 'error: ' + err.message; }
  return { name, res, requests: srv.stats.requests, served: srv.stats.served, objects: repo.stats.objects };
}

async function hostile() {
  const results = [];
  const blob = Buffer.from('hello world\n'), bid = gitID('blob', blob);
  results.push(await attempt('good', [{ id: bid, bytes: obj(3, blob) }], bid.toString('hex')));
  // the bytes do not match the id
  const fake = Buffer.alloc(20, 7);
  results.push(await attempt('wrong-id', [{ id: fake, bytes: obj(3, blob) }], fake.toString('hex')));
  // a zlib bomb: says 12 bytes, inflates to 200 MB
  const bomb = Buffer.concat([hdr(3, 12), zlib.deflateSync(Buffer.alloc(200 << 20))]);
  results.push(await attempt('bomb', [{ id: fake, bytes: bomb }], fake.toString('hex')));
  // header says 1 GB
  results.push(await attempt('huge', [{ id: fake, bytes: Buffer.concat([hdr(3, 1 << 30), zlib.deflateSync(blob)]) }], fake.toString('hex')));
  // ref-delta cycle: a's base is b, b's base is a
  const a = Buffer.alloc(20, 1), b = Buffer.alloc(20, 2);
  const copy = Buffer.concat([vint(12), vint(12), Buffer.from([0x90, 12])]), d = zlib.deflateSync(copy);
  const rd = (base) => Buffer.concat([hdr(7, copy.length), base, d]);
  results.push(await attempt('ref-cycle', [{ id: a, bytes: rd(b) }, { id: b, bytes: rd(a) }], a.toString('hex')));
  // a long ofs-delta chain, each step a copy of the whole base
  const chain = [{ id: bid, bytes: obj(3, blob) }];
  let off = 12 + chain[0].bytes.length, prev = 12;
  for (let i = 0; i < 1500; i++) {
    const dd = d, h = hdr(6, copy.length);
    let ofs = off - prev, enc = [ofs & 0x7f];
    while ((ofs >>= 7)) { ofs--; enc.unshift(0x80 | (ofs & 0x7f)); }
    const id = nodeCrypto.createHash('sha1').update('x' + i).digest();
    const bytes = Buffer.concat([h, Buffer.from(enc), dd]);
    chain.push({ id, bytes });
    prev = off;
    off += bytes.length;
  }
  results.push(await attempt('deep-chain', chain, chain[chain.length - 1].id.toString('hex')));
  // a delta that copies from past its base
  const badRaw = Buffer.concat([vint(12), vint(100), Buffer.from([0x91, 0, 100])]);
  results.push(await attempt('copy-out', [{ id: bid, bytes: obj(3, blob) }, { id: fake, bytes: Buffer.concat([hdr(7, badRaw.length), bid, zlib.deflateSync(badRaw)]) }], fake.toString('hex')));
  // a delta whose base is in no pack
  results.push(await attempt('no-base', [{ id: fake, bytes: Buffer.concat([hdr(7, copy.length), a, d]) }], fake.toString('hex')));
  // a delta that builds 64 MB again and again: work cap
  const big = Buffer.alloc(60 << 20, 0x61), bigID = gitID('blob', big);
  const work = [{ id: bigID, bytes: obj(3, big) }];
  let woff = 12 + work[0].bytes.length, wprev = 12;
  for (let i = 0; i < 20; i++) {
    const ops = [vint(big.length), vint(big.length)];
    for (let o = 0; o < big.length; o += 0x10000) ops.push(Buffer.from([0x80 | 0x01 | 0x02 | 0x04, o & 0xff, (o >> 8) & 0xff, (o >> 16) & 0xff]));
    const raw = Buffer.concat(ops), dz = zlib.deflateSync(raw);
    let ofs = woff - wprev, enc = [ofs & 0x7f];
    while ((ofs >>= 7)) { ofs--; enc.unshift(0x80 | (ofs & 0x7f)); }
    const bytes = Buffer.concat([hdr(6, raw.length), Buffer.from(enc), dz]);
    work.push({ id: nodeCrypto.createHash('sha1').update('w' + i).digest(), bytes });
    wprev = woff;
    woff += bytes.length;
  }
  results.push(await attempt('work', work, work[work.length - 1].id.toString('hex')));
  // a server that ignores Range: a small pack is fine, a large one refused
  results.push(await attempt('no-range-small', [{ id: bid, bytes: obj(3, blob) }], bid.toString('hex'), { noRange: true }));
  results.push(await attempt('no-range-large', [{ id: bid, bytes: obj(3, blob) }, { id: bigID, bytes: obj(3, big) }], bid.toString('hex'), { noRange: true, limits: { whole: 16 << 10 } }));
  // a damaged tree
  const tr = Buffer.from('100644 a\0short'), tid = gitID('tree', tr);
  results.push(await attempt('bad-tree', [{ id: tid, bytes: obj(2, tr) }], tid.toString('hex'), { tree: true }));
  // an idx whose checksum is wrong
  {
    const { pack, idx } = build([{ id: bid, bytes: obj(3, blob) }]);
    idx[idx.length - 1] ^= 1;
    const srv = server({ p: pack, i: idx });
    let res;
    try { await G.open({ packs: [{ pack: 'p', idx: 'i' }], fetch: srv.fetch }).read(bid.toString('hex')); res = 'ok'; } catch (err) { res = 'error: ' + err.message; }
    results.push({ name: 'idx-checksum', res, requests: srv.stats.requests });
  }
  console.log(JSON.stringify(results));
}

(mode === 'walk' ? walk(JSON.parse(process.argv[4])) : mode === 'probe' ? probe(JSON.parse(process.argv[4])) : hostile()).catch((err) => { console.error(err.stack || err); process.exit(1); });
