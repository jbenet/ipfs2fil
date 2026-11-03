package atproto

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Server is what a PDS says about itself in com.atproto.server.describeServer.
type Server struct {
	DID                   string   `json:"did"`
	AvailableUserDomains  []string `json:"availableUserDomains"`
	InviteCodeRequired    bool     `json:"inviteCodeRequired"`
	PhoneVerificationReqd bool     `json:"phoneVerificationRequired"`
}

// DescribeServer asks the PDS at base (https://host) to describe itself. An
// error means base is not a PDS, or it did not answer.
func DescribeServer(ctx context.Context, c *http.Client, base string) (*Server, error) {
	var s Server
	if err := getJSON(ctx, c, strings.TrimSuffix(base, "/")+"/xrpc/com.atproto.server.describeServer", &s); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(s.DID, "did:") {
		return nil, fmt.Errorf("%s: describeServer names no server DID", base)
	}
	return &s, nil
}

// HostedRepo is one account a PDS lists in com.atproto.sync.listRepos.
type HostedRepo struct {
	DID    string `json:"did"`
	Head   string `json:"head"`
	Rev    string `json:"rev"`
	Active *bool  `json:"active,omitempty"` // absent on older servers: active
	Status string `json:"status,omitempty"` // why it is not active
}

// IsActive says whether the PDS serves the account.
func (h HostedRepo) IsActive() bool { return h.Active == nil || *h.Active }

// maxHostedRepos bounds how many accounts ListRepos reads from one server.
const maxHostedRepos = 1 << 20

// ListRepos lists every account the PDS at base hosts, following the
// cursor page by page.
func ListRepos(ctx context.Context, c *http.Client, base string) ([]HostedRepo, error) {
	base = strings.TrimSuffix(base, "/")
	var out []HostedRepo
	cursor := ""
	for {
		u := base + "/xrpc/com.atproto.sync.listRepos?limit=1000"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Cursor string       `json:"cursor"`
			Repos  []HostedRepo `json:"repos"`
		}
		if err := getJSON(ctx, c, u, &page); err != nil {
			return nil, fmt.Errorf("listing the accounts on %s: %w", base, err)
		}
		for _, r := range page.Repos {
			if strings.HasPrefix(r.DID, "did:") {
				out = append(out, r)
			}
		}
		if page.Cursor == "" || page.Cursor == cursor || len(page.Repos) == 0 {
			return out, nil
		}
		if len(out) > maxHostedRepos {
			return nil, fmt.Errorf("%s lists over %d accounts", base, maxHostedRepos)
		}
		cursor = page.Cursor
	}
}
