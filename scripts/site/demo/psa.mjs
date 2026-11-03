// A small IPFS Pinning Service API server for the website demo's
// "pinning service" repo: it lists the pins in a JSON file and nothing else
// (GET /pins, newest first, with the API's status, limit and before):
//
//   node psa.mjs <pins.json> [port=2590]
//
// pins.json is [{"cid", "name", "created", "meta"?, "origins"?}, ...]. Any
// bearer token is accepted. Pinning services list pins but do not serve
// them; ipfs2fil fetches each pin's DAG from the gateways in
// $IPFS2FIL_GATEWAYS (make.sh runs a kubo node holding them).
import fs from 'node:fs'
import http from 'node:http'

const [file, port = '2590'] = process.argv.slice(2)
if (!file) { console.error('usage: node psa.mjs <pins.json> [port]'); process.exit(2) }
const pins = JSON.parse(fs.readFileSync(file, 'utf8'))
  .map((p, i) => ({ requestid: 'req-' + String(i + 1).padStart(4, '0'), status: 'pinned', created: p.created, pin: { cid: p.cid, name: p.name, origins: p.origins || [], meta: p.meta || {} }, delegates: [] }))
  .sort((a, b) => a.created < b.created ? 1 : -1)

http.createServer((req, res) => {
  const u = new URL(req.url, 'http://x')
  const send = (code, v) => { res.writeHead(code, { 'content-type': 'application/json' }); res.end(JSON.stringify(v)) }
  if (!/^Bearer \S+/.test(req.headers.authorization || '')) return send(401, { error: { reason: 'UNAUTHORIZED', details: 'no access token' } })
  if (req.method !== 'GET' || u.pathname !== '/pins') return send(404, { error: { reason: 'NOT_FOUND' } })
  const before = u.searchParams.get('before'), limit = Math.min(1000, +u.searchParams.get('limit') || 10)
  const status = (u.searchParams.get('status') || 'pinned').split(',')
  const all = pins.filter((p) => status.includes(p.status))
  send(200, { count: all.length, results: all.filter((p) => !before || p.created < before).slice(0, limit) })
}).listen(+port, '127.0.0.1', () => console.log(`pinning service at http://127.0.0.1:${port} with ${pins.length} pins`))
