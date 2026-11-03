// Package atproto fetches and reads AT Protocol (Bluesky) repos. An atproto
// repo is IPLD: a signed commit over a Merkle search tree of dag-cbor
// records, with images and video as raw blobs, so ipfs2fil archives it like
// any other repo.
package atproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Identity is an account's DID, its handle (when the handle and the DID
// document agree), and where its repo lives.
type Identity struct {
	DID    string `json:"did"`
	Handle string `json:"handle,omitempty"`
	PDS    string `json:"pds"`
	// DIDDoc is the DID document as served. AuditLog is the PLC directory's
	// log of signed operations for did:plc accounts, which lets anyone check
	// the commit signature after the account or its server is gone.
	DIDDoc   json.RawMessage `json:"-"`
	AuditLog json.RawMessage `json:"-"`
}

// Name is how people know the account: @handle, else the DID.
func (id *Identity) Name() string {
	if id.Handle != "" {
		return "@" + id.Handle
	}
	return id.DID
}

// IsAccount says whether s names an account: at://handle, at://did:...,
// did:plc:..., did:web:..., or @handle.
func IsAccount(s string) bool {
	return strings.HasPrefix(s, "at://") || strings.HasPrefix(s, "did:plc:") || strings.HasPrefix(s, "did:web:") ||
		(strings.HasPrefix(s, "@") && strings.Contains(s, "."))
}

// Account returns the handle or DID s names, without at:// or @.
func Account(s string) string {
	s = strings.TrimPrefix(s, "at://")
	s = strings.TrimPrefix(s, "@")
	s, _, _ = strings.Cut(s, "/")
	return strings.ToLower(s)
}

// Endpoints the resolver uses, each a comma-separated list tried in order.
// Tests point them at local servers. $IPFS2FIL_PLC_DIRECTORY and
// $IPFS2FIL_HANDLE_RESOLVER put those of a private network (a PLC directory
// and PDS of your own) before the public ones.
var (
	PLCDirectory = envFirst("IPFS2FIL_PLC_DIRECTORY", "https://plc.directory")
	// HandleResolver answers com.atproto.identity.resolveHandle when neither
	// DNS nor the handle's own web server does.
	HandleResolver = envFirst("IPFS2FIL_HANDLE_RESOLVER", "https://public.api.bsky.app")
	// LookupTXT is net.LookupTXT, swapped in tests.
	LookupTXT = net.DefaultResolver.LookupTXT
	// WellKnown builds the https URL of a host's /.well-known path; tests
	// send it to a local server.
	WellKnown = func(host, path string) string { return "https://" + host + "/.well-known/" + path }
)

var ErrNotFound = errors.New("account not found")

