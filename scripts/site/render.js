// Renders an asciicast to out.mp4, out.gif and last.png in a terminal window.
//   node render.js in.cast outdir [title] [maxIdleSeconds] [warpFromSeconds] [warpFactor]
// Gaps are capped at maxIdle; from warpFrom on, time runs warpFactor times faster.
// Needs playwright, ffmpeg and @xterm/xterm (npm i @xterm/xterm@5.5.0).
const { chromium } = require('playwright');
const fs = require('fs'), path = require('path'), { execFileSync } = require('child_process');
const [castFile, outDir, title = 'ipfs2fil', maxIdleS = '1.6', warpFromS = '1e9', warpS = '1'] = process.argv.slice(2);
const warpFrom = parseFloat(warpFromS), warp = parseFloat(warpS);
const lines = fs.readFileSync(castFile, 'utf8').trim().split('\n');
const hdr = JSON.parse(lines[0]); let ev = lines.slice(1).map(JSON.parse);
// compress idle gaps
const maxIdle = parseFloat(maxIdleS); let t = 0, prev = 0;
ev = ev.map(([tt, k, d]) => { let gap = tt - prev; prev = tt; t += Math.min(tt >= warpFrom ? gap / warp : gap, maxIdle); return [t, d]; });
const fps = 15, dt = 1 / fps;
fs.rmSync(outDir, { recursive: true, force: true }); fs.mkdirSync(outDir + '/f', { recursive: true });
const xtermJs = fs.readFileSync(require.resolve('@xterm/xterm/lib/xterm.js'), 'utf8');
const xtermCss = fs.readFileSync(require.resolve('@xterm/xterm/css/xterm.css'), 'utf8');
const html = `<!doctype html><style>${xtermCss}
body{margin:0;background:transparent;padding:0}
#win{display:inline-block;background:#0f1419;border-radius:10px;overflow:hidden;box-shadow:0 0 0 1px #2a3340}
#bar{height:34px;background:#1a212b;display:flex;align-items:center;padding:0 14px;gap:8px;position:relative}
#bar i{width:12px;height:12px;border-radius:50%;display:block}
#bar span{position:absolute;left:0;right:0;text-align:center;color:#8b97a6;font:13px -apple-system,Helvetica,Arial,sans-serif}
#t{padding:14px 16px 16px}
.xterm .xterm-viewport{background:#0f1419!important;overflow:hidden!important}
</style><div id=win><div id=bar><i style="background:#ff5f57"></i><i style="background:#febc2e"></i><i style="background:#28c840"></i><span>${title}</span></div><div id=t></div></div>
<script>${xtermJs}</script><script>
const term = new Terminal({cols:${hdr.width},rows:${hdr.height},fontFamily:'JetBrains Mono',fontSize:15,lineHeight:1.18,cursorBlink:false,
 theme:{background:'#0f1419',foreground:'#d7dde5',cursor:'#d7dde5',black:'#1c2430',brightBlack:'#5c6773',green:'#7fd962',brightGreen:'#a6e98f',blue:'#59c2ff',brightBlue:'#7fd0ff',yellow:'#ffb454',red:'#f07178',cyan:'#95e6cb',magenta:'#d2a6ff'}});
term.open(document.getElementById('t'));
window.w = s => new Promise(r => term.write(s, r));
</script>`;
(async () => {
  const b = await chromium.launch(); const p = await b.newPage({ deviceScaleFactor: 2 });
  await p.setContent(html); await p.evaluate(() => document.fonts.ready); await p.waitForTimeout(300);
  const el = await p.$('#win');
  const end = ev.length ? ev[ev.length - 1][0] + 2.5 : 1;
  let i = 0, n = 0; const list = [];
  for (let ft = 0; ft < end; ft += dt) {
    let chunk = '';
    while (i < ev.length && ev[i][0] <= ft) chunk += ev[i++][1];
    if (chunk || n === 0) {
      if (chunk) await p.evaluate(s => w(s), chunk);
      const f = `f/${String(n).padStart(5, '0')}.png`;
      await el.screenshot({ path: path.join(outDir, f) });
      list.push([f, dt]); n++;
    } else list[list.length - 1][1] += dt;
  }
  await b.close();
  fs.writeFileSync(path.join(outDir, 'list.txt'), list.map(([f, d]) => `file '${f}'\nduration ${d.toFixed(4)}`).join('\n') + `\nfile '${list[list.length - 1][0]}'\n`);
  fs.copyFileSync(path.join(outDir, list[list.length - 1][0]), path.join(outDir, 'last.png'));
  const run = (...a) => execFileSync('ffmpeg', ['-loglevel', 'error', '-y', ...a], { cwd: outDir, stdio: 'inherit' });
  run('-f', 'concat', '-i', 'list.txt', '-vf', 'fps=15,pad=ceil(iw/2)*2:ceil(ih/2)*2,format=yuv420p', '-c:v', 'libx264', '-crf', '20', '-movflags', '+faststart', 'out.mp4');
  run('-f', 'concat', '-i', 'list.txt', '-vf', 'fps=10,scale=iw/2:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=96:stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle', 'out.gif');
  console.log('frames', n, 'duration', end.toFixed(1));
})();
