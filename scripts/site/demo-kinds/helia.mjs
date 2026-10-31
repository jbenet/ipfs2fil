import { createHelia } from 'helia'
import { FsBlockstore } from 'blockstore-fs'
import { FsDatastore } from 'datastore-fs'
import { unixfs, globSource } from '@helia/unixfs'
import { mfs } from '@helia/mfs'
import { car } from '@helia/car'
import fs from 'node:fs'
const [out, content, carOut] = process.argv.slice(2)
const helia = await createHelia({ blockstore: new FsBlockstore(out + '/blocks'), datastore: new FsDatastore(out + '/datastore'), start: false })
const fsys = unixfs(helia)
async function addDir (dir) {
  let root
  for await (const e of fsys.addAll(globSource(dir, '**/*'), { wrapWithDirectory: true })) root = e.cid
  return root
}
const iceland = await addDir(content + '/photos/2024-iceland')
for await (const _ of helia.pins.add(iceland, { metadata: { name: 'iceland-2024' } }));
const m = mfs(helia)
await m.mkdir('/notes')
for (const f of fs.readdirSync(content + '/notes')) await m.writeBytes(fs.readFileSync(content + '/notes/' + f), '/notes/' + f)
const art = await addDir(content + '/art')
// The CAR: the art folder, exported the way `ipfs dag export` would.
const w = fs.createWriteStream(carOut)
for await (const b of car(helia).export(art)) w.write(b)
w.end()
console.log('helia iceland', iceland.toString(), 'art', art.toString())
await helia.stop()
