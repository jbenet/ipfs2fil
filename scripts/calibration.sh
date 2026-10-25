#!/bin/sh
# Stores 3 small pieces as 3 copies on Filecoin Calibration through the FOC
# store, re-runs to check nothing uploads twice, reads status from chain and
# verifies every piece. Needs network access to the RPC (glif by default).
#
# Needs a secp256k1 wallet on Calibration with tFIL for gas and a few USDFC,
# both from the Calibration faucets.
#
# Usage:
#   IPFS2FIL_KEY_FILE=~/calib.key scripts/calibration.sh     # dry: prints the funding plan
#   IPFS2FIL_KEY_FILE=~/calib.key IPFS2FIL_CALIBRATION_FUND=1 scripts/calibration.sh
#
# Optional: IPFS2FIL_RPC (default glif calibration), IPFS2FIL_CALIBRATION_ARCHIVE
# (data set key; the default is fixed so re-runs reuse the same data sets).
set -eu
if [ -z "${IPFS2FIL_KEY_FILE:-}${IPFS2FIL_PRIVATE_KEY:-}" ]; then
	echo "set IPFS2FIL_KEY_FILE or IPFS2FIL_PRIVATE_KEY to a Calibration wallet key" >&2
	exit 2
fi
cd "$(dirname "$0")/.."
exec go test -tags calibration -run TestCalibration -count=1 -timeout 50m -v ./internal/store/foc/
