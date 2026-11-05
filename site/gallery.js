// The explorer gallery: a screenshot for every row of the explorer's sidebar
// and for every pill. Moving the mouse level with a sidebar row, on the
// picture or anywhere across the page, shows that row's screenshot; a pill
// click shows its own; after 2 s with no mouse, the pills take turns.
(() => {
  const BASE = '127.0.0.1:8484/all/explorer/';
  const I = (d, c) => `<svg width="18" height="18" viewBox="0 0 16 16" fill="none" stroke="${c || 'currentColor'}" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round" aria-hidden="true"><path d="${d}"></path></svg>`;
  const ICON = {
    overview: I('M2 13.5h12 M4 13.5V8 M8 13.5V3 M12 13.5V6', '#1B5FD6'),
    pieces: I('M2 2h5v5h-5z M9 2h5v5h-5z M2 9h5v5h-5z M9 9h5v5h-5z', '#1B5FD6'),
    ipfs: I('M8 1.5l5.6 3.2v6.6L8 14.5l-5.6-3.2V4.7z M2.4 4.7L8 8l5.6-3.3 M8 8v6.5', '#3B9EA6'),
    car: I('M2 4.5l6-3 6 3v7l-6 3-6-3z M2 4.5l6 3 6-3 M8 7.5v7', '#047857'),
    image: I('M2 2.5h12v11h-12z M2 11l3.5-3.5 3 3 2-2 3.5 3.5', '#2F6FB0'),
    video: I('M1.5 3.5h9v9h-9z M10.5 7l4-2.5v7l-4-2.5', '#0B3B8C'),
    audio: I('M6 12V3.5l7-1.5v8.5 M6 12a2 2 0 1 1-4 0a2 2 0 0 1 4 0z M13 10.5a2 2 0 1 1-4 0a2 2 0 0 1 4 0z', '#6D28D9'),
    doc: I('M4 1.5h5l3 3v10h-8z M9 1.5v3h3 M5.5 9h5', '#B91C1C'),
    code: I('M5.5 4.5l-3.5 3.5 3.5 3.5 M10.5 4.5l3.5 3.5-3.5 3.5', '#B45309'),
    site: I('M8 1.5a6.5 6.5 0 1 0 0 13a6.5 6.5 0 0 0 0-13z M1.5 8h13 M8 1.5c-2 1.8-2.8 4-2.8 6.5s.8 4.7 2.8 6.5 M8 1.5c2 1.8 2.8 4 2.8 6.5s-.8 4.7-2.8 6.5', '#0369A1'),
    app: I('M3 5h10a2 2 0 0 1 2 2v3a2 2 0 0 1-2 2h-1.5l-1.5-1.5h-4l-1.5 1.5h-1.5a2 2 0 0 1-2-2v-3a2 2 0 0 1 2-2z M4.5 8.5h2.5 M5.75 7.25v2.5 M11 8h.01 M12.3 9.3h.01', '#C2410C'),
    zip: I('M3 1.5h10v13h-10z M8 1.5v1.5 M8 4.5v1.5 M7 7.5h2v2.5h-2z', '#047857'),
    bsky: '<svg width="18" height="18" viewBox="0 0 600 530" aria-hidden="true"><path fill="#1185FE" d="m135.72 44.03c66.496 49.921 138.02 151.14 164.28 205.46 26.262-54.316 97.782-155.54 164.28-205.46 47.98-36.021 125.72-63.892 125.72 24.795 0 17.712-10.155 148.79-16.111 170.07-20.703 73.984-96.144 92.854-163.25 81.433 117.3 19.964 147.14 86.092 82.697 152.22-122.39 125.59-175.91-31.511-189.63-71.766-2.514-7.3797-3.6904-10.832-3.7077-7.8964-0.0174-2.9357-1.1937 0.51669-3.7077 7.8964-13.714 40.255-67.233 197.36-189.63 71.766-64.444-66.128-34.605-132.26 82.697-152.22-67.108 11.421-142.55-7.4491-163.25-81.433-5.9562-21.282-16.111-152.36-16.111-170.07 0-88.687 77.742-60.816 125.72-24.795z"/></svg>',
    git: '<svg width="18" height="18" viewBox="0 0 16 16" fill="none" stroke="#F05133" stroke-width="1.5" aria-hidden="true"><circle cx="4" cy="3.5" r="1.5"></circle><circle cx="4" cy="12.5" r="1.5"></circle><circle cx="12" cy="6" r="1.5"></circle><path d="M4 5v6M12 7.5c0 2.5-3 2.5-6.5 4"></path></svg>',
    lists: I('M5.5 4h8.5 M5.5 8h8.5 M5.5 12h8.5 M2 4h.01 M2 8h.01 M2 12h.01', '#4B5260'),
    search: '<svg width="18" height="18" viewBox="0 0 16 16" fill="none" stroke="#4B5260" stroke-width="1.5" aria-hidden="true"><circle cx="7" cy="7" r="4.5"></circle><path d="M10.5 10.5l3.5 3.5"></path></svg>',
    keys: I('M1.5 4h13v8h-13z M4 6.5h.01 M6.5 6.5h.01 M9 6.5h.01 M11.5 6.5h.01 M4.5 9.5h7', '#4B5260')
  };
  // The pills, in groups, in the order they take turns.
  const PILLS = [
    ['Archive', [['overview', 'Overview', 'overview'], ['pieces', 'Pieces', 'pieces'], ['ada', 'IPFS repo', 'ipfs'], ['art.car', 'CAR files', 'car']]],
    ['Content', [['photos', 'Photos', 'image'], ['videos', 'Video', 'video'], ['audio', 'Audio', 'audio'], ['documents', 'Documents', 'doc'], ['code', 'Code', 'code'], ['sites', 'Web sites', 'site'], ['webapps', 'Web apps', 'app'], ['zip', 'ZIP files', 'zip']]],
    ['Viewers', [['bluesky', 'Bluesky', 'bsky'], ['git', 'Git', 'git'], ['lists', 'Lists', 'lists']]],
    ['Explorer', [['search', 'Search', 'search'], ['shortcuts', 'Shortcuts', 'keys']]]
  ];
  // Each shot: [address after the explorer, caption, pill it lights when it is not a pill's own]
  const SHOTS = {
    overview: ['index.html', 'The overview: what takes the space, by part of the repo and by file type, and each repo with what it shares with the others.'],
    pieces: ['pieces.html', 'Every piece, what it holds, and which providers store it. Pieces from a new snapshot are marked.'],
    everything: ['browse.html?deep=1', 'Everything: every file of every repo in one list, with its folder, kind, size and the piece that holds it.'],
    photos: ['browse.html?kind=image', 'Photos from every repo, largest first. Pick one and it fills the preview, with its CID, gateway links, its piece and how many copies exist.'],
    videos: ['browse.html?kind=video', 'Video plays straight from the archive, read from the pieces on your machine or from a gateway.'],
    audio: ['browse.html?kind=audio', 'Audio plays in place: music, recordings and the sounds inside the games.'],
    posts: ['browse.html?kind=post', 'Posts from the archived Bluesky accounts, as cards, with their threads and media.'],
    documents: ['browse.html?kind=doc', 'Documents open in place: PDFs, EPUB books with their covers, Office files and notebooks.'],
    sites: ['browse.html?kind=site', 'Web sites open as sites, with links that work inside the archive: the ones a site list names, and any folder with an index.html.'],
    pages: ['browse.html?kind=page', 'Web pages: every HTML file, previewed as a page.'],
    text: ['browse.html?kind=text', 'Text and Markdown, rendered or as plain text.'],
    code: ['browse.html?kind=code', 'Code is highlighted, and JavaScript and Python run sandboxed in the page with Run (Python on Pyodide).'],
    records: ['browse.html?kind=record', 'Records: the IPLD inside the archive, such as Bluesky records and build records, as JSON.'],
    accounts: ['browse.html?kind=account', 'Accounts: every Bluesky account the archive holds, from a single account or a whole PDS.'],
    repositories: ['browse.html?kind=repo', 'Repositories: the git repos, PDSes, site lists and pinning services the archive holds.'],
    archives: ['browse.html?kind=archive', 'Archives: tar, tar.gz and zip files, which open like folders.', 'zip'],
    other: ['browse.html?kind=other', 'Everything else, with a hex view for what has no viewer.'],
    bluesky: ['browse.html?app=bsky&a=0', 'The Bluesky viewer shows an archived account as Bluesky does: profile, posts, replies, media and threads, and a summary of every record.'],
    git: ['browse.html?app=git&path=ipfs/go-cid', 'The Git viewer reads the stored git packs in the browser: the files at any commit, the log, branches, tags and snapshots.'],
    repos: ['browse.html', 'All repos: each repo this machine archived, side by side.'],
    ada: ['browse.html?p=ada', 'A kubo repo, as archived: its pins, its MFS files, and the orphan blocks no pin reaches.'],
    'old-laptop': ['browse.html?p=old-laptop', 'A second kubo repo, from an old laptop: what it shares with the first is stored once.'],
    'bsky-bsky.app': ['browse.html?p=bsky-bsky.app', 'The @bsky.app Bluesky account: its repo of records and its blobs.'],
    'js-ipfs': ['browse.html?p=js-ipfs', 'A js-ipfs repo, made by js-ipfs itself.'],
    'helia-photo-app': ['browse.html?p=helia-photo-app', 'A Helia store from an app, made by Helia itself.'],
    'art.car': ['browse.html?p=art.car', 'A CAR file of art, video, texts and PDFs: each of its roots becomes a folder.'],
    'dev-bundle.car': ['browse.html?p=dev-bundle.car', 'A CAR of dev assets, with three roots: a folder, a dag-cbor build record and a dag-json release.'],
    multiformats: ['browse.html?p=multiformats', 'Five multiformats spec repos, as one set of git archives.'],
    'go-cid': ['browse.html?p=go-cid', 'ipfs/go-cid, archived from its URL with all its history.'],
    pds: ['browse.html?p=pds', 'A whole PDS: every account on it.'],
    'sites-ipfs-websites': ['browse.html?p=sites-ipfs-websites', 'A site list: ipfs.tech, specs.ipfs.tech and docs.ipfs.tech, each resolved and archived.'],
    'pinning-service': ['browse.html?p=pinning-service', 'The pins on a pinning service, with their names.'],
    zip: ['browse.html?p=dev-bundle.car/…/docs.zip', 'Inside a zip file, in the browser: its files list and open like any folder.'],
    webapps: ['browse.html?kind=site', 'Web apps run from the archive. This is Clumsy Bird, an HTML game, playing from the archive.'],
    lists: ['browse.html?app=bsky', 'Bluesky accounts and git repos show as cards or as a sortable list, with their numbers in columns. Press v to switch.'],
    search: ['browse.html?q=apollo', 'Search names and CIDs across every repo, scoped to a folder and below.'],
    shortcuts: ['browse.html?kind=image', 'Everything works from the keyboard. Press ? on any page.']
  };
  const ORDER = PILLS.flatMap((g) => g[1].map((p) => p[0]));
  const ROWS = window.GALLERY_ROWS || [];
  const W = 1440, H = 1140; // the shots' size in CSS pixels
  const root = document.getElementById('gallery');
  if (!root) return;
  const pills = root.querySelector('.pills'), img = root.querySelector('.gal-img'), hl = root.querySelector('.gal-hl'),
    url = root.querySelector('.gal-url'), cap = root.querySelector('.gal-cap'), stage = root.querySelector('.gal-stage');
  pills.innerHTML = PILLS.map((g) => `<div class="pgroup" role="group" aria-label="${g[0]}"><span class="pg-h">${g[0]}</span><div class="pg-row">${g[1].map((p) => `<button type="button" class="pill" data-shot="${p[0]}" aria-pressed="false">${ICON[p[2]]}<span>${p[1]}</span></button>`).join('')}</div></div>`).join('');
  const src = (id) => 'media/gallery/' + id + '.webp';
  let cur = '';
  function show(id) {
    if (id === cur || !SHOTS[id]) return;
    cur = id;
    img.src = src(id);
    img.alt = 'The explorer: ' + SHOTS[id][1];
    url.textContent = BASE + SHOTS[id][0];
    cap.textContent = SHOTS[id][1];
    const pill = ORDER.includes(id) ? id : SHOTS[id][2] || '';
    pills.querySelectorAll('.pill').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.shot === pill)));
    const r = ROWS.find((x) => x.id === id);
    hl.hidden = !r;
    if (r) { hl.style.top = (r.top / H * 100) + '%'; hl.style.height = (r.h / H * 100) + '%'; hl.style.left = (8 / W * 100) + '%'; hl.style.width = (195 / W * 100) + '%'; }
  }
  // the next pill after the current shot (a row's shot goes on from its pill, or the start)
  const next = () => { const p = ORDER.includes(cur) ? cur : SHOTS[cur] && SHOTS[cur][2]; return ORDER[(ORDER.indexOf(p) + 1) % ORDER.length]; };

  let active = 0, turned = 0, onScreen = false;
  const touch = () => { active = Date.now(); };
  pills.addEventListener('click', (ev) => { const b = ev.target.closest('.pill'); if (b) { touch(); show(b.dataset.shot); } });
  // level with a sidebar row, anywhere across the page
  document.addEventListener('mousemove', (ev) => {
    touch();
    if (!onScreen) return;
    const box = img.getBoundingClientRect();
    if (!box.height || ev.clientY < box.top || ev.clientY > box.bottom) return;
    const y = (ev.clientY - box.top) / box.height * H;
    const r = ROWS.find((x) => y >= x.top && y < x.top + x.h);
    if (r) show(r.id);
  }, { passive: true });
  ['keydown', 'touchstart', 'scroll'].forEach((e) => addEventListener(e, touch, { passive: true }));
  // every picture, once the gallery is near
  let loaded = false;
  const preload = () => { if (loaded) return; loaded = true; Object.keys(SHOTS).forEach((id, i) => setTimeout(() => { new Image().src = src(id); }, 60 * i)); };
  new IntersectionObserver((es) => { onScreen = es[0].isIntersecting; if (onScreen) preload(); }, { rootMargin: '200px' }).observe(stage);
  setInterval(() => {
    const now = Date.now();
    if (!onScreen || document.hidden || now - active < 2000 || now - turned < 3200) return;
    turned = now;
    show(next());
  }, 250);
  show('photos');
})();
