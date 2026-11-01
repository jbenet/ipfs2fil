package main

import (
	"context"
	"flag"
	"fmt"
)

func init() {
	register(&command{
		name:    "up",
		summary: "Pack and push in one go, after confirming the cost",
		help: `
The whole job in one command: reads the repo, packs the archive into --out,
shows what storing it costs, and after you confirm, uploads every piece not
stored yet. Run it again whenever the repo changes; only new pieces upload.
Stopped with Ctrl-C, it picks up where it left off. --yes skips the question.

With --all, does this for every repo 'ipfs2fil repos add' listed: blocks
shared between repos are stored once, all repos share one set of data sets,
and you confirm the cost once for all of them.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			repoFlag(fs, o)
			outFlag(fs, o)
			copiesFlag(fs, o)
			yesFlag(fs, o)
			fs.BoolVar(&packOrphans, "orphans", true, "include blocks no pin or MFS reaches, under data/orphans")
			fs.BoolVar(&packNewPages, "new-pages", false, "write the index and explorer pages anew even when the repo has not changed, so this version's explorer goes into the archive (only those pieces upload)")
			addStoreFlags(fs)
			allFlag(fs, o)
		},
		run: func(ctx context.Context, o *opts, _ []string) error {
			if err := checkKey(); err != nil {
				return err
			}
			if o.all {
				return upAll(ctx, o)
			}
			if _, err := pack(ctx, o); err != nil {
				return err
			}
			fmt.Fprintln(o.stdout)
			m, pieces, err := loadArchive(o.out)
			if err != nil {
				return err
			}
			return push(ctx, o, m, pieces)
		},
	})
}
