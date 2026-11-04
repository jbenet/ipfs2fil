// Runs a self-hosted Bluesky PDS (the reference @atproto/pds) and a PLC
// directory (@did-plc/server, in memory) on this machine, with a few made-up
// accounts and a copy of @bsky.app, for the website demo's "whole PDS" repo:
//
//   node pds.mjs <data dir> <demo content dir>
//
// It serves the PLC directory on :2582 and the PDS on :2583 until stopped.
// Point ipfs2fil at them with IPFS2FIL_PLC_DIRECTORY=http://localhost:2582
// and IPFS2FIL_HANDLE_RESOLVER=http://localhost:2583, then add
// pds:http://localhost:2583. The PLC directory lives in memory, so archive
// the PDS before stopping this.
import fs from 'node:fs'
import path from 'node:path'
import crypto from 'node:crypto'
import { PDS, envToCfg, envToSecrets, readEnv } from '@atproto/pds'
import * as plc from '@did-plc/server'
import { AtpAgent } from '@atproto/api'

const [dir, content] = process.argv.slice(2)
if (!dir || !content) { console.error('usage: node pds.mjs <data dir> <demo content dir>'); process.exit(2) }
fs.rmSync(dir, { recursive: true, force: true })
fs.mkdirSync(dir, { recursive: true })

const plcServer = plc.PlcServer.create({ db: plc.Database.mock(), port: 2582 })
await plcServer.start()
const hex = () => crypto.randomBytes(32).toString('hex')
Object.assign(process.env, {
  PDS_PORT: '2583', PDS_HOSTNAME: 'localhost', PDS_DEV_MODE: 'true',
  PDS_DATA_DIRECTORY: dir, PDS_BLOBSTORE_DISK_LOCATION: path.join(dir, 'blocks'),
  PDS_DID_PLC_URL: 'http://localhost:2582', PDS_SERVICE_HANDLE_DOMAINS: '.test',
  PDS_JWT_SECRET: hex(), PDS_ADMIN_PASSWORD: 'demo-admin', PDS_PLC_ROTATION_KEY_K256_PRIVATE_KEY_HEX: hex(),
  PDS_INVITE_REQUIRED: 'false', PDS_RATE_LIMITS_ENABLED: 'false', PDS_CRAWLERS: '',
  PDS_BSKY_APP_VIEW_URL: 'http://localhost:2584', PDS_BSKY_APP_VIEW_DID: 'did:example:none',
  LOG_ENABLED: 'false',
})
const env = readEnv()
const pds = await PDS.create(envToCfg(env), envToSecrets(env))
await pds.start()

const base = 'http://localhost:2583'
// Made-up accounts; posts are about the demo's own content.
const people = [
  { handle: 'ada.test', name: 'Ada', about: 'Photos, fractals and notes. My IPFS repos live on Filecoin now.' },
  { handle: 'gardenlog.test', name: 'Garden log', about: 'What grew this week, one photo at a time.' },
  { handle: 'cidlab.test', name: 'Lab notes', about: 'Small experiments with content addressing.' },
]
const img = (...p) => path.join(content, ...p)
const posts = {
  'ada.test': [
    ['Back from Iceland. Every photo is pinned in my IPFS repo, and the repo is archived on Filecoin.', img('photos', '2024-iceland')],
    ['A Julia set I rendered this morning.', img('pub', 'images', 'julia', 'julia-01.jpg')],
    ['Content addressing means the same photo is stored once, however many repos hold it.'],
    ['Webb sees further than ever.', img('pub', 'photos', 'nasa', 'webb')],
  ],
  'gardenlog.test': [
    ['Week 1: seeds in.'],
    ['Week 3: the first leaves.', img('art')],
    ['Truchet tiles for the garden path?', img('pub', 'images', 'truchet')],
  ],
  'cidlab.test': [
    ['A CID is a hash with a label saying which hash and which format.'],
    ['Flow fields, drawn from noise.', img('pub', 'images', 'flow-fields', 'flow-01.png')],
    ['Same bytes, same CID, on any machine.'],
  ],
}
// The first image in a file or folder, as JPEG or PNG.
function pick(p) {
  if (!p || !fs.existsSync(p)) return null
  if (fs.statSync(p).isFile()) return p
  const f = fs.readdirSync(p).sort().find(n => /\.(jpe?g|png)$/i.test(n))
  return f ? path.join(p, f) : null
}

