A no-cgo stand-in for github.com/supranational/blst, wired in with a `replace`
in the top-level go.mod.

synapse-go imports blst for BLS signing, and blst needs cgo, but ipfs2fil is
built with CGO_ENABLED=0 for every kubo platform. ipfs2fil only signs with
secp256k1 (0x / f1) keys, so BLS is not needed. `SecretKey.Deserialize`
always fails here, which makes synapse-go reject any BLS key before anything
else in this package is reached. ipfs2fil itself refuses BLS keys earlier with
a plain message (see internal/store/foc/wallet.go).
