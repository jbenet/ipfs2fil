// Writes the demo PDFs into papers/, run from the content folder content.py made.
const { chromium } = require('playwright');
const fs = require('fs');
const css = `body{font:14px/1.55 Georgia,serif;margin:56px 64px;color:#1b1b1b} h1{font:600 28px/1.2 Helvetica,Arial,sans-serif;margin:0 0 6px} .sub{color:#666;margin-bottom:28px;font-family:Helvetica,Arial} h2{font:600 17px Helvetica,Arial;margin-top:26px} img{width:100%;border-radius:6px;margin:10px 0} .cols{columns:2;column-gap:28px}`;
const lorem = (t) => `<p>${t}</p>`;
const docs = [
 ['papers/notes-on-content-addressing.pdf', `<h1>Notes on content addressing</h1><div class=sub>Field notes, vol. 3</div><div class=cols>
 ${lorem('A content address names data by what it is, not where it lives. Hash the bytes, and the hash becomes the name: anyone holding the same bytes can serve them, and anyone receiving them can check they got the right thing.')}
 <h2>Why it matters for archives</h2>${lorem('Location-based links rot when servers move or disappear. A content address keeps working as long as some copy exists anywhere, which turns preservation into a question of keeping enough copies alive.')}
 <h2>Merkle DAGs</h2>${lorem('Large files are split into blocks, and directories link to files by their addresses. The root address commits to the whole tree, so verifying the root verifies every byte beneath it.')}
 <h2>Pieces and proofs</h2>${lorem('Storage networks group blocks into larger pieces and prove, over time, that each piece is still held. The archive keeps a map from every block to the piece that holds it.')}
 ${lorem('With the map, the index and a viewer travel together with the data, so the archive explains itself to whoever opens it in ten years.')}</div>`],
 ['papers/fractal-gallery.pdf', `<h1>Fractal gallery</h1><div class=sub>Three renders of the Mandelbrot set</div>
 <img src="file://${process.cwd()}/art/mandelbrot.png" style="width:60%"><h2>Seahorse valley</h2><img src="file://${process.cwd()}/art/seahorse-valley.png" style="width:45%"> <img src="file://${process.cwd()}/art/elephants.png" style="width:45%">`],
 ['papers/trip-report-iceland-2024.pdf', `<h1>Trip report: Iceland 2024</h1><div class=sub>Nine days, one ring road</div>
 <img src="file://${process.cwd()}/photos/2024-iceland/IMG_4103.jpg"><div class=cols>${lorem('We drove the ring road counter-clockwise, starting with the south coast. The light changed every hour, and the photos show it.')}${lorem('Days four and five were the east fjords: slow roads, empty valleys, and the best weather of the trip.')}${lorem('The last two days were Reykjavik and the peninsula. Weather data for the year is in data/weather-reykjavik-2024.csv.')}</div>`],
];
(async () => {
  const b = await chromium.launch(); const p = await b.newPage();
  for (const [out, body] of docs) {
    fs.writeFileSync(process.cwd() + '/.tmp.html', `<!doctype html><style>${css}</style>${body}`);
    await p.goto('file://' + process.cwd() + '/.tmp.html'); await p.waitForTimeout(300);
    await p.pdf({ path: out, format: 'A4' });
  }
  await b.close();
})();
