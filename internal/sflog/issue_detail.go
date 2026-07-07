package sflog

import (
	"context"
	"errors"
	"strings"
)

// ErrNotAnArchive marks a file named like an archive whose bytes do not match
// the format signature (decoy/renamed junk). Surfaced as a parse issue.
var ErrNotAnArchive = errors.New("not a recognized archive")

// IssueDetail returns a short analyst-friendly reason for an issue, suitable
// for the summary block and debug log. Empty when there is nothing useful to add.
func IssueDetail(is Issue) string {
	if is.Err != nil {
		return humanizeIssueErr(is.Err)
	}
	switch is.Kind {
	case IssueNoULP:
		return "no credential files found"
	case IssuePasswordNotFound:
		return "none of the candidate passwords worked"
	case IssueMissingVolume:
		return "first volume of the set is missing"
	default:
		return ""
	}
}

func humanizeIssueErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "interrupted before completion"
	}
	if errors.Is(err, ErrNotAnArchive) {
		return "not a recognized archive (signature mismatch)"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "error reading header id: EOF"):
		return "truncated or corrupt archive (header EOF)"
	case strings.Contains(msg, "unexpected EOF"):
		return "truncated or corrupt archive (unexpected EOF)"
	case strings.Contains(msg, "password not found"):
		return "none of the candidate passwords worked"
	}
	return clampIssueMsg(msg)
}

func clampIssueMsg(msg string) string {
	const max = 96
	r := []rune(msg)
	if len(r) <= max {
		return msg
	}
	return string(r[:max-1]) + "…"
}
