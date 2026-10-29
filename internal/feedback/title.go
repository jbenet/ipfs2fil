package feedback

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// titleMax is the longest title titleFrom makes, in characters.
const titleMax = 80

var (
	listMarker   = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)]|>|#{1,6})\s*`)
	imageSyntax  = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	imageAlt     = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	linkSyntax   = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	emphasis     = regexp.MustCompile("[*_`]")
	spaces       = regexp.MustCompile(`\s+`)
	firstSent    = regexp.MustCompile(`^(.+?[.!?])(?:\s|$)`)
	trailPunct   = regexp.MustCompile(`[.,:;\s—-]+$`)
	aside        = regexp.MustCompile(`\s*\([^)]*\)`)
	lastWord     = regexp.MustCompile(`\s+\S*$`)
	continuation = regexp.MustCompile(`[ \t]*\\[ \t]*(\r?\n|$)`)
)

// continuations removes a backslash at the end of a line: a terminal's typed
// newline, not text anybody meant to file.
func continuations(s string) string {
	return continuation.ReplaceAllString(s, "$1")
}

func hasWord(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

// titleFrom is the kit's fallback title: the first sentence somebody wrote,
// at most 80 characters, or "" when there is nothing to name.
func titleFrom(body string) string {
	var line string
	for _, l := range strings.Split(continuations(body), "\n") {
		l = strings.TrimSpace(listMarker.ReplaceAllString(l, ""))
		if hasWord(l) && !strings.HasPrefix(l, "```") && !strings.HasPrefix(l, "![") {
			line = l
			break
		}
	}
	if line == "" {
		return ""
	}
	t := imageSyntax.ReplaceAllString(line, "")
	t = linkSyntax.ReplaceAllString(t, "$1")
	t = emphasis.ReplaceAllString(t, "")
	t = strings.TrimSpace(spaces.ReplaceAllString(t, " "))
	if t == "" {
		return ""
	}
	if m := firstSent.FindStringSubmatch(t); m != nil && utf8.RuneCountInString(m[1]) >= 12 {
		t = m[1]
	}
	t = strings.TrimSpace(trailPunct.ReplaceAllString(t, ""))
	if utf8.RuneCountInString(t) > titleMax {
		t = strings.TrimSpace(spaces.ReplaceAllString(aside.ReplaceAllString(t, ""), " "))
	}
	if r := []rune(t); len(r) > titleMax {
		head := string(r[:titleMax])
		clause := -1
		for _, sep := range []string{", ", "; ", " — ", ": "} {
			if i := strings.LastIndex(head, sep); i > clause {
				clause = i
			}
		}
		if clause >= 0 && utf8.RuneCountInString(head[:clause]) >= 32 {
			t = head[:clause]
		} else {
			t = lastWord.ReplaceAllString(string(r[:titleMax-1]), "") + "…"
		}
	}
	r := []rune(t)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// lineTitle is the last resort: the first line with a word in it, images
// reduced to their alt text, one line, at most 80 characters.
func lineTitle(body string) string {
	for _, l := range strings.Split(imageAlt.ReplaceAllString(body, "$1"), "\n") {
		l = strings.TrimSpace(spaces.ReplaceAllString(emphasis.ReplaceAllString(l, ""), " "))
		l = strings.TrimRight(l, ". ")
		if !hasWord(l) {
			continue
		}
		if r := []rune(l); len(r) > titleMax {
			l = lastWord.ReplaceAllString(string(r[:titleMax-1]), "") + "…"
		}
		return l
	}
	return ""
}
