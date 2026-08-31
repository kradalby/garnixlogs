// Package garnix is a client for the garnix CI HTTP API.
//
// Auth is two-step: the access token is the Basic-auth password that mints a
// short-lived Bearer JWT. Sent as a Bearer token itself it reads as anonymous —
// fine on public repos, a misleading "not found" on private ones.
package garnix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// jwtSkew is how long before expiry a cached JWT is re-minted, so a request
// cannot start with a token that expires mid-flight.
const jwtSkew = time.Minute

var (
	// ErrNotFound is returned for a 404. Callers probing an ambiguous path
	// segment rely on it to tell "wrong kind of id" from "backend is broken".
	ErrNotFound = errors.New("not found")

	// ErrNoToken means no access token was configured, so only public
	// repositories are reachable.
	ErrNoToken = errors.New("no access token configured")
)

// Summary is the build tally for one commit.
type Summary struct {
	RepoOwner string    `json:"repo_owner"`
	RepoName  string    `json:"repo_name"`
	Public    bool      `json:"repo_is_public"`
	Commit    string    `json:"git_commit"`
	Branch    string    `json:"branch"`
	ReqUser   string    `json:"req_user"`
	StartTime time.Time `json:"start_time"`
	Succeeded int       `json:"succeeded"`
	Failed    int       `json:"failed"`
	Pending   int       `json:"pending"`
	Cancelled int       `json:"cancelled"`
}

// Build is one derivation realised for a commit. Status is nil while the build
// is still queued or running; System is nil for evaluation-only entries.
type Build struct {
	ID          string     `json:"id"`
	RepoUser    string     `json:"repo_user"`
	RepoName    string     `json:"repo_name"`
	Commit      string     `json:"git_commit"`
	Branch      string     `json:"branch"`
	Package     string     `json:"package"`
	PackageType string     `json:"package_type"`
	System      *string    `json:"system"`
	Status      *string    `json:"status"`
	StartTime   *time.Time `json:"start_time"`
	EndTime     *time.Time `json:"end_time"`
}

// Done reports whether the build has reached a terminal status.
func (b Build) Done() bool { return b.Status != nil }

// StatusOr returns the build status, or fallback while it is still running.
func (b Build) StatusOr(fallback string) string {
	if b.Status == nil {
		return fallback
	}

	return *b.Status
}

// SystemOr returns the build's system, or fallback when the API reports none.
func (b Build) SystemOr(fallback string) string {
	if b.System == nil {
		return fallback
	}

	return *b.System
}

// Commit is the full build set for one commit.
type Commit struct {
	Summary Summary `json:"summary"`
	Builds  []Build `json:"builds"`
}

// CommitList is the recent-commit listing for one repository.
type CommitList struct {
	Commits []Summary `json:"commits"`
}

