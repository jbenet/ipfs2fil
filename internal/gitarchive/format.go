package gitarchive

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"strconv"

	"github.com/pjbgf/sha1cd"
)

// ErrCollision means an object's SHA-1 shows the pattern of a known collision
// attack (SHA-1DC, the check git itself runs). Archiving and restore stop on it.
var ErrCollision = errors.New("SHA-1 collision attack detected")

// Format is a repo's object format: the hash git names objects with.
type Format struct {
	Name string // "sha1" or "sha256", as git spells it
	Size int    // hash length in bytes
}

var (
	SHA1   = Format{"sha1", 20}
	SHA256 = Format{"sha256", 32}
)

// ParseFormat reads git's name for an object format.
func ParseFormat(s string) (Format, error) {
	switch s {
	case "", "sha1":
		return SHA1, nil
	case "sha256":
		return SHA256, nil
	}
	return Format{}, fmt.Errorf("unknown git object format %q", s)
}

func (f Format) hasher() hash.Hash {
	if f.Size == 32 {
		return sha256.New()
	}
	return sha1cd.New()
}

// sum finishes h, reporting ErrCollision for a SHA-1 that shows an attack.
func (f Format) sum(h hash.Hash) ([]byte, error) {
	if cr, ok := h.(interface {
		CollisionResistantSum([]byte) ([]byte, bool)
	}); ok {
		s, col := cr.CollisionResistantSum(nil)
		if col {
			return s, fmt.Errorf("%w in object %x", ErrCollision, s)
		}
		return s, nil
	}
	return h.Sum(nil), nil
}

// objectHeader is what git hashes before an object's bytes: "blob 12\x00".
func objectHeader(typ int, size int64) []byte {
	b := make([]byte, 0, 32)
	b = append(b, typeNames[typ]...)
	b = append(b, ' ')
	b = strconv.AppendInt(b, size, 10)
	return append(b, 0)
}

// Object id for typ and data, checked with SHA-1DC for sha1 repos.
func (f Format) objectID(typ int, data []byte) ([]byte, error) {
	h := f.hasher()
	h.Write(objectHeader(typ, int64(len(data))))
	h.Write(data)
	return f.sum(h)
}

// Pack object types.
const (
	objCommit   = 1
	objTree     = 2
	objBlob     = 3
	objTag      = 4
	objOfsDelta = 6
	objRefDelta = 7
)

var typeNames = [8]string{objCommit: "commit", objTree: "tree", objBlob: "blob", objTag: "tag"}

// TypeName is git's name for a pack object type: commit, tree, blob, tag.
func TypeName(t int) string {
	if t < 0 || t >= len(typeNames) {
		return ""
	}
	return typeNames[t]
}
