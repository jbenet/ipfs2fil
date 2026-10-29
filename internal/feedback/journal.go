package feedback

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The journal is the kit's (feedback-dev-kit docs/SERVER.md §4): a report is
// on disk, fsynced, before the browser hears 202, then filed as an issue.
//
//	<inbox>/<clientId>.json                       journaled, not yet filed
//	<inbox>/files/<clientId>.<nonce>/N-kind.ext   its pictures
//	<inbox>/filed/<clientId>.json                 filed: issue id and file
//	<inbox>/refused/<clientId>.json               can never be filed

type journalAttachment struct {
	Kind        string `json:"kind"`
	ContentType string `json:"contentType"`
	Name        string `json:"name,omitempty"`
	File        string `json:"file"`
	Bytes       int    `json:"bytes"`
	SHA256      string `json:"sha256"`
}

type journalRequest struct {
	Title       string              `json:"title"`
	Body        string              `json:"body"`
	Kind        string              `json:"kind"`
	Priority    string              `json:"priority"`
	Page        string              `json:"page"`
	Context     map[string]any      `json:"context"`
	ImageOffset int                 `json:"imageOffset"`
	Attachments []journalAttachment `json:"attachments"`
}

type journalEntry struct {
	V          int            `json:"v"`
	ClientID   string         `json:"clientId"`
	ReceivedAt string         `json:"receivedAt"`
	Reporter   *string        `json:"reporter"`
	Request    journalRequest `json:"request"`
}

type filedRecord struct {
	ClientID   string `json:"clientId"`
	IssueID    string `json:"issueId"`
	Location   string `json:"location"`
	Title      string `json:"title"`
	ReceivedAt string `json:"receivedAt"`
	FiledAt    string `json:"filedAt"`
}

type journal struct{ dir string }

// state says where a client id stands: "journaled", "filed", "refused" or "".
func (j journal) state(id string) (string, *filedRecord, string) {
	if _, err := os.Stat(filepath.Join(j.dir, id+".json")); err == nil {
		return "journaled", nil, ""
	}
	if b, err := os.ReadFile(filepath.Join(j.dir, "filed", id+".json")); err == nil {
		var f filedRecord
		json.Unmarshal(b, &f)
		return "filed", &f, ""
	}
	if b, err := os.ReadFile(filepath.Join(j.dir, "refused", id+".json")); err == nil {
		var r struct {
			Reason string `json:"reason"`
		}
		json.Unmarshal(b, &r)
		return "refused", nil, r.Reason
	}
	return "", nil, ""
}

// write journals a checked report, with its pictures decoded beside it. It
// writes nothing for a client id the journal already knows.
func (j journal) write(e journalEntry, pics []checkedPicture) error {
	nonce := make([]byte, 4)
	rand.Read(nonce)
	folder := "files/" + e.ClientID + "." + hex.EncodeToString(nonce)
	for i, p := range pics {
		b, err := base64.StdEncoding.DecodeString(p.base64)
		if err != nil {
			return errors.New("a picture is not valid base64")
		}
		name := fmt.Sprintf("%d-%s.%s", i+1, p.kind, extOf[p.contentType])
		if err := writeAtomic(filepath.Join(j.dir, filepath.FromSlash(folder)), name, b); err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		e.Request.Attachments = append(e.Request.Attachments, journalAttachment{
			Kind: p.kind, ContentType: p.contentType, Name: p.name, File: folder + "/" + name,
			Bytes: len(b), SHA256: hex.EncodeToString(sum[:]),
		})
	}
	if e.Request.Attachments == nil {
		e.Request.Attachments = []journalAttachment{}
	}
	return writeAtomic(j.dir, e.ClientID+".json", []byte(jsonText(e, "  ")))
}

// pending lists journaled entries, oldest first.
func (j journal) pending() []journalEntry {
	names, _ := os.ReadDir(j.dir)
	var out []journalEntry
	for _, n := range names {
		if n.IsDir() || strings.HasPrefix(n.Name(), ".") || !strings.HasSuffix(n.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(j.dir, n.Name()))
		if err != nil {
			continue
		}
		var e journalEntry
		if json.Unmarshal(b, &e) == nil && isClientID(e.ClientID) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ReceivedAt < out[b].ReceivedAt })
	return out
}

// filed records where an entry went and removes it and its pictures.
func (j journal) filed(e journalEntry, f filedRecord) error {
	if err := writeAtomic(filepath.Join(j.dir, "filed"), e.ClientID+".json", []byte(jsonText(f, "  "))); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(j.dir, e.ClientID+".json")); err != nil {
		return err
	}
	for _, a := range e.Request.Attachments {
		os.RemoveAll(filepath.Join(j.dir, filepath.FromSlash(filepath.Dir(a.File))))
	}
	return nil
}

// refuse sets an entry aside for good, with the reason.
func (j journal) refuse(e journalEntry, reason string) error {
	b, _ := json.Marshal(struct {
		journalEntry
		Reason string `json:"reason"`
	}{e, reason})
	if err := writeAtomic(filepath.Join(j.dir, "refused"), e.ClientID+".json", b); err != nil {
		return err
	}
	return os.Remove(filepath.Join(j.dir, e.ClientID+".json"))
}

// pictures reads an entry's pictures back for filing.
func (j journal) pictures(e journalEntry) ([]picture, error) {
	var out []picture
	for _, a := range e.Request.Attachments {
		if strings.Contains(a.File, "..") {
			return nil, errors.New("bad picture path")
		}
		b, err := os.ReadFile(filepath.Join(j.dir, filepath.FromSlash(a.File)))
		if err != nil {
			return nil, err
		}
		out = append(out, picture{Kind: a.Kind, ContentType: a.ContentType, Bytes: b})
	}
	return out, nil
}
