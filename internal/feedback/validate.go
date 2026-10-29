package feedback

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// The kit's limits (feedback-dev-kit packages/feedback-server/src/validate.ts).
const (
	maxRequestBytes = 48_000_000
	maxImageBase64  = 12_000_000
	maxTotalBase64  = 40_000_000
	maxAttachments  = 20
	maxBodyChars    = 60_000
	maxTitleChars   = 200
	maxContextBytes = 64_000
)

type checkedPicture struct {
	kind, contentType, name, base64 string
}

var (
	dataURL     = regexp.MustCompile(`^data:(image/(?:png|jpeg|gif|webp));base64,([A-Za-z0-9+/=]+)$`)
	secretParam = regexp.MustCompile(`(?i)^(?:.*[_-])?(?:token|access_token|id_token|key|api_key|apikey|code|password|passwd|secret|signature|sig|session|auth)$`)
	secretHash  = regexp.MustCompile(`(?i)(?:token|code|key|secret)=`)
	lineBreaks  = regexp.MustCompile(`\s*[\r\n]+\s*`)
	nameJunk    = regexp.MustCompile(`[\r\n\\/]+`)
)

// serverKeys are context keys only the server writes.
var serverKeys = []string{"reporter", "reporterVerification", "user", "journaledAt", "receivedAt"}

// checkReport is the kit's checkReport: words, only the four image types
// (screenshots PNG only), sizes capped. It returns an HTTP status and reason
// when the report is refused.
func checkReport(b map[string]any) (journalRequest, []checkedPicture, int, string) {
	str := func(v any) string { s, _ := v.(string); return s }
	title := strings.TrimSpace(lineBreaks.ReplaceAllString(continuations(str(b["title"])), " "))
	body := strings.ReplaceAll(continuations(str(b["body"])), "\r\n", "\n")
	if title == "" && titleFrom(body) == "" {
		return journalRequest{}, nil, 400, "Say what happened. A description is enough."
	}
	if len([]rune(body)) > maxBodyChars {
		return journalRequest{}, nil, 413, fmt.Sprintf("The description is too long (over %d characters).", maxBodyChars)
	}
	if len([]rune(title)) > maxTitleChars {
		return journalRequest{}, nil, 413, fmt.Sprintf("The title is too long (over %d characters).", maxTitleChars)
	}

	type listed struct{ kind, name, dataURL string }
	var all []listed
	if as, _ := b["attachments"].([]any); len(as) > 0 {
		for _, a := range as {
			o, _ := a.(map[string]any)
			k := "image"
			if str(o["kind"]) == "screenshot" {
				k = "screenshot"
			}
			all = append(all, listed{k, str(o["name"]), str(o["dataUrl"])})
		}
	} else {
		ss, _ := b["screenshots"].([]any)
		for _, s := range ss {
			all = append(all, listed{"screenshot", "", str(s)})
		}
		is, _ := b["images"].([]any)
		for _, i := range is {
			o, _ := i.(map[string]any)
			all = append(all, listed{"image", str(o["name"]), str(o["dataUrl"])})
		}
	}
	if len(all) > maxAttachments {
		return journalRequest{}, nil, 413, fmt.Sprintf("Too many pictures (at most %d).", maxAttachments)
	}
	var pics []checkedPicture
	total := 0
	for _, a := range all {
		m := dataURL.FindStringSubmatch(a.dataURL)
		if a.kind == "screenshot" && (m == nil || m[1] != "image/png") {
			return journalRequest{}, nil, 400, "A screenshot was not a PNG."
		}
		if m == nil {
			return journalRequest{}, nil, 400, "An attached file was not a PNG, JPEG, GIF or WebP."
		}
		if len(m[2]) > maxImageBase64 {
			if a.kind == "screenshot" {
				return journalRequest{}, nil, 413, "A screenshot is too large."
			}
			return journalRequest{}, nil, 413, "An attached file is too large."
		}
		total += len(m[2])
		name := strings.TrimSpace(nameJunk.ReplaceAllString(a.name, " "))
		if r := []rune(name); len(r) > 120 {
			name = string(r[:120])
		}
		pics = append(pics, checkedPicture{a.kind, m[1], name, m[2]})
	}
	if total > maxTotalBase64 {
		return journalRequest{}, nil, 413, "The pictures together are too large. Remove one and send again."
	}

	ctx := map[string]any{}
	if c, ok := b["context"].(map[string]any); ok {
		for k, v := range c {
			ctx[k] = v
		}
	}
	for _, k := range serverKeys {
		delete(ctx, k)
	}
	scrubSecrets(ctx)
	if cb, err := json.Marshal(ctx); err != nil || len(cb) > maxContextBytes {
		return journalRequest{}, nil, 413, "The captured page context is too large."
	}

	kind := str(b["kind"])
	if !oneOf(kind, kinds) {
		kind = "bug"
	}
	priority := str(b["priority"])
	if !oneOf(priority, priorities) {
		priority = "P2"
	}
	page := strings.TrimSpace(lineBreaks.ReplaceAllString(str(b["page"]), " "))
	if r := []rune(page); len(r) > 2000 {
		page = string(r[:2000])
	}
	if page == "" {
		page = "/"
	}
	offset := 0
	if f, ok := b["imageOffset"].(float64); ok && f == float64(int(f)) && f >= 0 && int(f) <= len(all) {
		offset = int(f)
	}
	return journalRequest{
		Title: title, Body: body, Kind: kind, Priority: priority, Page: page,
		Context: ctx, ImageOffset: offset,
	}, pics, 0, ""
}

// scrubSecrets replaces credentials a URL or its filters carried (a magic
// link, an OAuth redirect) before anything is written.
func scrubSecrets(ctx map[string]any) {
	if s, ok := ctx["url"].(string); ok {
		if u, err := url.Parse(s); err == nil {
			q := u.Query()
			changed := false
			for k := range q {
				if secretParam.MatchString(k) {
					q.Set(k, "[removed]")
					changed = true
				}
			}
			if changed {
				u.RawQuery = q.Encode()
			}
			if u.Fragment != "" && secretHash.MatchString(u.Fragment) {
				u.Fragment, u.RawFragment, changed = "", "", true
			}
			if u.User != nil {
				u.User, changed = nil, true
			}
			if changed {
				ctx["url"] = u.String()
			}
		}
	}
	if f, ok := ctx["filters"].(map[string]any); ok {
		out := map[string]any{}
		for k, v := range f {
			if secretParam.MatchString(k) {
				v = "[removed]"
			}
			out[k] = v
		}
		ctx["filters"] = out
	}
}

func newClientID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
