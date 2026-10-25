// Package blst stands in for github.com/supranational/blst/bindings/go so
// ipfs2fil builds without cgo. BLS keys are not supported: Deserialize always
// returns nil, so no key can be loaded and nothing else here runs.
package blst

type (
	SecretKey struct{}
	P1Affine  struct{}
	P2Affine  struct{}
)

func (*SecretKey) Deserialize([]byte) *SecretKey            { return nil }
func (p *P1Affine) From(*SecretKey) *P1Affine               { return p }
func (*P1Affine) Compress() []byte                          { return nil }
func (*P2Affine) Sign(*SecretKey, []byte, []byte) *P2Affine { return nil }
func (*P2Affine) Compress() []byte                          { return nil }
