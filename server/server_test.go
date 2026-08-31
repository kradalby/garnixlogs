package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kradalby/garnixlogs/garnix"
)

const testBuildID = "qgx4megl"

func status(s string) *string { return &s }

// fakeGarnix serves the slice of the garnix API the server uses.
//
// It reproduces one behaviour that matters: a path segment that is not a valid
// Hashid is rejected by the route parser with 400, not 404. Probing has to
// treat that as "wrong kind of id" or a bare repository name never resolves.
func fakeGarnix(t *testing.T, running bool) *httptest.Server {
	t.Helper()

	stamp := time.Date(2026, 8, 29, 8, 45, 47, 0, time.UTC)

	buildOf := func(st *string) garnix.Build {
		return garnix.Build{
			ID: testBuildID, RepoUser: "kradalby", RepoName: "dotfiles",
			Commit: "3090e8b2cb364fe70b4a5d0db00dd082ec5f1649",
			Branch: "master", Package: "dev-ldn", Status: st,
		}
	}

	// A followed build reports as running once, then finishes, so the poll
	// loop is exercised without the test depending on wall-clock timing.
	calls := 0

	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/auth/jwt", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"token":     "test-jwt",
			"expiresAt": time.Now().Add(time.Hour),
		})
	})

	mux.HandleFunc("GET /api/build/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != testBuildID {
			http.Error(w, "Invalid hash id", http.StatusBadRequest)

			return
		}

		st := status("Failure")
		if running && calls == 0 {
			st = nil
		}
		calls++

		json.NewEncoder(w).Encode(buildOf(st))
	})

	mux.HandleFunc("GET /api/build/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != testBuildID {
			http.Error(w, "Invalid hash id", http.StatusBadRequest)

			return
		}

		// Only the first page has content; the "after" page is empty, which is
		// how the real API signals "caught up".
		lines := []garnix.LogLine{{Timestamp: stamp, Message: "\x1b[31;1merror:\x1b[0m boom"}}
		if r.URL.Query().Get("after") != "" {
			lines = nil
		}

		json.NewEncoder(w).Encode(garnix.Logs{Finished: true, MaxPageSize: 4096, Lines: lines})
	})

	mux.HandleFunc("GET /api/run/{id}/logs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Invalid hash id", http.StatusBadRequest)
	})

	mux.HandleFunc("GET /api/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		if len(r.PathValue("sha")) != 40 {
			http.Error(w, "not found", http.StatusNotFound)

			return
		}

		json.NewEncoder(w).Encode(garnix.Commit{
			Summary: garnix.Summary{
				RepoOwner: "kradalby", RepoName: "dotfiles",
				Commit: r.PathValue("sha"), Branch: "master",
				Succeeded: 1, Failed: 1,
			},
			Builds: []garnix.Build{buildOf(status("Failure"))},
		})
	})

	mux.HandleFunc("GET /api/commits/repo/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("repo") != "dotfiles" {
			http.Error(w, "not found", http.StatusNotFound)

			return
		}

		json.NewEncoder(w).Encode(garnix.CommitList{Commits: []garnix.Summary{{
			Commit: "3090e8b2cb364fe70b4a5d0db00dd082ec5f1649",
			Branch: "master", Succeeded: 22, StartTime: stamp,
		}}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

func newTestServer(t *testing.T, running bool) *Server {
	t.Helper()

	up := fakeGarnix(t, running)

	gx, err := garnix.New(up.URL, "kradalby", "token",
		garnix.WithLogger(slog.New(slog.DiscardHandler)))
	require.NoError(t, err)

	s, err := New(gx,
		WithLogger(slog.New(slog.DiscardHandler)),
		WithPollInterval(10*time.Millisecond))
	require.NoError(t, err)

	return s
}

func get(t *testing.T, s *Server, target string) (int, string) {
	t.Helper()

	rec := httptest.NewRecorder()
	s.Handle(rec, httptest.NewRequest(http.MethodGet, target, nil))

	return rec.Code, rec.Body.String()
}

func TestRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		target   string
		wantCode int
		contains []string
		absent   []string
	}{
		{
			name:     "root serves usage",
			target:   "/",
			wantCode: http.StatusOK,
			contains: []string{"garnixlogs", "?follow"},
		},
		{
			// The bare name is shaped like a Hashid, so it is probed as a
			// build and a run first. Both reject it with 400, and the repo
			// lookup behind them is what must answer.
			name:     "bare repo name falls through two 400s",
			target:   "/dotfiles",
			wantCode: http.StatusOK,
			contains: []string{"kradalby/dotfiles", "3090e8b2cb364fe70b4a5d0db00dd082ec5f1649"},
		},
		{
			name:     "explicit owner and repo",
			target:   "/kradalby/dotfiles",
			wantCode: http.StatusOK,
			contains: []string{"kradalby/dotfiles"},
		},
		{
			name:     "build id",
			target:   "/" + testBuildID,
			wantCode: http.StatusOK,
			contains: []string{"===== " + testBuildID, "error: boom", "--- qgx4megl Failure ---"},
			absent:   []string{"\x1b["},
		},
		{
			name:     "ansi kept on request",
			target:   "/" + testBuildID + "?ansi",
			wantCode: http.StatusOK,
			contains: []string{"\x1b[31;1merror:"},
		},
		{
			name:     "full commit hash",
			target:   "/3090e8b2cb364fe70b4a5d0db00dd082ec5f1649",
			wantCode: http.StatusOK,
			contains: []string{"kradalby/dotfiles 3090e8b2 master", "FAILED:", "error: boom"},
		},
		{
			name:     "unknown segment reports the short-hash trap",
			target:   "/nosuchthing",
			wantCode: http.StatusNotFound,
			contains: []string{"full 40 characters"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestServer(t, false)
			code, body := get(t, s, tt.target)

			require.Equal(t, tt.wantCode, code, "body: %s", body)

			for _, want := range tt.contains {
				require.Contains(t, body, want)
			}

			for _, no := range tt.absent {
				require.NotContains(t, body, no)
			}
		})
	}
}

