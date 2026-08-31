// Package server serves garnix build logs as plain text over HTTP. One
// catch-all handler resolves a path segment to a build, run, commit or repo and
// writes the matching view.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kradalby/garnixlogs/garnix"
	"github.com/kradalby/garnixlogs/render"
)

// defaultOwner is assumed when a repository is named without one.
const defaultOwner = "kradalby"

// defaultPoll is how often a followed build is re-checked for new output.
const defaultPoll = 2 * time.Second

// Server renders garnix API objects as text.
type Server struct {
	gx      *garnix.Client
	log     *slog.Logger
	owner   string
	baseURL string
	poll    time.Duration
}

// Option configures a Server.
type Option func(*Server) error

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) error {
		if l == nil {
			return errors.New("nil logger")
		}
		s.log = l

		return nil
	}
}

// WithDefaultOwner sets the owner assumed for a bare repository name.
func WithDefaultOwner(owner string) Option {
	return func(s *Server) error {
		if owner == "" {
			return errors.New("empty default owner")
		}
		s.owner = owner

		return nil
	}
}

// WithBaseURL sets the URL shown in the usage text.
func WithBaseURL(url string) Option {
	return func(s *Server) error {
		if url == "" {
			return errors.New("empty base url")
		}
		s.baseURL = url

		return nil
	}
}

// WithPollInterval sets how often a followed build is re-checked.
func WithPollInterval(d time.Duration) Option {
	return func(s *Server) error {
		if d <= 0 {
			return errors.New("poll interval must be positive")
		}
		s.poll = d

		return nil
	}
}

// New builds a Server around a garnix client.
func New(gx *garnix.Client, opts ...Option) (*Server, error) {
	if gx == nil {
		return nil, errors.New("nil garnix client")
	}

	s := &Server{
		gx:      gx,
		log:     slog.Default(),
		owner:   defaultOwner,
		baseURL: "http://garnixlogs",
		poll:    defaultPoll,
	}

	for _, o := range opts {
		if err := o(s); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// request is the parsed form of one incoming HTTP request.
type request struct {
	seg    string
	follow bool
	all    bool
	opts   render.Opts
}

func parse(r *http.Request) request {
	q := r.URL.Query()
	has := func(k string) bool { return q.Has(k) && q.Get(k) != "false" && q.Get(k) != "0" }

	return request{
		seg:    strings.Trim(r.URL.Path, "/"),
		follow: has("follow"),
		all:    has("all"),
		opts:   render.Opts{ANSI: has("ansi"), Timestamps: has("ts")},
	}
}

// flush pushes buffered output to the client, so a followed build appears live
// rather than in one lump at the end.
func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// Handle is the catch-all text handler.
func (s *Server) Handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	req := parse(r)
	if req.seg == "" {
		// Configured name, not r.Host: never reflect an attacker-controlled
		// header into the body.
		fmt.Fprint(w, render.Usage(s.baseURL))

		return
	}

	// Errors after the first byte cannot become a status code, so they are
	// appended to the body instead — an agent reading the tail still sees them.
	if err := s.route(r.Context(), w, req); err != nil {
		s.log.Error("request failed", "path", r.URL.Path, "err", err)

		if errors.Is(err, errNoMatch) {
			w.WriteHeader(http.StatusNotFound)
		}

		fmt.Fprintf(w, "\nerror: %v\n", err)
	}
}

// errNoMatch names the abbreviated-hash case explicitly: git prints short
// hashes everywhere, but garnix only resolves full ones.
var errNoMatch = errors.New(
	"not a known build id, run id, commit hash, or repository (commit hashes must be the full 40 characters)")

// route probes each kind the segment could name and serves the first that
// resolves.
func (s *Server) route(ctx context.Context, w http.ResponseWriter, req request) error {
	for _, kind := range render.Order(req.seg) {
		var err error

		switch kind {
		case render.KindBuild:
			err = s.serveBuild(ctx, w, req)
		case render.KindRun:
			err = s.serveRun(ctx, w, req)
		case render.KindCommit:
			err = s.serveCommit(ctx, w, req)
		case render.KindRepo:
			err = s.serveRepo(ctx, w, req)
		}

		if errors.Is(err, garnix.ErrNotFound) {
			s.log.Debug("probe missed", "segment", req.seg, "kind", kind)

			continue
		}

		return err
	}

	return fmt.Errorf("%q: %w", req.seg, errNoMatch)
}

func (s *Server) serveRepo(ctx context.Context, w http.ResponseWriter, req request) error {
	owner, repo := s.owner, req.seg
	if o, r, ok := strings.Cut(req.seg, "/"); ok {
		owner, repo = o, r
	}

	commits, err := s.gx.RepoCommits(ctx, owner, repo)
	if err != nil {
		return err
	}

	fmt.Fprint(w, render.RepoCommits(owner, repo, commits))

	return nil
}

func (s *Server) serveBuild(ctx context.Context, w http.ResponseWriter, req request) error {
	build, err := s.gx.Build(ctx, req.seg)
	if err != nil {
		return err
	}

	fmt.Fprint(w, render.BuildHeader(build))
	flush(w)

	last, err := s.drain(ctx, w, req.seg, s.gx.Logs, time.Time{}, req.opts)
	if err != nil {
		return err
	}

	// Following an already-finished build returns now rather than after a
	// poll tick, so ?follow is safe to pass unconditionally.
	if req.follow && !build.Done() {
		return s.followBuild(ctx, w, req, last)
	}

	fmt.Fprint(w, render.StatusLine(build))

	return nil
}

// followBuild keeps draining a build's output until it reaches a terminal
// status.
func (s *Server) followBuild(ctx context.Context, w http.ResponseWriter, req request, last time.Time) error {
	tick := time.NewTicker(s.poll)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}

		build, err := s.gx.Build(ctx, req.seg)
		if err != nil {
			return err
		}

		last, err = s.drain(ctx, w, req.seg, s.gx.Logs, last, req.opts)
		if err != nil {
			return err
		}

		// Status is checked before the drain's result is judged, so the final
		// page written after a build finishes is never cut off.
		if build.Done() {
			fmt.Fprint(w, render.StatusLine(build))
			flush(w)

			return nil
		}
	}
}