// envFirst is the URLs in $name (comma-separated), then def.
func envFirst(name, def string) string {
	var out []string
	for _, v := range strings.Split(os.Getenv(name), ",") {
		if v = strings.TrimRight(strings.TrimSpace(v), "/"); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(append(out, def), ",")
}

// endpoints splits a list of endpoints like PLCDirectory.
func endpoints(list string) []string {
	var out []string
	for _, v := range strings.Split(list, ",") {
		if v = strings.TrimRight(strings.TrimSpace(v), "/"); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Resolve finds the account s names (see IsAccount) and its PDS.
func Resolve(ctx context.Context, c *http.Client, s string) (*Identity, error) {
	acct := Account(s)
	id := &Identity{}
	if strings.HasPrefix(acct, "did:") {
		id.DID = acct
	} else {
		did, err := resolveHandle(ctx, c, acct)
		if err != nil {
			return nil, fmt.Errorf("resolving @%s: %w", acct, err)
		}
		id.DID = did
	}
	var docURLs []string
	switch {
	case strings.HasPrefix(id.DID, "did:plc:"):
		for _, d := range endpoints(PLCDirectory) {
			docURLs = append(docURLs, d+"/"+id.DID)
		}
	case strings.HasPrefix(id.DID, "did:web:"):
		host, err := url.PathUnescape(strings.TrimPrefix(id.DID, "did:web:"))
		if err != nil || strings.Contains(host, ":") {
			return nil, fmt.Errorf("%s: only did:web hosts without paths are supported", id.DID)
		}
		docURLs = []string{WellKnown(host, "did.json")}
	default:
		return nil, fmt.Errorf("%s: not a did:plc or did:web", id.DID)
	}
	var doc []byte
	var err error
	plc := ""
	for _, u := range docURLs {
		if doc, err = getBytes(ctx, c, u, 1<<20); err == nil {
			plc = strings.TrimSuffix(u, "/"+id.DID)
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("fetching the DID document of %s: %w", id.DID, err)
	}
	var d struct {
		ID          string   `json:"id"`
		AlsoKnownAs []string `json:"alsoKnownAs"`
		Service     []struct {
			ID, Type, ServiceEndpoint string
		} `json:"service"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("the DID document of %s: %w", id.DID, err)
	}
	if d.ID != id.DID {
		return nil, fmt.Errorf("the DID document of %s names %s", id.DID, d.ID)
	}
	id.DIDDoc = doc
	for _, s := range d.Service {
		if strings.HasSuffix(s.ID, "#atproto_pds") && s.Type == "AtprotoPersonalDataServer" {
			id.PDS = strings.TrimSuffix(s.ServiceEndpoint, "/")
		}
	}
	if id.PDS == "" {
		return nil, fmt.Errorf("the DID document of %s names no PDS", id.DID)
	}
	// The handle counts only when the document claims it and it points back.
	for _, aka := range d.AlsoKnownAs {
		h, ok := strings.CutPrefix(aka, "at://")
		if !ok {
			continue
		}
		h = strings.ToLower(h)
		if h == acct {
			id.Handle = h
			break
		}
		if did, err := resolveHandle(ctx, c, h); err == nil && did == id.DID {
			id.Handle = h
			break
		}
	}
	if strings.HasPrefix(id.DID, "did:plc:") {
		if id.AuditLog, err = getBytes(ctx, c, plc+"/"+id.DID+"/log/audit", 16<<20); err != nil {
			return nil, fmt.Errorf("fetching the PLC log of %s: %w", id.DID, err)
		}
	}
	return id, nil
}

// resolveHandle turns a handle into a DID: DNS first, then the handle's own
// web server, then a public resolver.
func resolveHandle(ctx context.Context, c *http.Client, handle string) (string, error) {
	if txts, err := LookupTXT(ctx, "_atproto."+handle); err == nil {
		for _, t := range txts {
			if did, ok := strings.CutPrefix(t, "did="); ok && strings.HasPrefix(did, "did:") {
				return did, nil
			}
		}
	}
	if b, err := getBytes(ctx, c, WellKnown(handle, "atproto-did"), 2048); err == nil {
		if did := strings.TrimSpace(string(b)); strings.HasPrefix(did, "did:") {
			return did, nil
		}
	}
	err := ErrNotFound
	for _, r := range endpoints(HandleResolver) {
		var v struct{ DID string }
		if err = getJSON(ctx, c, r+"/xrpc/com.atproto.identity.resolveHandle?handle="+url.QueryEscape(handle), &v); err != nil {
			continue
		}
		if strings.HasPrefix(v.DID, "did:") {
			return v.DID, nil
		}
		err = ErrNotFound
	}
	return "", err
}

// xrpcError is the body of a failed XRPC call.
type xrpcError struct {
	Error, Message string
}

// get starts a GET whose whole answer must arrive within timeout.
func get(ctx context.Context, c *http.Client, u string, timeout time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	res, err := c.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		defer cancel()
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		var e xrpcError
		if json.Unmarshal(b, &e) == nil && (e.Error != "" || e.Message != "") {
			if res.StatusCode == http.StatusNotFound || e.Error == "RepoNotFound" || e.Error == "BlobNotFound" {
				return nil, fmt.Errorf("%w: %s %s", ErrNotFound, e.Error, e.Message)
			}
			return nil, fmt.Errorf("%s: %s %s", res.Status, e.Error, e.Message)
		}
		if res.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%s", res.Status)
	}
	res.Body = &cancelBody{res.Body, cancel}
	return res, nil
}

// cancelBody cancels the request's timeout once the body is closed.
type cancelBody struct {
	io.ReadCloser
	cancel func()
}

func (b *cancelBody) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}

func getBytes(ctx context.Context, c *http.Client, u string, limit int64) ([]byte, error) {
	res, err := get(ctx, c, u, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%s: answer over %d bytes", u, limit)
	}
	return b, err
}

func getJSON(ctx context.Context, c *http.Client, u string, v any) error {
	b, err := getBytes(ctx, c, u, 16<<20)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// IdentityFromDoc rebuilds an account's identity from its DID document, as
// archived, and its handle as known then (empty if it had none): what an
// export writes as identity.json.
func IdentityFromDoc(doc []byte, handle string) (*Identity, error) {
	var d struct {
		ID      string `json:"id"`
		Service []struct {
			ID, Type, ServiceEndpoint string
		} `json:"service"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("the DID document: %w", err)
	}
	id := &Identity{DID: d.ID, Handle: handle, DIDDoc: doc}
	for _, s := range d.Service {
		if strings.HasSuffix(s.ID, "#atproto_pds") && s.Type == "AtprotoPersonalDataServer" {
			id.PDS = strings.TrimSuffix(s.ServiceEndpoint, "/")
		}
	}
	if id.DID == "" {
		return nil, fmt.Errorf("the DID document names no DID")
	}
	return id, nil
}
