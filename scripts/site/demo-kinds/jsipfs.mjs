import { create, globSource } from 'ipfs-core'
import fs from 'node:fs'
const [repo, content] = process.argv.slice(2)
const ipfs = await create({ repo, offline: true, start: false, silent: true, config: { Addresses: { Swarm: [] } } })
let root
for await (const e of ipfs.addAll(globSource(content + '/photos/2025-desert', '**/*'), { wrapWithDirectory: true })) root = e.cid
await ipfs.pin.add(root)
await ipfs.files.mkdir('/art', { parents: true })
for (const f of fs.readdirSync(content + '/art')) await ipfs.files.write('/art/' + f, fs.readFileSync(content + '/art/' + f), { create: true })
console.log('js-ipfs desert', root.toString(), 'version', fs.readFileSync(repo + '/version', 'utf8'))
await ipfs.stop()
