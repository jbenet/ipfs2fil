// Packs a folder into a CAR with Helia's defaults (CIDv1, raw leaves, 1 MiB
// chunks): node car.mjs <folder> <out.car>
// The root is the folder, as `ipfs dag export` of `ipfs add -r --cid-version 1`
// would write it, so files shared with the other stores keep their CIDs.
import { createHelia } from 'helia'
import { unixfs, globSource } from '@helia/unixfs'
import { car } from '@helia/car'
import fs from 'node:fs'
const [dir, out] = process.argv.slice(2)
const helia = await createHelia({ start: false })
let root
for await (const e of unixfs(helia).addAll(globSource(dir, '**/*'), { wrapWithDirectory: true })) root = e.cid
const w = fs.createWriteStream(out)
for await (const b of car(helia).export(root)) w.write(b)
await new Promise((resolve) => w.end(resolve))
console.log('car', out, root.toString())
await helia.stop()
