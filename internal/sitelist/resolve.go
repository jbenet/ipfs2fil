package sitelist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ipfs/boxo/ipns"
	"github.com/ipfs/go-cid"
)

// Resolver finds the root a site serves now.
type Resolver struct {
	HTTP *http.Client
	// LookupTXT is net.DefaultResolver.LookupTXT by default, and then,
	// when that finds no DNSLink, DNS over HTTPS (DoH).
	LookupTXT func(ctx context.Context, name string) ([]string, error)
	// DoH is a DNS-over-HTTPS JSON endpoint the default lookup falls back
	// to, for networks whose own DNS answers no TXT queries. Default
	// https://cloudflare-dns.com/dns-query; "off" turns it off.
	DoH string
	// Gateways answer IPNS record requests (?format=ipns-record).
	Gateways []string
	// ENSGateway is a format with one %s, the ENS name, giving the URL of an
	// HTTP gateway that serves ENS names and says what it serves in its
	// X-Ipfs-Path header. Default https://%s.limo (eth.limo).
	ENSGateway string
}

// Resolved is where a site's root came from.
type Resolved struct {
	Root cid.Cid
	// How names the steps that found it, such as "dnslink" or
	// "ens via eth.limo, then ipns".
	How string
}

// maxDepth bounds DNSLink and IPNS chains.
const maxDepth = 8

// Resolve finds the root s serves now.
func (r *Resolver) Resolve(ctx context.Context, s Site) (Resolved, error) {
	if s.Kind == KindIPFS {
		c, err := cid.Decode(s.Host)
		return Resolved{Root: c, How: "fixed"}, err
	}
	return r.resolve(ctx, s.Kind, s.Host, nil, 0)
}

func (r *Resolver) resolve(ctx context.Context, kind, host string, how []string, depth int) (Resolved, error) {
	if depth > maxDepth {
		return Resolved{}, fmt.Errorf("over %d steps of DNSLink and IPNS", maxDepth)
	}
	var value string
	var err error
	switch kind {
	case KindDNSLink, KindURL:
		if value, err = r.dnslink(ctx, host); err == nil {
			how = append(how, "dnslink")
		} else if v, herr := r.headerPath(ctx, "https://"+host+"/"); herr == nil {
			value, err, how = v, nil, append(how, "x-ipfs-path from https://"+host)
		} else {
			return Resolved{}, fmt.Errorf("%s: no DNSLink (%v), and its server sends no X-Ipfs-Path (%v)", host, err, herr)
		}
	case KindENS:
		ens := r.ENSGateway
		if ens == "" {
			ens = "https://%s.limo"
		}
		u := fmt.Sprintf(ens, host)
		if value, err = r.headerPath(ctx, u); err != nil {
			return Resolved{}, fmt.Errorf("%s: %w", host, err)
		}
		via := u
		if pu, err := url.Parse(u); err == nil {
			via = strings.TrimPrefix(pu.Host, strings.TrimSuffix(host, ".eth")+".") // yourname.eth.limo -> eth.limo
		}
		how = append(how, "ens via "+via)
	case KindIPNS:
		if value, err = r.ipnsRecord(ctx, host); err != nil {
			return Resolved{}, fmt.Errorf("/ipns/%s: %w", host, err)
		}
		how = append(how, "ipns")
	default:
		return Resolved{}, fmt.Errorf("cannot resolve a %s site", kind)
	}
	value = strings.TrimSuffix(value, "/")
	switch {
	case strings.HasPrefix(value, "/ipfs/"):
		id := strings.TrimPrefix(value, "/ipfs/")
		if strings.Contains(id, "/") {
			return Resolved{}, fmt.Errorf("%s points inside a DAG (%s); only whole roots are archived", host, value)
		}
		c, err := cid.Decode(id)
		if err != nil {
			return Resolved{}, fmt.Errorf("%s points at %s: %w", host, value, err)
		}
		return Resolved{Root: c, How: strings.Join(how, ", then ")}, nil
	case strings.HasPrefix(value, "/ipns/"):
		next := strings.TrimPrefix(value, "/ipns/")
		if strings.Contains(next, "/") {
			return Resolved{}, fmt.Errorf("%s points inside a DAG (%s); only whole roots are archived", host, value)
		}
		k := KindIPNS
		if strings.HasSuffix(next, ".eth") {
			k = KindENS
		} else if isDomain(next) {
			k = KindDNSLink
		}
		return r.resolve(ctx, k, next, how, depth+1)
	}
	return Resolved{}, fmt.Errorf("%s points at %q, not an /ipfs/ or /ipns/ path", host, value)
}

