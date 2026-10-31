// A photo app's Helia store: node helia.mjs <store dir> <content dir>
// Files go in with Helia's defaults (CIDv1, raw leaves, 1 MiB chunks), the
// same as `ipfs add --cid-version 1 --chunker=size-1048576`, so the NASA photos and the Bluesky
// images here have the same CIDs as in the kubo repo, art.car and @bsky.app.
import { createHelia } from 'helia'
import { FsBlockstore } from 'blockstore-fs'
import { FsDatastore } from 'datastore-fs'
import { unixfs, globSource } from '@helia/unixfs'
import { mfs } from '@helia/mfs'
import fs from 'node:fs'
const [out, content] = process.argv.slice(2)
const helia = await createHelia({ blockstore: new FsBlockstore(out + '/blocks'), datastore: new FsDatastore(out + '/datastore'), start: false })
const fsys = unixfs(helia)
async function addDir (dir) {
  let root
  for await (const e of fsys.addAll(globSource(dir, '**/*'), { wrapWithDirectory: true })) root = e.cid
  return root
}
const pins = {
  'iceland-2024': content + '/photos/2024-iceland',
  'nasa-favorites': content + '/pub/photos/nasa/webb',
  'saved-from-bluesky': content + '/from-bluesky'
}
for (const [name, dir] of Object.entries(pins)) {
  const cid = await addDir(dir)
  for await (const _ of helia.pins.add(cid, { metadata: { name } }));
  console.log('helia', name, cid.toString())
}
// Albums and captions the app keeps in MFS.
const m = mfs(helia)
await m.mkdir('/albums')
await m.writeBytes(new TextEncoder().encode(JSON.stringify({ name: 'Iceland 2024', cover: 'IMG_4103.jpg', count: 14 }, null, 2)), '/albums/iceland-2024.json')
await m.writeBytes(new TextEncoder().encode(JSON.stringify({ name: 'Space', source: 'NASA, public domain' }, null, 2)), '/albums/space.json')
await m.mkdir('/notes')
for (const f of ['packing-iceland.md', 'ideas.md']) await m.writeBytes(fs.readFileSync(content + '/notes/' + f), '/notes/' + f)
await helia.stop()