// serveRun writes an action run's output. Runs have no status endpoint of their
// own here, so the log pager's own "finished" flag ends a follow.
func (s *Server) serveRun(ctx context.Context, w http.ResponseWriter, req request) error {
	page, err := s.gx.RunLogs(ctx, req.seg, time.Time{})
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "===== run %s =====\n", req.seg)

	if err := render.Lines(w, page.Lines, req.opts); err != nil {
		return err
	}
	flush(w)

	last := lastStamp(page.Lines, time.Time{})
	if !req.follow {
		return nil
	}

	tick := time.NewTicker(s.poll)
	defer tick.Stop()

	for !page.Finished {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}

		if page, err = s.gx.RunLogs(ctx, req.seg, last); err != nil {
			return err
		}

		if err := render.Lines(w, page.Lines, req.opts); err != nil {
			return err
		}
		flush(w)

		last = lastStamp(page.Lines, last)
	}

	return nil
}

// serveCommit writes the commit summary and then the log of every failed build.
// Under ?follow it keeps watching until nothing is pending, emitting each
// build's log as it fails.
func (s *Server) serveCommit(ctx context.Context, w http.ResponseWriter, req request) error {
	commit, err := s.gx.Commit(ctx, req.seg)
	if err != nil {
		return err
	}

	fmt.Fprint(w, render.CommitHeader(commit, req.all))
	flush(w)

	shown := make(map[string]bool)
	if err := s.dumpFailed(ctx, w, commit, shown, req.opts); err != nil {
		return err
	}

	if !req.follow || commit.Summary.Pending == 0 {
		return nil
	}

	tick := time.NewTicker(s.poll)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}

		if commit, err = s.gx.Commit(ctx, req.seg); err != nil {
			return err
		}

		if err := s.dumpFailed(ctx, w, commit, shown, req.opts); err != nil {
			return err
		}

		if commit.Summary.Pending == 0 {
			fmt.Fprintf(w, "\n--- %s done: %s ---\n", render.Short(commit.Summary.Commit), render.Tally(commit.Summary))
			flush(w)

			return nil
		}
	}
}

// dumpFailed writes the log of every failed build not already written.
func (s *Server) dumpFailed(
	ctx context.Context,
	w http.ResponseWriter,
	commit *garnix.Commit,
	shown map[string]bool,
	opts render.Opts,
) error {
	for _, b := range commit.Builds {
		if b.Status == nil || *b.Status == "Success" || shown[b.ID] {
			continue
		}
		shown[b.ID] = true

		fmt.Fprintf(w, "\n%s", render.BuildHeader(&b))
		flush(w)

		if _, err := s.drain(ctx, w, b.ID, s.gx.Logs, time.Time{}, opts); err != nil {
			return err
		}
	}

	return nil
}

// pager fetches one page of output newer than after.
type pager func(ctx context.Context, id string, after time.Time) (*garnix.Logs, error)

// drain writes every log line available now, paging until it catches up, and
// returns the newest timestamp written. A short page means "nothing more yet",
// never "build finished" — a running build in a quiet moment returns one too.
func (s *Server) drain(
	ctx context.Context,
	w http.ResponseWriter,
	id string,
	fetch pager,
	after time.Time,
	opts render.Opts,
) (time.Time, error) {
	for {
		page, err := fetch(ctx, id, after)
		if err != nil {
			return after, err
		}

		if len(page.Lines) == 0 {
			return after, nil
		}

		if err := render.Lines(w, page.Lines, opts); err != nil {
			return after, err
		}
		flush(w)

		after = lastStamp(page.Lines, after)

		if page.MaxPageSize <= 0 || len(page.Lines) < page.MaxPageSize {
			return after, nil
		}
	}
}

// lastStamp returns the newest timestamp in lines, or fallback when empty.
func lastStamp(lines []garnix.LogLine, fallback time.Time) time.Time {
	if len(lines) == 0 {
		return fallback
	}

	return lines[len(lines)-1].Timestamp
}
