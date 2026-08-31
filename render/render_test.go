package render

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	"github.com/kradalby/garnixlogs/garnix"
)

func TestOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		seg  string
		want []Kind
	}{
		{
			name: "owner and repo",
			seg:  "kradalby/dotfiles",
			want: []Kind{KindRepo},
		},
		{
			name: "full commit hash is unambiguous",
			seg:  "3090e8b2cb364fe70b4a5d0db00dd082ec5f1649",
			want: []Kind{KindCommit},
		},
		{
			name: "hashid with non-hex letters cannot be a commit",
			seg:  "qgx4megl",
			want: []Kind{KindBuild, KindRun, KindRepo},
		},
		{
			name: "bare repo name has the shape of a hashid, so it is probed as one",
			seg:  "dotfiles",
			want: []Kind{KindBuild, KindRun, KindRepo},
		},
		{
			name: "short hex is both a possible hashid and a possible commit",
			seg:  "129c34c6",
			want: []Kind{KindBuild, KindRun, KindCommit, KindRepo},
		},
		{
			name: "too short for a hashid",
			seg:  "abc1234",
			want: []Kind{KindCommit, KindRepo},
		},
		{
			name: "short non-hex name is only a repo",
			seg:  "kra",
			want: []Kind{KindRepo},
		},
		{
			name: "punctuation rules out an id",
			seg:  "my-repo",
			want: []Kind{KindRepo},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if diff := cmp.Diff(tt.want, Order(tt.seg)); diff != "" {
				t.Errorf("Order(%q) mismatch (-want +got):\n%s", tt.seg, diff)
			}
		})
	}
}

func TestStripANSI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain text is untouched",
			in:   "these 194 derivations will be built:",
			want: "these 194 derivations will be built:",
		},
		{
			name: "nix error colouring",
			in:   "\x1b[31;1merror:\x1b[0m Cannot build",
			want: "error: Cannot build",
		},
		{
			name: "reset only",
			in:   "done\x1b[0m",
			want: "done",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, StripANSI(tt.in))
		})
	}
}

func TestShort(t *testing.T) {
	t.Parallel()

	require.Equal(t, "3090e8b2", Short("3090e8b2cb364fe70b4a5d0db00dd082ec5f1649"))
	require.Equal(t, "abc", Short("abc"))
	require.Empty(t, Short(""))
}

func TestTally(t *testing.T) {
	t.Parallel()

	require.Equal(t, "ok=22 fail=4 pend=0",
		Tally(garnix.Summary{Succeeded: 22, Failed: 4}))
	require.Equal(t, "ok=1 fail=0 pend=2 cancelled=3",
		Tally(garnix.Summary{Succeeded: 1, Pending: 2, Cancelled: 3}))
}

// TestRepoCommitsPrintsFullHash guards the round trip: garnix rejects an
// abbreviated commit hash, so a hash printed here must be usable verbatim as
// the next request's path.
func TestRepoCommitsPrintsFullHash(t *testing.T) {
	t.Parallel()

	const full = "3090e8b2cb364fe70b4a5d0db00dd082ec5f1649"

	out := RepoCommits("kradalby", "dotfiles", []garnix.Summary{{
		Commit:    full,
		Branch:    "master",
		Succeeded: 22,
		Failed:    4,
		StartTime: time.Date(2026, 8, 29, 8, 44, 23, 0, time.UTC),
	}})

	require.Contains(t, out, "kradalby/dotfiles")
	require.Contains(t, out, full)
	require.Contains(t, out, "ok=22 fail=4 pend=0")
	require.Contains(t, out, "2026-08-29T08:44:23Z")
}

func TestRepoCommitsEmpty(t *testing.T) {
	t.Parallel()

	require.Contains(t, RepoCommits("kradalby", "nope", nil), "no commits built")
}

func build(id, pkg, status string) garnix.Build {
	b := garnix.Build{
		ID:       id,
		RepoUser: "kradalby",
		RepoName: "dotfiles",
		Commit:   "3090e8b2cb364fe70b4a5d0db00dd082ec5f1649",
		Package:  pkg,
	}
	if status != "" {
		b.Status = &status
	}

	return b
}

func TestCommitHeader(t *testing.T) {
	t.Parallel()

	c := &garnix.Commit{
		Summary: garnix.Summary{
			RepoOwner: "kradalby",
			RepoName:  "dotfiles",
			Commit:    "3090e8b2cb364fe70b4a5d0db00dd082ec5f1649",
			Branch:    "master",
			Succeeded: 1,
			Failed:    2,
		},
		Builds: []garnix.Build{
			build("aaaaaaaa", "zeta", "Success"),
			build("cccccccc", "beta", "Timeout"),
			build("bbbbbbbb", "alpha", "Failure"),
			build("dddddddd", "running", ""),
		},
	}

	out := CommitHeader(c, false)

	require.Contains(t, out, "kradalby/dotfiles 3090e8b2 master")
	require.Contains(t, out, "ok=1 fail=2 pend=0")
	require.Contains(t, out, "FAILED:")

	// Failures are sorted by package, so output is stable between requests.
	require.Less(t, strings.Index(out, "alpha"), strings.Index(out, "beta"))

	// A build with no status yet has not failed, and a success is not listed
	// unless asked for.
	require.NotContains(t, out, "running")
	require.NotContains(t, out, "zeta")

	withAll := CommitHeader(c, true)
	require.Contains(t, withAll, "SUCCEEDED:")
	require.Contains(t, withAll, "zeta")
}

func TestCommitHeaderNoFailures(t *testing.T) {
	t.Parallel()

	c := &garnix.Commit{
		Summary: garnix.Summary{RepoOwner: "kradalby", RepoName: "kra", Succeeded: 3},
		Builds:  []garnix.Build{build("aaaaaaaa", "one", "Success")},
	}

	require.Contains(t, CommitHeader(c, false), "no failed builds")
}

func TestStatusLine(t *testing.T) {
	t.Parallel()

	done := build("qgx4megl", "dev-ldn", "Failure")
	require.Equal(t, "--- qgx4megl Failure ---\n", StatusLine(&done))

	running := build("qgx4megl", "dev-ldn", "")
	require.Contains(t, StatusLine(&running), "?follow")
}

func TestLines(t *testing.T) {
	t.Parallel()

	stamp := time.Date(2026, 8, 29, 8, 45, 47, 0, time.UTC)
	lines := []garnix.LogLine{
		{Timestamp: stamp, Message: "\x1b[31;1merror:\x1b[0m boom"},
	}

	tests := []struct {
		name string
		opts Opts
		want string
	}{
		{
			name: "colour stripped by default",
			opts: Opts{},
			want: "error: boom\n",
		},
		{
			name: "ansi kept on request",
			opts: Opts{ANSI: true},
			want: "\x1b[31;1merror:\x1b[0m boom\n",
		},
		{
			name: "timestamps prefixed on request",
			opts: Opts{Timestamps: true},
			want: "2026-08-29T08:45:47Z error: boom\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b strings.Builder
			require.NoError(t, Lines(&b, lines, tt.opts))
			require.Equal(t, tt.want, b.String())
		})
	}
}

func TestUsageMentionsHost(t *testing.T) {
	t.Parallel()

	require.Contains(t, Usage("http://garnixlogs"), "curl http://garnixlogs/dotfiles")
}
