package main

import (
	"context"
	"fmt"
)

// Commands whose packages have not landed yet. Each moves to its own file as
// it is wired in.
func init() {
	for _, c := range []struct{ name, summary, waits string }{
		{"pack", "Build the archive and its pieces on disk", "the archive package"},
		{"push", "Upload pieces that are not stored yet (3 copies by default)", "the store package"},
		{"up", "Scan, pack and push in one go, after confirming the cost", "the archive and store packages"},
		{"status", "Show copies, funding and proofs for an archive", "the store package"},
		{"verify", "Fetch pieces back and check every block", "the store package"},
	} {
		c := c
		if commands[c.name] != nil {
			continue
		}
		register(&command{
			name:    c.name,
			summary: c.summary,
			help:    "Not in this build yet; it lands with " + c.waits + ".",
			run: func(context.Context, *opts, []string) error {
				return fmt.Errorf("%s is not in this build yet (it lands with %s)", c.name, c.waits)
			},
		})
	}
}