// TestFollowTerminates covers the poll loop: a build that reports as running
// must be re-checked until it reaches a terminal status, and the response must
// end rather than hang once it does.
func TestFollowTerminates(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, true)

	done := make(chan string, 1)
	go func() {
		_, body := get(t, s, "/"+testBuildID+"?follow")
		done <- body
	}()

	select {
	case body := <-done:
		require.Contains(t, body, "--- qgx4megl Failure ---")
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not terminate after the build finished")
	}
}

// TestFollowOnFinishedBuildReturnsAtOnce means an agent can pass ?follow
// unconditionally without paying a poll interval for an old build.
func TestFollowOnFinishedBuildReturnsAtOnce(t *testing.T) {
	t.Parallel()

	s, err := New(mustClient(t, fakeGarnix(t, false).URL), WithLogger(slog.New(slog.DiscardHandler)),
		WithPollInterval(time.Hour))
	require.NoError(t, err)

	start := time.Now()
	code, body := get(t, s, "/"+testBuildID+"?follow")

	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, "--- qgx4megl Failure ---")
	require.Less(t, time.Since(start), 5*time.Second)
}

func mustClient(t *testing.T, url string) *garnix.Client {
	t.Helper()

	gx, err := garnix.New(url, "kradalby", "token", garnix.WithLogger(slog.New(slog.DiscardHandler)))
	require.NoError(t, err)

	return gx
}

func TestParseFlags(t *testing.T) {
	t.Parallel()

	req := parse(httptest.NewRequest(http.MethodGet, "/x?follow&ansi&ts&all", nil))
	require.True(t, req.follow)
	require.True(t, req.all)
	require.True(t, req.opts.ANSI)
	require.True(t, req.opts.Timestamps)

	// An explicit false turns a flag off, so ?follow=0 is not a surprise.
	off := parse(httptest.NewRequest(http.MethodGet, "/x?follow=0&ansi=false", nil))
	require.False(t, off.follow)
	require.False(t, off.opts.ANSI)

	require.Equal(t, "x", strings.TrimPrefix(req.seg, "/"))
}
