package explorer

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	_ "image/gif" // decoders for thumbnails
	"image/jpeg"
	_ "image/png"
	"io"
	"sync"
)

// Thumbnails, so a grid of photos does not load each one whole. explore
// makes them from JPEG, PNG and GIF files; other images are sent as they are
// and the browser scales them.

const maxThumbSource = 64 << 20 // bytes; bigger files are not decoded

type thumbs struct {
	mu    sync.Mutex
	cache map[string][]byte
	bytes int
	slots chan struct{}
}

func newThumbs() *thumbs {
	return &thumbs{cache: map[string][]byte{}, slots: make(chan struct{}, 4)}
}

func (t *thumbs) get(key string) ([]byte, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.cache[key]
	return b, ok
}

func (t *thumbs) put(key string, b []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.bytes+len(b) > 64<<20 {
		t.cache, t.bytes = map[string][]byte{}, 0
	}
	t.cache[key] = b
	t.bytes += len(b)
}

// thumbnail decodes r and returns a JPEG at most w pixels wide and high, or
// nil when r is not an image it can read.
func (t *thumbs) thumbnail(r io.Reader, w int) []byte {
	t.slots <- struct{}{}
	defer func() { <-t.slots }()
	data, err := io.ReadAll(io.LimitReader(r, maxThumbSource+1))
	if err != nil || len(data) > maxThumbSource {
		return nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width*cfg.Height > 100e6 {
		return nil
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	dst := scale(src, w, orientation(data))
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil
	}
	return out.Bytes()
}

// scale shrinks src to fit in w×w, turned upright by its EXIF orientation,
// averaging a few samples per pixel.
func scale(src image.Image, w, orient int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if orient >= 5 { // turned a quarter: the picture is sh wide
		sw, sh = sh, sw
	}
	dw, dh := sw, sh
	if dw > w || dh > w {
		if sw >= sh {
			dw, dh = w, max(1, sh*w/sw)
		} else {
			dw, dh = max(1, sw*w/sh), w
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	const n = 3 // samples per side
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var r, g, bl, a uint32
			for j := 0; j < n; j++ {
				for i := 0; i < n; i++ {
					// a point in the upright picture, then where it is in src
					ux := (x*n + i) * sw / (dw * n)
					uy := (y*n + j) * sh / (dh * n)
					sx, sy := upright(ux, uy, sw, sh, orient)
					cr, cg, cb, ca := src.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					r, g, bl, a = r+cr, g+cg, bl+cb, a+ca
				}
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / (n * n) >> 8), uint8(g / (n * n) >> 8), uint8(bl / (n * n) >> 8), uint8(a / (n * n) >> 8)})
		}
	}
	return dst
}

// upright maps (x, y) in the upright picture, w×h, to the stored pixel for
// EXIF orientation o.
func upright(x, y, w, h, o int) (int, int) {
	switch o {
	case 2:
		return w - 1 - x, y
	case 3:
		return w - 1 - x, h - 1 - y
	case 4:
		return x, h - 1 - y
	case 5:
		return y, x
	case 6:
		return y, w - 1 - x
	case 7:
		return h - 1 - y, w - 1 - x
	case 8:
		return h - 1 - y, x
	}
	return x, y
}

// orientation reads the EXIF orientation of a JPEG, or 1.
func orientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(b) && b[i] == 0xFF; {
		marker, n := b[i+1], int(binary.BigEndian.Uint16(b[i+2:]))
		if marker == 0xDA || n < 2 || i+2+n > len(b) {
			return 1
		}
		seg := b[i+4 : i+2+n]
		if marker == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return exifOrientation(seg[6:])
		}
		i += 2 + n
	}
	return 1
}

func exifOrientation(t []byte) int {
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	ifd := int(bo.Uint32(t[4:]))
	if ifd+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[ifd:]))
	for k := 0; k < n; k++ {
		e := ifd + 2 + k*12
		if e+12 > len(t) {
			break
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if o := int(bo.Uint16(t[e+8:])); o >= 1 && o <= 8 {
				return o
			}
		}
	}
	return 1
}
