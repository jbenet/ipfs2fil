package gitarchive

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ConfigFile is a repo's archive settings, committed at its root in git
// config syntax, so `git config -f .ipfs-archive-config archive.tips all`
// edits it:
//
//	[archive]
//	    tips = default      # none | default | all | a ref, or several, comma-separated
//	[ipfs2fil]
//	    copies = 3          # sections other than archive go in the record's instructions
//
// The copy committed at HEAD counts, not the working tree's, so the archive
// can be made again from the repo alone. Options override it.
const ConfigFile = ".ipfs-archive-config"

// maxConfigFile caps the config file; a real one is a few lines.
const maxConfigFile = 64 << 10

// Config is what a repo's ConfigFile says.
type Config struct {
	Found bool
	Tips  string // "" when the file does not say

	Submodules *bool // submodules.archive
	LFSFetch   *bool // lfs.fetch

	// Instructions are the keys outside the sections this package reads,
	// such as ipfs2fil.copies: they are copied into the record for
	// whatever stores it.
	Instructions map[string]string

	Notes []string // settings that were ignored, and why
}

// own are the sections this package reads; every other key is an
// instruction for the storing tool.
var own = map[string]bool{"archive": true, "submodules": true, "lfs": true}

// notInstructions are git config sections that are never instructions.
var notInstructions = map[string]bool{"include": true, "includeif": true}

// ReadConfig reads ConfigFile as committed at HEAD. A repo without one, or
// without commits, has an empty Config.
func (g *Git) ReadConfig(ctx context.Context) (*Config, error) {
	cfg := &Config{}
	out, err := g.run(ctx, nil, "rev-parse", "-q", "--verify", "HEAD:"+ConfigFile)
	if err != nil {
		return cfg, nil // no HEAD or no file
	}
	id := strings.TrimSpace(string(out))
	if out, err = g.run(ctx, nil, "cat-file", "-t", id); err != nil {
		return nil, err
	}
	if t := strings.TrimSpace(string(out)); t != "blob" {
		cfg.Notes = append(cfg.Notes, fmt.Sprintf("%s at HEAD is a %s, not a file; ignored", ConfigFile, t))
		return cfg, nil
	}
	if out, err = g.run(ctx, nil, "cat-file", "-s", id); err != nil {
		return nil, err
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err != nil || n > maxConfigFile {
		cfg.Notes = append(cfg.Notes, fmt.Sprintf("%s at HEAD is over %d bytes; ignored", ConfigFile, maxConfigFile))
		return cfg, nil
	}
	body, err := g.run(ctx, nil, "cat-file", "blob", id)
	if err != nil {
		return nil, err
	}
	return parseConfig(ctx, g, body)
}

// parseConfig has git parse the file, with includes off: an include could
// read any file on this machine into the record.
func parseConfig(ctx context.Context, g *Git, body []byte) (*Config, error) {
	cfg := &Config{Found: true}
	f, err := os.CreateTemp("", "ipfs-archive-config-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	out, err := g.run(ctx, nil, "config", "--file", f.Name(), "--no-includes", "--null", "--list")
	if err != nil {
		cfg.Notes = append(cfg.Notes, fmt.Sprintf("%s at HEAD does not parse; ignored (%v)", ConfigFile, err))
		return &Config{Notes: cfg.Notes}, nil
	}
	for _, kv := range bytes.Split(out, []byte{0}) {
		if len(kv) == 0 {
			continue
		}
		key, val, hasVal := strings.Cut(string(kv), "\n")
		sec, name, _ := strings.Cut(key, ".")
		if !hasVal {
			val = "true" // a bare key is a true bool in git config
		}
		switch {
		case key == "archive.tips":
			cfg.Tips = strings.TrimSpace(val)
		case key == "submodules.archive":
			b, ok := parseBool(val, hasVal)
			if !ok {
				cfg.Notes = append(cfg.Notes, fmt.Sprintf("%s: submodules.archive = %q is not true or false; ignored", ConfigFile, val))
				continue
			}
			cfg.Submodules = &b
		case key == "lfs.fetch":
			b, ok := parseBool(val, hasVal)
			if !ok {
				cfg.Notes = append(cfg.Notes, fmt.Sprintf("%s: lfs.fetch = %q is not true or false; ignored", ConfigFile, val))
				continue
			}
			cfg.LFSFetch = &b
		case own[sec]:
			cfg.Notes = append(cfg.Notes, fmt.Sprintf("%s: %s is not a setting this version reads; ignored", ConfigFile, key))
		case name == "" || notInstructions[sec]:
			continue
		default:
			if cfg.Instructions == nil {
				cfg.Instructions = map[string]string{}
			}
			cfg.Instructions[key] = val
		}
	}
	return cfg, nil
}

// settings is what an archive run does, after options, the config file and
// defaults are combined. Non-defaults are kept in the record.
type settings struct {
	tips       string
	submodules bool
	lfsFetch   bool
}

func (s settings) record() map[string]string {
	m := map[string]string{}
	if s.tips != "none" {
		m["archive.tips"] = s.tips
	}
	if s.submodules {
		m["submodules.archive"] = "true"
	}
	if s.lfsFetch {
		m["lfs.fetch"] = "true"
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// resolve combines options over the config file over defaults. A config
// file the caller does not trust (one in a repo taken from someone else)
// cannot turn on settings that cost much more: tips = all.
func resolve(o Options, cfg *Config) (settings, []string) {
	s := settings{tips: "none"}
	notes := append([]string(nil), cfg.Notes...)
	if t := cfg.Tips; t != "" {
		if o.UntrustedConfig && t == "all" {
			notes = append(notes, fmt.Sprintf("%s asks for tips = all; ignored for a repo whose config is not trusted", ConfigFile))
		} else {
			s.tips = t
		}
	}
	if o.Tips != "" {
		s.tips = o.Tips
	}
	if b := cfg.Submodules; b != nil {
		if o.UntrustedConfig && *b {
			notes = append(notes, fmt.Sprintf("%s asks to archive submodules; ignored for a repo whose config is not trusted", ConfigFile))
		} else {
			s.submodules = *b
		}
	}
	if o.Submodules != nil {
		s.submodules = *o.Submodules
	}
	if b := cfg.LFSFetch; b != nil {
		if o.UntrustedConfig && *b {
			notes = append(notes, fmt.Sprintf("%s asks to fetch LFS objects; ignored for a repo whose config is not trusted", ConfigFile))
		} else {
			s.lfsFetch = *b
		}
	}
	if o.LFSFetch != nil {
		s.lfsFetch = *o.LFSFetch
	}
	return s, notes
}

// mergeInstructions is the record's instructions: the options' over the
// config file's when either is given, else the previous snapshot's.
func mergeInstructions(o Options, cfg *Config) map[string]string {
	if o.Instructions == nil && !cfg.Found {
		if o.Prev != nil {
			return o.Prev.Instructions
		}
		return nil
	}
	m := map[string]string{}
	for k, v := range cfg.Instructions {
		m[k] = v
	}
	for k, v := range o.Instructions {
		m[k] = v
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// parseBool reads a git config bool: a bare key is true, an empty value
// false.
func parseBool(v string, hasVal bool) (bool, bool) {
	if !hasVal {
		return true, true
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on", "1":
		return true, true
	case "false", "no", "off", "0", "":
		return false, true
	}
	return false, false
}