// LogLine is a single line of build output.
type LogLine struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"log_message"`
	Package   *string   `json:"package"`
	Phase     *string   `json:"phase"`
}

// Logs is one page of build output. Finished is a paging signal, not a build
// state — use Build.Done for that.
type Logs struct {
	Finished    bool      `json:"finished"`
	MaxPageSize int       `json:"max_page_size"`
	Lines       []LogLine `json:"logs"`
}

// Client talks to one garnix server, caching the minted JWT between calls.
type Client struct {
	base  string
	user  string
	token string
	hc    *http.Client
	log   *slog.Logger

	mu     sync.Mutex
	jwt    string
	jwtExp time.Time
}

// Option configures a Client.
type Option func(*Client) error

// WithHTTPClient replaces the default HTTP client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) error {
		if hc == nil {
			return errors.New("nil http client")
		}
		c.hc = hc

		return nil
	}
}

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) error {
		if l == nil {
			return errors.New("nil logger")
		}
		c.log = l

		return nil
	}
}

// New builds a Client for the garnix server at baseURL. user and token may be
// empty, in which case requests are anonymous and only public repositories
// resolve.
func New(baseURL, user, token string, opts ...Option) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("base url is required")
	}

	c := &Client{
		base:  strings.TrimSuffix(baseURL, "/"),
		user:  user,
		token: token,
		// No overall timeout: a follow request streams for as long as the
		// build runs. Per-request deadlines come from the caller's context.
		hc:  &http.Client{},
		log: slog.Default(),
	}

	for _, o := range opts {
		if err := o(c); err != nil {
			return nil, err
		}
	}

	return c, nil
}

// Authenticated reports whether a token was configured.
func (c *Client) Authenticated() bool { return c.token != "" }

// mintJWT trades the access token for a fresh JWT over Basic auth.
func (c *Client) mintJWT(ctx context.Context) (string, time.Time, error) {
	if c.token == "" {
		return "", time.Time{}, ErrNoToken
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/auth/jwt", nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build jwt request: %w", err)
	}
	req.SetBasicAuth(c.user, c.token)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint jwt: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("mint jwt: unexpected status %s", resp.Status)
	}

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, fmt.Errorf("decode jwt: %w", err)
	}

	if out.Token == "" {
		return "", time.Time{}, errors.New("mint jwt: empty token")
	}

	return out.Token, out.ExpiresAt, nil
}

// bearer returns a usable JWT, minting one when the cache is empty, stale or
// force is set. "" when no token is configured: the request goes out anonymously.
func (c *Client) bearer(ctx context.Context, force bool) (string, error) {
	if c.token == "" {
		return "", nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	fresh := c.jwt != "" && time.Now().Before(c.jwtExp.Add(-jwtSkew))
	if fresh && !force {
		return c.jwt, nil
	}

	tok, exp, err := c.mintJWT(ctx)
	if err != nil {
		return "", err
	}

	c.jwt, c.jwtExp = tok, exp
	c.log.Debug("minted garnix jwt", "expires_at", exp)

	return tok, nil
}

// get fetches path into out, re-minting the JWT once if the server rejects the
// cached one.
func (c *Client) get(ctx context.Context, path string, out any) error {
	status, err := c.getOnce(ctx, path, out, false)
	if err == nil {
		return nil
	}

	rejected := status == http.StatusUnauthorized || status == http.StatusForbidden
	if !rejected || !c.Authenticated() {
		return err
	}

	c.log.Debug("garnix rejected cached jwt, re-minting", "path", path, "status", status)

	_, err = c.getOnce(ctx, path, out, true)

	return err
}

// getOnce performs a single attempt, returning the HTTP status so get can
// decide whether a retry with a fresh token is worth it.
func (c *Client) getOnce(ctx context.Context, path string, out any, force bool) (int, error) {
	tok, err := c.bearer(ctx, force)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, fmt.Errorf("build request %s: %w", path, err)
	}

	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("get %s: %w", path, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	// A path segment that does not decode as a Hashid is rejected by the
	// route parser as 400, before any lookup happens. For a caller probing
	// which kind of id it holds, that means the same thing as 404.
	case http.StatusNotFound, http.StatusBadRequest:
		return resp.StatusCode, fmt.Errorf("get %s: %w", path, ErrNotFound)
	default:
		return resp.StatusCode, fmt.Errorf("get %s: unexpected status %s", path, resp.Status)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode %s: %w", path, err)
	}

	return resp.StatusCode, nil
}

// Commit returns every build recorded for a commit hash.
func (c *Client) Commit(ctx context.Context, hash string) (*Commit, error) {
	var out Commit
	if err := c.get(ctx, "/api/commits/"+url.PathEscape(hash), &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// RepoCommits returns the recent commits garnix has built for a repository.
func (c *Client) RepoCommits(ctx context.Context, owner, repo string) ([]Summary, error) {
	var out CommitList

	path := "/api/commits/repo/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}

	return out.Commits, nil
}

// Build returns one build by id.
func (c *Client) Build(ctx context.Context, id string) (*Build, error) {
	var out Build
	if err := c.get(ctx, "/api/build/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// Logs returns one page of a build's output. A zero after fetches from the
// beginning; otherwise only lines strictly newer than after are returned.
func (c *Client) Logs(ctx context.Context, id string, after time.Time) (*Logs, error) {
	path := "/api/build/" + url.PathEscape(id) + "/logs"
	if !after.IsZero() {
		path += "?after=" + url.QueryEscape(after.UTC().Format(time.RFC3339Nano))
	}

	var out Logs
	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// RunLogs returns one page of an action run's output. Runs have no raw-text
// endpoint of their own, so the shape matches Logs.
func (c *Client) RunLogs(ctx context.Context, id string, after time.Time) (*Logs, error) {
	path := "/api/run/" + url.PathEscape(id) + "/logs"
	if !after.IsZero() {
		path += "?after=" + url.QueryEscape(after.UTC().Format(time.RFC3339Nano))
	}

	var out Logs
	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}

	return &out, nil
}