// dnslink reads the _dnslink TXT record of host.
func (r *Resolver) dnslink(ctx context.Context, host string) (string, error) {
	if r.LookupTXT != nil {
		return r.dnslinkWith(ctx, r.LookupTXT, host)
	}
	v, err := r.dnslinkWith(ctx, net.DefaultResolver.LookupTXT, host)
	if err == nil || r.DoH == "off" {
		return v, err
	}
	v, derr := r.dnslinkWith(ctx, r.dohTXT, host)
	if derr == nil {
		return v, nil
	}
	return "", fmt.Errorf("%v; over DoH: %v", err, derr)
}

func (r *Resolver) dnslinkWith(ctx context.Context, lookup func(context.Context, string) ([]string, error), host string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	txts, err := lookup(ctx, "_dnslink."+host)
	if err != nil {
		return "", err
	}
	var vals []string
	for _, t := range txts {
		if v, ok := strings.CutPrefix(strings.TrimSpace(t), "dnslink="); ok && (strings.HasPrefix(v, "/ipfs/") || strings.HasPrefix(v, "/ipns/")) {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return "", errors.New("no dnslink= TXT record")
	}
	sort.Strings(vals) // several records: the same choice every time
	return vals[0], nil
}

func (r *Resolver) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return http.DefaultClient
}

// headerPath asks u what it serves: the X-Ipfs-Path header that IPFS
// gateways and many IPFS-hosted sites send.
func (r *Resolver) headerPath(ctx context.Context, u string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return "", err
		}
		res, err := r.client().Do(req)
		if err != nil {
			return "", err
		}
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		if p := res.Header.Get("X-Ipfs-Path"); p != "" {
			return p, nil
		}
		if res.StatusCode != http.StatusMethodNotAllowed {
			return "", fmt.Errorf("%s answers %s with no X-Ipfs-Path", u, res.Status)
		}
	}
	return "", fmt.Errorf("%s sends no X-Ipfs-Path", u)
}

// ipnsRecord fetches the IPNS record for key from a gateway and checks its
// signature and validity, returning the path it names.
func (r *Resolver) ipnsRecord(ctx context.Context, key string) (string, error) {
	name, err := ipns.NameFromString(key)
	if err != nil {
		return "", err
	}
	var errs []error
	for _, gw := range r.Gateways {
		v, err := r.ipnsFrom(ctx, gw, name)
		if err == nil {
			return v, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", gw, err))
	}
	if len(errs) == 0 {
		return "", errors.New("no gateways to ask")
	}
	return "", errors.Join(errs...)
}

func (r *Resolver) ipnsFrom(ctx context.Context, gw string, name ipns.Name) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(gw, "/")+"/ipns/"+name.String()+"?format=ipns-record", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.ipfs.ipns-record")
	res, err := r.client().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s", res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, int64(ipns.MaxRecordSize)+1))
	if err != nil {
		return "", err
	}
	rec, err := ipns.UnmarshalRecord(b)
	if err != nil {
		return "", err
	}
	if err := ipns.ValidateWithName(rec, name); err != nil {
		return "", err
	}
	p, err := rec.Value()
	if err != nil {
		return "", err
	}
	return p.String(), nil
}

// dohTXT looks up the TXT records of name with DNS over HTTPS (the JSON
// API Cloudflare and Google serve), following CNAMEs as the server does.
func (r *Resolver) dohTXT(ctx context.Context, name string) ([]string, error) {
	ep := r.DoH
	if ep == "" {
		ep = "https://cloudflare-dns.com/dns-query"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep+"?name="+url.QueryEscape(name)+"&type=TXT", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")
	res, err := r.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", ep, res.Status)
	}
	var ans struct {
		Status int
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		}
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&ans); err != nil {
		return nil, err
	}
	if ans.Status != 0 {
		return nil, fmt.Errorf("%s: no such name (DNS status %d)", name, ans.Status)
	}
	var out []string
	for _, a := range ans.Answer {
		if a.Type != 16 { // TXT
			continue
		}
		// "a" "b" -> ab, as a TXT record's strings join
		var b strings.Builder
		for _, part := range strings.Split(a.Data, "\" \"") {
			b.WriteString(strings.Trim(part, "\""))
		}
		out = append(out, b.String())
	}
	return out, nil
}
