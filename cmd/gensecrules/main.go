//go:build secrets

// Command gensecrules emits the titus secret-scanning rule reference in two
// formats, both refreshed in place between sentinel markers so surrounding
// hand-written content is preserved:
//
//	go run -tags secrets ./cmd/gensecrules -format md          -o <docs page>.mdx
//	go run -tags secrets ./cmd/gensecrules -format toml-comment -o config.toml.example
//
// The short description is the rule's prose Description (first line) when
// present, otherwise the joined Categories, otherwise empty. Run manually when
// the titus version bumps and commit the refreshed outputs.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/praetorian-inc/titus"
)

var whitespace = regexp.MustCompile(`\s+`)

const (
	beginMarker = "BEGIN titus rules"
	endMarker   = "END titus rules"
)

func main() {
	format := flag.String("format", "md", "output format: md | toml-comment")
	out := flag.String("o", "", "output path (refreshed in place between markers)")
	flag.Parse()

	rules, err := titus.LoadBuiltinRules()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load builtin rules: %v\n", err)
		os.Exit(1)
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })

	var body string
	switch *format {
	case "md":
		body = markdownTable(rules)
	case "toml-comment":
		body = tomlCommentTable(rules)
	default:
		fmt.Fprintf(os.Stderr, "unknown format %q\n", *format)
		os.Exit(2)
	}

	if err := writeRefresh(*out, *format, body); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		os.Exit(1)
	}
}

func shortDesc(r *titus.Rule) string {
	if d := strings.TrimSpace(r.Description); d != "" {
		line := strings.TrimSpace(whitespace.ReplaceAllString(strings.Split(d, "\n")[0], " "))
		return truncate(line, 80)
	}
	if cats := r.Categories; len(cats) > 0 {
		return strings.Join(cats, " / ")
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// markdownTable builds the GitHub-flavored markdown table used by the docs
// Reference page. Pipe characters in cell text are escaped.
func markdownTable(rules []*titus.Rule) string {
	var b strings.Builder
	b.WriteString("| Rule ID | Name | Description |\n")
	b.WriteString("|---------|------|-------------|\n")
	for _, r := range rules {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", mdCell(r.ID), mdCell(r.Name), mdCell(shortDesc(r)))
	}
	return b.String()
}

func mdCell(s string) string {
	// Escape HTML-special characters so descriptions containing e.g.
	// '<input type="...">' don't start a JSX tag inside MDX, plus the table
	// pipe. & first so we don't double-escape the entities we introduce.
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "|", "\\|")
	return s
}

// tomlCommentTable builds a #-prefixed, column-aligned block for
// config.toml.example so users can see rule IDs to glob against. IDs are never
// truncated (users need the full ID to write globs); the column pads to the
// longest ID in the set.
func tomlCommentTable(rules []*titus.Rule) string {
	idCol := 0
	for _, r := range rules {
		if len(r.ID) > idCol {
			idCol = len(r.ID)
		}
	}
	var b strings.Builder
	for _, r := range rules {
		desc := strings.ReplaceAll(shortDesc(r), "\n", " ")
		fmt.Fprintf(&b, "# %-*s  %s — %s\n", idCol, r.ID, r.Name, desc)
	}
	return b.String()
}

// writeRefresh wraps body with the format-appropriate sentinel markers and
// writes it to path. If path exists and already contains the markers, the
// region between them is replaced; otherwise the marked block is appended.
func writeRefresh(path, format, body string) error {
	var begin, end string
	switch format {
	case "md":
		// MDX (fumadocs) rejects HTML <!-- --> comments; use JSX expression
		// comments instead, which render to nothing.
		begin = "{/* " + beginMarker + " */}"
		end = "{/* " + endMarker + " */}"
	default: // toml-comment
		begin = "# " + beginMarker
		end = "# " + endMarker
	}
	block := begin + "\n" + body + end + "\n"

	existing, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return os.WriteFile(path, []byte(block), 0o644)
	}
	refreshed, ok := replaceBetween(existing, begin, end, body)
	if !ok {
		refreshed = append(existing, []byte("\n"+block)...)
	}
	return os.WriteFile(path, refreshed, 0o644)
}

// replaceBetween returns content with the region between begin and end lines
// replaced by body. Returns ok=false if the markers aren't both present.
func replaceBetween(content []byte, begin, end, body string) ([]byte, bool) {
	bi := bytes.Index(content, []byte(begin))
	ei := bytes.Index(content, []byte(end))
	if bi < 0 || ei < 0 || ei < bi {
		return nil, false
	}
	// keep everything up to and including the begin marker line; insert body;
	// resume at the end marker line through the rest.
	head := content[:bi+len(begin)]
	tail := content[ei:]
	out := make([]byte, 0, len(head)+len(body)+1+len(tail))
	out = append(out, head...)
	out = append(out, '\n')
	out = append(out, []byte(body)...)
	out = append(out, tail...)
	return out, true
}
