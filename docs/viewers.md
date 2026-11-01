# Viewers and thumbnails

How the explorer's Content page shows files and makes their thumbnails, and
where to add another kind of file.

## Viewers (in the browser)

`internal/explorer/assets/explorer.js` shows the kinds every browser handles
itself: photos, video, sound, web pages, PDFs (in a deck of frames) and EPUBs
(its own reader), and other files as hex.

The pane's Full screen button (or Shift-F) puts its header and stage on the
whole screen, so every viewer gets it for free; j and k keep moving through
the list there. Browsers with no element full screen (iPhone Safari) get the
pane laid over the window instead, closed with Esc or the button.

Everything else is in `internal/explorer/assets/viewers.js`, one viewer per
sort of file. A viewer is

```js
view({
  name: 'markdown',
  test: (e, x) => x === 'md',           // e: the entry, x: its lower-case extension
  async show(st, v) { st.innerHTML = '...'; },
});
```

`show` fills the preview's stage element. `v` brings the bytes (`v.head(n)`,
`v.all(max)`, `v.tail(n)`, `v.range(a, b)`), `v.still()` (is this file still
the one shown), `v.base` (where its folder is served, for relative links) and
the explorer's formatting helpers. The first viewer whose `test` says yes
shows the file; add yours above a more general one.

Viewers now: Markdown (rendered, with a switch to the source), CSV and TSV
tables, JSON (pretty printed and highlighted, or Exact as stored, or Plain;
a Bluesky record says its type and a post links to the Bluesky App viewer), code
and text (highlighted with line numbers, or Plain; Wrap and Copy for both,
and the choices are kept), Jupyter notebooks, fonts (a specimen), zip and
tar listings (a zip's directory is read from its end, so only that is
fetched), Word text, Excel sheets, PowerPoint slide text, and WebAssembly
imports and exports.

A `.gz` file on its own shows what it unpacks to, as that kind of file
(a log as text, an image as an image), with a switch to its compressed bytes
in hex.

## Archives as folders

A `.tar`, `.tar.gz`/`.tgz` or zip (and zip-based `.jar`, `.whl`, `.apk`,
`.cbz`, ...) opens like a folder: click its name, press Enter, or Open in
its preview. `arc` in viewers.js reads it in the browser (GNU and pax tar
headers, zip64, deflate through the browser's DecompressionStream) into blobs,
so its files list, preview, search ("Include subfolders") and download like
any other, in every view. Paths go on through the archive
(`browse.html?p=dist/app.tar.gz/app/README.md`), so they can be linked and
reloaded, and archives inside archives open the same way. `dirInfo` in
explorer.js falls through to `arc.dir` when the index has no such folder.
Nothing is unpacked at pack time and nothing changes on chain; archives up to
2 GiB open, and the last four stay in memory.

Highlighting is a small tokenizer per family of languages (`LANG` and `RX`
in viewers.js: C-like, `#` comments, `--` comments, JSON, XML and HTML) that
tells comments, strings, numbers, keywords and calls apart. Add an extension
to a family in `langs(...)`.

Code can run: JavaScript in a sandboxed frame with no access to the explorer
or the archive, and Python on Pyodide (CPython for WebAssembly), which the
frame downloads from cdn.jsdelivr.net only when you press Run. Add a language
in `RUN` and `harness` in viewers.js.

Web pages: `ipfs2fil explore` serves folders by path (`_i2f/path/<folder
CID>/<path>`), so a page's relative links to its pictures, styles and scripts
work. The explorer is at 127.0.0.1 and loads pages from localhost, which a
browser treats as another origin: pages run their scripts and keep their own
storage, and cannot reach the explorer. From a gateway pages come from
`../data/`, sandboxed. A static copy with files only by CID (the website's
demo) has no folders to load from, so the explorer finds the files a page
links in the index by path and puts them in (`pageDoc` in explorer.js):
stylesheets and scripts inline, pictures and fonts by their content URLs, and
a small script that sends the page's own fetch, XMLHttpRequest and media
requests to them too. Such a page gets storage that lasts while it is open.

## Thumbnails

`ipfs2fil explore` makes them (`internal/explorer/thumbnails.go`), one
thumbnailer per kind of file, tried in order:

| Files | How | Needs |
|---|---|---|
| JPEG, PNG, GIF | decoded and scaled in Go | |
| EPUB | its cover | |
| Word, Excel, PowerPoint, OpenDocument, Pages, Keynote, Numbers | the preview picture inside the file | |
| PDF | the first page | `pdftoppm` (poppler) or `mutool` (MuPDF) |
| Video | a telling frame from the first seconds | `ffmpeg` |
| Audio | the album art in the file | `ffmpeg` |
| On a Mac, most else (HEIC and raw photos, fonts, other documents) | Quick Look (`qlmanage`) | macOS |

A thumbnailer is a `make(ctx, file, width)` that returns a picture of any size,
which explore then scales. Add one to the `thumbnailers` list.

Thumbnails are kept on disk in the user's cache folder
(`~/Library/Caches/ipfs2fil/thumbs` on a Mac, `~/.cache/ipfs2fil/thumbs` on
Linux), so each is made once. Files with none are remembered too, unless a
program that might make one is missing (install it and they are tried again).

When explore cannot make a video's thumbnail (no ffmpeg), the page takes a
frame in the browser and sends it back, and explore keeps it. From a gateway
the browser takes video frames itself, and grid tiles of text and code show
their first lines. Thumbnails are not packed into the archive: they are a
cache, and the archive on Filecoin stays exactly the repo's blocks.

## Files only partly here

An orphan whose root block a repo had, but not the rest, is listed with its
full size but cannot be shown. explore checks every file's blocks before
sending it (`parts` in content.go, which reads only inner nodes), answers 404
with "Only N of the M parts...", and lists such files for the pages
(`POST _i2f/parts`), which mark them "partial" and say so in the preview.
