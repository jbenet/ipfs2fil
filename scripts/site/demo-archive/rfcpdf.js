// Prints the RFC pages fetched by fetch.sh to PDF: node rfcpdf.js <rfc html dir> <out dir>
const { chromium } = require('playwright');
const names = {
  1: 'host-software', 675: 'internet-transmission-control-program', 791: 'internet-protocol',
  1958: 'architectural-principles-of-the-internet', 2324: 'hyper-text-coffee-pot-control-protocol',
  2616: 'http-1.1', 8890: 'the-internet-is-for-end-users',
};
(async () => {
  const [dir, out] = process.argv.slice(2);
  const b = await chromium.launch(); const p = await b.newPage();
  for (const [n, name] of Object.entries(names)) {
    await p.goto(`file://${dir}/rfc${n}.html`);
    await p.pdf({ path: `${out}/rfc${n}-${name}.pdf`, format: 'Letter', margin: { top: '0.6in', bottom: '0.6in', left: '0.6in', right: '0.6in' } });
  }
  await b.close();
})();
