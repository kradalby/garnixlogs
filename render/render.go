// Package render turns garnix API objects into the flat text the HTTP surface
// serves. Everything here is pure — no network, no clock — so the output shape
// is fully covered by table tests.
package render

import (
	"cmp"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kradalby/garnixlogs/garnix"
)

// Kind is what a single URL path segment might name.
type Kind int

// The kinds a path segment can resolve to, in the order they are probed.
const (
	KindBuild Kind = iota
	KindRun
	KindCommit
	KindRepo
)

func (k Kind) String() string {
	switch k {
	case KindBuild:
		return "build"
	case KindRun:
		return "run"
	case KindCommit:
		return "commit"
	case KindRepo:
		return "repo"
	default:
		return "unknown"
	}
}

// ansiRE matches a CSI escape sequence. Build output is full of colour codes
// that only get in an agent's way.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// StripANSI removes colour and cursor escapes from a log line.
func StripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}

	return s != ""
}

func isAlnum(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}

	return s != ""
}

// Order returns the kinds a path segment could name, most likely first.
//
// Build and run ids are both Hashids with a minimum length of 8 and an
// alphanumeric alphabet, so they cannot be told apart from each other — nor
// from a short commit hash — by shape alone. Anything ambiguous is probed in
// this order rather than guessed.
func Order(seg string) []Kind {
	if strings.Contains(seg, "/") {
		return []Kind{KindRepo}
	}

	if len(seg) == 40 && isHex(seg) {
		return []Kind{KindCommit}
	}

	var order []Kind
	if len(seg) >= 8 && isAlnum(seg) {
		order = append(order, KindBuild, KindRun)
	}

	if len(seg) >= 7 && isHex(seg) {
		order = append(order, KindCommit)
	}

	return append(order, KindRepo)
}

// Usage is the help text served at /.
func Usage(host string) string {
	return fmt.Sprintf(`garnixlogs — garnix CI logs as plain text

  /<build-id>        full log for one build
  /<commit-sha>      summary, then the log of every failed build
  /<repo>            recent commits for kradalby/<repo>
  /<owner>/<repo>    recent commits, explicit owner

Flags (query string):

  ?follow            keep streaming until the build finishes
  ?all               commit view: list successful builds too
  ?ansi              keep colour escapes (stripped by default)
  ?ts                prefix every log line with its timestamp

Examples:

  curl %[1]s/dotfiles
  curl %[1]s/$(git rev-parse HEAD)
  curl "%[1]s/<build-id>?follow"
`, host)
}

// Short abbreviates a commit hash for display.
func Short(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}

	return commit
}

// Tally is the one-line build count shared by the commit and repo views.
func Tally(s garnix.Summary) string {
	out := fmt.Sprintf("ok=%d fail=%d pend=%d", s.Succeeded, s.Failed, s.Pending)
	if s.Cancelled > 0 {
		out += fmt.Sprintf(" cancelled=%d", s.Cancelled)
	}

	return out
}

// RepoCommits renders the recent-commit listing for one repository.
func RepoCommits(owner, repo string, commits []garnix.Summary) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s/%s\n", owner, repo)

	if len(commits) == 0 {
		b.WriteString("\nno commits built\n")

		return b.String()
	}

	b.WriteString("\n")

	// Full hashes, not abbreviated: the API rejects a short hash, so anything
	// printed here has to be usable as the next request's path.
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, c := range commits {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			c.Commit, c.Branch, Tally(c), c.StartTime.UTC().Format(time.RFC3339))
	}
	tw.Flush()

	return b.String()
}

// failed returns the builds that did not succeed, sorted for stable output.
func failed(builds []garnix.Build) []garnix.Build {
	var out []garnix.Build

	for _, b := range builds {
		if b.Status != nil && *b.Status != "Success" {
			out = append(out, b)
		}
	}

	slices.SortFunc(out, func(a, b garnix.Build) int {
		return cmp.Or(cmp.Compare(a.Package, b.Package), cmp.Compare(a.ID, b.ID))
	})

	return out
}

// buildTable renders one aligned id/status/package/system block.
func buildTable(w io.Writer, builds []garnix.Build) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, b := range builds {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
			b.ID, b.StatusOr("Running"), b.Package, b.SystemOr("-"))
	}
	tw.Flush()
}

// CommitHeader renders the summary block for a commit: the repo line, the
// tally, and a table of the builds that failed. The caller appends each failed
// build's log after it.
func CommitHeader(c *garnix.Commit, all bool) string {
	var b strings.Builder

	s := c.Summary
	fmt.Fprintf(&b, "%s/%s %s %s\n%s\n", s.RepoOwner, s.RepoName, Short(s.Commit), s.Branch, Tally(s))

	bad := failed(c.Builds)
	if len(bad) == 0 {
		b.WriteString("\nno failed builds\n")
	} else {
		b.WriteString("\nFAILED:\n")
		buildTable(&b, bad)
	}

	if all {
		ok := make([]garnix.Build, 0, len(c.Builds))
		for _, bld := range c.Builds {
			if bld.Status != nil && *bld.Status == "Success" {
				ok = append(ok, bld)
			}
		}

		if len(ok) > 0 {
			slices.SortFunc(ok, func(a, b garnix.Build) int { return cmp.Compare(a.Package, b.Package) })
			b.WriteString("\nSUCCEEDED:\n")
			buildTable(&b, ok)
		}
	}

	return b.String()
}

// BuildHeader is the one-line banner introducing a build's log.
func BuildHeader(b *garnix.Build) string {
	return fmt.Sprintf("===== %s  %s/%s  %s  %s  %s =====\n",
		b.ID, b.RepoUser, b.RepoName, Short(b.Commit), b.Package, b.StatusOr("Running"))
}

// StatusLine closes a build's log, so a caller that did not follow can still
// tell a finished build from one still running.
func StatusLine(b *garnix.Build) string {
	if b.Status == nil {
		return fmt.Sprintf("--- %s still running; re-request with ?follow to stream ---\n", b.ID)
	}

	return fmt.Sprintf("--- %s %s ---\n", b.ID, *b.Status)
}

// Opts controls per-line log formatting.
type Opts struct {
	ANSI       bool // keep colour escapes
	Timestamps bool // prefix each line with its timestamp
}

// Lines writes log lines as text, one per line.
func Lines(w io.Writer, lines []garnix.LogLine, opts Opts) error {
	for _, l := range lines {
		msg := l.Message
		if !opts.ANSI {
			msg = StripANSI(msg)
		}

		var err error
		if opts.Timestamps {
			_, err = fmt.Fprintf(w, "%s %s\n", l.Timestamp.UTC().Format(time.RFC3339Nano), msg)
		} else {
			_, err = fmt.Fprintln(w, msg)
		}

		if err != nil {
			return fmt.Errorf("write log line: %w", err)
		}
	}

	return nil
}
