// A CAR of dev assets with three roots of three codecs:
//   node devbundle.mjs <folder> <out.car>
//  1. the folder (dag-pb UnixFS): code, logs, configs, data, assets;
//  2. a dag-cbor build record linking the commit's tree, each step's log
//     and the artifacts;
//  3. a dag-json release manifest linking the build record and the tarball.
// The explorer walks all three: the folder as files, the records as JSON
// with their links.
import { createHelia } from 'helia'
import { unixfs, globSource } from '@helia/unixfs'
import { CarWriter } from '@ipld/car'
import * as dagCbor from '@ipld/dag-cbor'
import * as dagJson from '@ipld/dag-json'
import { CID } from 'multiformats/cid'
import { sha256 } from 'multiformats/hashes/sha2'
import { Readable } from 'node:stream'
import fs from 'node:fs'
const [dir, out] = process.argv.slice(2)
const helia = await createHelia({ start: false })
const fsys = unixfs(helia)
let tree
const files = {}
for await (const e of fsys.addAll(globSource(dir, '**/*'), { wrapWithDirectory: true })) {
  tree = e.cid
  files[e.path] = e.cid
}
const link = (p) => { if (!files['/' + p] && !files[p]) throw new Error('no ' + p); return files['/' + p] || files[p] }
async function put (codec, value) {
  const bytes = codec.encode(value)
  const cid = CID.create(1, codec.code, await sha256.digest(bytes))
  await helia.blockstore.put(cid, bytes)
  return cid
}
const build = await put(dagCbor, {
  type: 'build',
  project: 'tidepool',
  commit: '9f2c4e1',
  branch: 'main',
  started: '2025-03-14T09:26:53Z',
  finished: '2025-03-14T09:31:07Z',
  source: tree,
  steps: [
    { name: 'install', seconds: 41, ok: true, log: link('logs/ci/01-install.log') },
    { name: 'lint', seconds: 12, ok: true, log: link('logs/ci/02-lint.log') },
    { name: 'test', seconds: 133, ok: true, log: link('logs/ci/03-test.log') },
    { name: 'build', seconds: 68, ok: true, log: link('logs/ci/04-build.log') }
  ],
  artifacts: { tarball: link('dist/tidepool-1.4.0.tar.gz'), coverage: link('reports/coverage.json') }
})
const release = await put(dagJson, {
  name: 'tidepool',
  version: '1.4.0',
  date: '2025-03-14',
  notes: 'Faster sync, smaller bundle, dark mode for the dashboard.',
  build,
  tarball: link('dist/tidepool-1.4.0.tar.gz'),
  changelog: link('CHANGELOG.md')
})
const roots = [tree, build, release]
const { writer, out: bytes } = await CarWriter.create(roots)
const done = new Promise((resolve, reject) => Readable.from(bytes).pipe(fs.createWriteStream(out)).on('finish', resolve).on('error', reject))
// Every block once, records first so a reader meets them early.
const seen = new Set()
async function walk (cid) {
  if (seen.has(cid.toString())) return
  seen.add(cid.toString())
  const b = await bytesOf(await helia.blockstore.get(cid))
  await writer.put({ cid, bytes: b })
  if (cid.code === dagCbor.code) for (const [, l] of walkLinks(dagCbor.decode(b))) await walk(l)
  else if (cid.code === dagJson.code) for (const [, l] of walkLinks(dagJson.decode(b))) await walk(l)
}
// Newer blockstores hand back the bytes as a stream of chunks.
async function bytesOf (v) {
  if (v instanceof Uint8Array) return v
  const parts = []
  for await (const p of v) parts.push(p)
  return Buffer.concat(parts)
}
function * walkLinks (v, p = '') {
  const c = CID.asCID(v)
  if (c) { yield [p, c]; return }
  if (v && typeof v === 'object' && !(v instanceof Uint8Array)) for (const [k, x] of Object.entries(v)) yield * walkLinks(x, p + '/' + k)
}
await walk(release); await walk(build)
// Then the tree, from the CAR export of the folder.
const { car } = await import('@helia/car')
const { CarBlockIterator } = await import('@ipld/car')
const chunks = []
for await (const c of car(helia).export(tree)) chunks.push(c)
for await (const b of await CarBlockIterator.fromBytes(Buffer.concat(chunks))) {
  if (seen.has(b.cid.toString())) continue
  seen.add(b.cid.toString())
  await writer.put(b)
}
await writer.close()
await done
console.log('dev-bundle', out, 'roots', roots.map(String).join(' '))
await helia.stop()
