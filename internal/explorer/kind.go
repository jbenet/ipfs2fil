package explorer

import (
	"path"
	"strings"
)

// KindOf guesses an Entry.Kind from a file name: "folder" for directories,
// otherwise by extension, and "other" when the extension says nothing.
func KindOf(name string, isDir bool) string {
	if isDir {
		return "folder"
	}
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	if k, ok := kindByExt[ext]; ok {
		return k
	}
	return "other"
}

var kindByExt = map[string]string{}

func init() {
	for kind, exts := range map[string]string{
		"image":   "jpg jpeg png gif webp avif heic heif bmp tif tiff svg ico raw cr2 nef arw dng psd jxl",
		"video":   "mp4 m4v mov webm mkv avi wmv flv mpg mpeg ts m2ts 3gp ogv",
		"audio":   "mp3 m4a aac flac wav ogg oga opus wma aiff aif mid midi",
		"doc":     "pdf doc docx odt rtf xls xlsx ods ppt pptx odp epub mobi djvu pages key numbers",
		"page":    "html htm xhtml mhtml",
		"text":    "txt md markdown rst org log csv tsv srt vtt tex nfo",
		"code":    "js mjs cjs ts tsx jsx css scss json yaml yml toml xml go rs py rb java c h cc cpp hpp cs swift kt sh bash zsh ps1 php pl lua sql wasm ipynb proto",
		"archive": "zip tar gz tgz bz2 xz zst 7z rar car lz4 deb rpm apk jar",
		"disk":    "iso img dmg vhd vhdx vmdk qcow2",
	} {
		for _, e := range strings.Fields(exts) {
			kindByExt[e] = kind
		}
	}
}
