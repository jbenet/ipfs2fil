// A js-ipfs repo (repo 12): node jsipfs.mjs <repo dir> <content dir>
import { create, globSource } from 'ipfs-core'
import fs from 'node:fs'
const [repo, content] = process.argv.slice(2)
const ipfs = await create({ repo, offline: true, start: false, silent: true, config: { Addresses: { Swarm: [] } } })
for (const dir of ['photos/2025-desert', 'pub/music/synth']) {
  let root
  for await (const e of ipfs.addAll(globSource(content + '/' + dir, '**/*'), { wrapWithDirectory: true })) root = e.cid
  await ipfs.pin.add(root)
  console.log('js-ipfs', dir, root.toString())
}
await ipfs.files.mkdir('/art', { parents: true })
for (const f of fs.readdirSync(content + '/art')) await ipfs.files.write('/art/' + f, fs.readFileSync(content + '/art/' + f), { create: true })
console.log('js-ipfs version', fs.readFileSync(repo + '/version', 'utf8'))
await ipfs.stop()
