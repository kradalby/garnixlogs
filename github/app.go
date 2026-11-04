// Package github re-runs a commit's garnix checks through the GitHub API.
// GitHub accepts a check-suite rerequest only from the App that owns the
// suite, so this signs as the garnix App rather than as any user. garnix then
// receives the same rerequested webhook as GitHub's "Re-run all jobs" button.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
)

// ErrNoSuite means the App has no check suite on the commit to re-run.
var ErrNoSuite = errors.New("no garnix check suite on this commit")

// App calls the GitHub API as one GitHub App.
type App struct {
	id  int64
	key []byte
	api *string // nil: GitHub itself
}

// Option configures an App.
type Option func(*App)

// WithAPIURL points the App at another GitHub API, for tests.
func WithAPIURL(u string) Option {
	return func(a *App) { a.api = &u }
}

// New builds an App from its numeric id and PEM private key.
func New(id string, keyPEM []byte, opts ...Option) (*App, error) {
	appID, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse app id: %w", err)
	}

	a := &App{id: appID, key: keyPEM}
	for _, o := range opts {
		o(a)
	}

	// Fail at startup, not on the first retry.
	if _, err := a.transport(); err != nil {
		return nil, err
	}

	return a, nil
}

// transport signs as the App. Each rerun gets its own: an installation
// transport writes into the App transport it was made from.
func (a *App) transport() (*ghinstallation.AppsTransport, error) {
	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, a.id, a.key)
	if err != nil {
		return nil, fmt.Errorf("load app key: %w", err)
	}

	if a.api != nil {
		atr.BaseURL = *a.api
	}

	return atr, nil
}

func (a *App) client(tr http.RoundTripper) (*github.Client, error) {
	c, err := github.NewClient(github.WithTransport(tr), github.WithTimeout(30*time.Second), github.WithURLs(a.api, nil))
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}

	return c, nil
}

// Rerun re-requests every check suite the App has on the commit and returns
// how many it re-requested.
func (a *App) Rerun(ctx context.Context, owner, repo, sha string) (int, error) {
	atr, err := a.transport()
	if err != nil {
		return 0, err
	}

	app, err := a.client(atr)
	if err != nil {
		return 0, err
	}

	inst, _, err := app.Apps.GetRepositoryInstallation(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("find installation: %w", err)
	}

	itr := ghinstallation.NewFromAppsTransport(atr, inst.GetID())
	// Scoped to this repository and to checks, so the token can do nothing
	// beyond this rerun.
	itr.InstallationTokenOptions = &github.InstallationTokenOptions{
		Repositories: []string{repo},
		Permissions:  &github.InstallationPermissions{Checks: github.Ptr("write")},
	}

	gh, err := a.client(itr)
	if err != nil {
		return 0, err
	}

	suites, _, err := gh.Checks.ListCheckSuitesForRef(ctx, owner, repo, sha, &github.ListCheckSuiteOptions{AppID: &a.id})
	if err != nil {
		return 0, fmt.Errorf("list check suites: %w", err)
	}

	if len(suites.CheckSuites) == 0 {
		return 0, ErrNoSuite
	}

	for _, s := range suites.CheckSuites {
		if _, err := gh.Checks.ReRequestCheckSuite(ctx, owner, repo, s.GetID()); err != nil {
			return 0, fmt.Errorf("rerequest check suite %d: %w", s.GetID(), err)
		}
	}

	return len(suites.CheckSuites), nil
}
