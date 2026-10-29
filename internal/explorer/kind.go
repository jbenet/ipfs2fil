package explorer

import (
	"path"
	"strings"
	"unicode/utf8"
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

// Sniff guesses a file's kind from its first bytes (one block is plenty), for
// files whose name says nothing: pinned files are often named only by CID.
// It returns "other" when nothing matches.
func Sniff(b []byte) string {
	has := func(at int, sig string) bool { return len(b) >= at+len(sig) && string(b[at:at+len(sig)]) == sig }
	switch {
	case has(4, "ftyp") && len(b) >= 12:
		// ISO media: the brand says photo, sound or video
		switch brand := strings.TrimSpace(string(b[8:12])); brand {
		case "heic", "heix", "heim", "heis", "hevc", "mif1", "msf1", "avif", "avis":
			return "image"
		case "M4A", "M4B", "M4P", "F4A":
			return "audio"
		case "crx":
			return "image" // Canon raw
		}
		return "video"
	case has(0, "RIFF") && has(8, "WAVE"), has(0, "FORM") && (has(8, "AIFF") || has(8, "AIFC")),
		has(0, "ID3"), has(0, "fLaC"), has(0, "MThd"), has(0, "OggS"), has(0, "#!AMR"),
		len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0 && b[1]&0x06 != 0: // an MPEG audio frame
		return "audio"
	case has(0, "RIFF") && has(8, "AVI "), has(0, "\x1a\x45\xdf\xa3"), has(0, "FLV\x01"),
		has(0, "\x00\x00\x01\xba"), has(0, "\x00\x00\x01\xb3"), has(0, "\x30\x26\xb2\x75\x8e\x66\xcf\x11"),
		len(b) >= 189 && b[0] == 0x47 && b[188] == 0x47: // MPEG transport stream
		return "video"
	case has(0, "\xff\xd8\xff"), has(0, "\x89PNG\r\n\x1a\n"), has(0, "GIF87a"), has(0, "GIF89a"),
		has(0, "RIFF") && has(8, "WEBP"), has(0, "BM") && len(b) > 18 && b[15] == 0 && b[16] == 0 && b[17] == 0 && (b[14] == 12 || b[14] == 40 || b[14] == 108 || b[14] == 124), has(0, "II*\x00"), has(0, "MM\x00*"),
		has(0, "\x00\x00\x01\x00"), has(0, "8BPS"), has(0, "\xff\x0a"), has(0, "\x00\x00\x00\x0cJXL "):
		return "image"
	case has(0, "%PDF-"), has(0, "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"), has(0, "{\\rtf"), has(0, "AT&TFORM"):
		return "doc"
	case has(0, "PK\x03\x04"):
		return zipKind(b)
	case has(0, "\x1f\x8b"), has(0, "BZh"), has(0, "\xfd7zXZ\x00"), has(0, "7z\xbc\xaf\x27\x1c"),
		has(0, "Rar!\x1a\x07"), has(0, "\x28\xb5\x2f\xfd"), has(0, "\x04\x22\x4d\x18"), has(257, "ustar"):
		return "archive"
	case has(0, "koly"), has(0, "conectix"), has(0, "KDMV"), has(0, "QFI\xfb"), has(0x8001, "CD001"):
		return "disk"
	case has(0, "\x00asm"):
		return "code"
	}
	return sniffText(b)
}

// zipKind tells documents and ebooks, which are zip files inside, from plain
// zip archives, by the first entry's name.
func zipKind(b []byte) string {
	if len(b) < 30 {
		return "archive"
	}
	n := int(b[26]) | int(b[27])<<8
	if len(b) < 30+n {
		return "archive"
	}
	switch name := string(b[30 : 30+n]); {
	case name == "mimetype": // epub, odt, ods, odp
		return "doc"
	case strings.HasPrefix(name, "[Content_Types].xml"), strings.HasPrefix(name, "word/"),
		strings.HasPrefix(name, "xl/"), strings.HasPrefix(name, "ppt/"), strings.HasPrefix(name, "docProps/"), strings.HasPrefix(name, "_rels/"):
		return "doc"
	}
	return "archive"
}

// sniffText calls valid UTF-8 with no control bytes text, or a page or an
// image when it starts like HTML or SVG.
func sniffText(b []byte) string {
	if len(b) > 1024 {
		b = b[:1024]
	}
	s := strings.TrimLeft(string(b), "\ufeff \t\r\n")
	if s == "" {
		return "other"
	}
	// a block can end in the middle of a character
	for i := 0; i < 3 && len(s) > 0 && !utf8.ValidString(s); i++ {
		s = s[:len(s)-1]
	}
	if !utf8.ValidString(s) {
		return "other"
	}
	for _, r := range s {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' && r != '\f' {
			return "other"
		}
	}
	low := strings.ToLower(s)
	switch {
	case strings.HasPrefix(low, "<!doctype html"), strings.HasPrefix(low, "<html"), strings.HasPrefix(low, "<head"),
		strings.HasPrefix(low, "<!--") && strings.Contains(low, "<html"):
		return "page"
	case strings.HasPrefix(low, "<svg"), strings.HasPrefix(low, "<?xml") && strings.Contains(low, "<svg"):
		return "image"
	case strings.HasPrefix(low, "#!"), strings.HasPrefix(low, "package "), strings.HasPrefix(low, "<?php"):
		return "code"
	}
	return "text"
}