const agents = {}
for (const p of people) {
  const a = new AtpAgent({ service: base })
  await a.createAccount({ handle: p.handle, email: `${p.handle.split('.')[0]}@example.com`, password: 'demo-password' })
  await a.upsertProfile(() => ({ displayName: p.name, description: p.about }))
  agents[p.handle] = a
}
const made = {}
let t = Date.parse('2026-10-20T09:00:00Z')
for (const p of people) {
  const a = agents[p.handle]
  made[p.handle] = []
  for (const [text, media] of posts[p.handle]) {
    const rec = { text, createdAt: new Date(t += 3 * 3600e3).toISOString() }
    const f = pick(media)
    if (f) {
      const { data } = await a.uploadBlob(fs.readFileSync(f), { encoding: f.endsWith('.png') ? 'image/png' : 'image/jpeg' })
      rec.embed = { $type: 'app.bsky.embed.images', images: [{ image: data.blob, alt: path.basename(f) }] }
    }
    made[p.handle].push(await a.post(rec))
  }
}
// Follows, likes and a reply thread, so the accounts point at each other.
const did = h => agents[h].session.did
for (const h of Object.keys(agents)) for (const o of Object.keys(agents)) if (h !== o) await agents[h].follow(did(o))
const first = made['ada.test'][2]
await agents['cidlab.test'].like(first.uri, first.cid)
const reply = await agents['cidlab.test'].post({ text: 'And the CID proves it is the same photo.', reply: { root: first, parent: first }, createdAt: new Date(t += 3600e3).toISOString() })
await agents['ada.test'].post({ text: 'Exactly.', reply: { root: first, parent: reply }, createdAt: new Date(t += 3600e3).toISOString() })
await agents['gardenlog.test'].repost(made['ada.test'][0].uri, made['ada.test'][0].cid)

// A copy of @bsky.app's profile and latest posts (not replies or videos), as @bsky-app.test (handles
// here must end in .test). Its images are uploaded unchanged, so they get the
// same CIDs as in the at://bsky.app archive and are stored once.
const BSKY = { did: 'did:plc:z72i7hdynmk6r22z27h6tvur', pds: 'https://puffball.us-east.host.bsky.network' }
const xrpc = async (m, q) => {
  const r = await fetch(BSKY.pds + '/xrpc/' + m + '?' + new URLSearchParams({ repo: BSKY.did, did: BSKY.did, ...q }))
  if (!r.ok) throw new Error(m + ': ' + r.status)
  return m.endsWith('getBlob') ? Buffer.from(await r.arrayBuffer()) : r.json()
}
const copyBlob = async (a, b) => b && (await a.uploadBlob(await xrpc('com.atproto.sync.getBlob', { cid: b.ref.$link }), { encoding: b.mimeType })).data.blob
try {
  const a = new AtpAgent({ service: base })
  await a.createAccount({ handle: 'bsky-app.test', email: 'bsky-app@example.com', password: 'demo-password' })
  const prof = (await xrpc('com.atproto.repo.getRecord', { collection: 'app.bsky.actor.profile', rkey: 'self' })).value
  const avatar = await copyBlob(a, prof.avatar), banner = await copyBlob(a, prof.banner)
  await a.upsertProfile(() => ({ displayName: prof.displayName, description: prof.description, ...(avatar && { avatar }), ...(banner && { banner }) }))
  const { records } = await xrpc('com.atproto.repo.listRecords', { collection: 'app.bsky.feed.post', limit: '100' })
  let n = 0
  for (const { value: v } of records.filter(r => !r.value.reply && r.value.embed?.$type !== 'app.bsky.embed.video').slice(0, 30).reverse()) {
    const rec = { text: v.text, facets: v.facets, langs: v.langs, createdAt: v.createdAt }
    const e = v.embed
    if (e?.$type === 'app.bsky.embed.images')
      rec.embed = { $type: e.$type, images: await Promise.all(e.images.map(async i => ({ ...i, image: await copyBlob(a, i.image) }))) }
    else if (e?.$type === 'app.bsky.embed.external')
      rec.embed = { $type: e.$type, external: { ...e.external, thumb: await copyBlob(a, e.external.thumb) || undefined } }
    await a.post(JSON.parse(JSON.stringify(rec)))
    n++
  }
  console.log('@bsky-app.test: ' + n + ' posts copied from @bsky.app')
} catch (err) { console.error('could not copy @bsky.app: ' + err.message) }

console.log('PDS at ' + base + ' with ' + people.map(p => '@' + p.handle + ' ' + did(p.handle)).join(', '))
console.log('Serving until stopped.')
