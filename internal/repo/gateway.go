package repo

import (
	"context"
	"os"
	"path/filepath"

	"github.com/ipfs/go-cid"

	"github.com/jbenet/ipfs2fil/internal/dagfetch"
)

// Gateways are the trustless IPFS gateways the readers fetch content from
// by CID, tried in order. $IPFS2FIL_GATEWAYS, comma-separated, replaces them.
var Gateways = dagfetch.DefaultGateways

// DAGDir is where DAGs fetched from gateways are kept, one CAR per root
// CID (<cid>.car), shared by every reader that fetches by CID. The CLI sets
// it under its home directory.
var DAGDir = filepath.Join(os.TempDir(), "ipfs2fil-dags")

// ErrIncomplete is a DAG a gateway did not send all of.
var ErrIncomplete = dagfetch.ErrIncomplete

// fetcher fetches with this run's settings.
func fetcher() *dagfetch.Fetcher {
	return &dagfetch.Fetcher{HTTP: HTTPClient, Gateways: Gateways, Dir: DAGDir}
}

// dagFile is where the DAG under root is kept.
func dagFile(root cid.Cid) string { return fetcher().File(root) }

// fetchDAG downloads the whole DAG under root into dest (see
// dagfetch.Fetcher.FetchTo).
func fetchDAG(ctx context.Context, root cid.Cid, dest string) error {
	return fetcher().FetchTo(ctx, root, dest)
}
