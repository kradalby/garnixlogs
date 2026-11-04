# garnixlogs

garnix CI build logs as plain text, on the tailnet, with no authentication.

One curl gets an agent the output it needs:

```sh
curl http://garnixlogs/dotfiles                    # recent commits
curl http://garnixlogs/$(git rev-parse HEAD)       # summary + every failed build's log
curl http://garnixlogs/qgx4megl                    # one build's log
curl "http://garnixlogs/qgx4megl?follow"           # stream until it finishes
curl -X POST http://garnixlogs/$(git rev-parse HEAD)/retry  # re-run a red commit
```

## Why

garnix authenticates in two steps, and the obvious guess is wrong. The API
access token is **not** a JWT — it is the password half of HTTP Basic auth
against `POST /api/auth/jwt`, which mints the hour-long JWT the rest of the API
wants as a Bearer token.

Sending the access token directly as a Bearer token does not fail. It reads as
an anonymous request, which succeeds on public repositories and returns

```
Commit 742f2b9… not found. Either it doesn't exist, or you don't have access to it.
```

on private ones. That message reads like a missing commit, so a caller gives up
instead of fixing its auth.

This service does the two-step once, caches the JWT, and serves flat text so
nothing downstream has to know any of the above.

## URLs

| URL | Returns |
| --- | --- |
| `/` | this usage |
| `/<build-id>` | one build's log |
| `/<commit-sha>` | commit summary, then the log of every failed build |
| `/<repo>` | recent commits for `kradalby/<repo>` |
| `/<owner>/<repo>` | recent commits, explicit owner |
| `POST /<commit-sha>/retry` | re-run every garnix check on the commit |

Query flags: `?follow` stream until finished · `?all` list successes too ·
`?ansi` keep colour escapes · `?ts` prefix timestamps.

Commit hashes must be the **full 40 characters** — garnix does not resolve
abbreviated hashes, so the repo listing prints them in full.

Build ids and action-run ids are Hashids with a minimum length of 8 and an
alphanumeric alphabet, so a single path segment cannot be classified by shape
alone. Ambiguous segments are probed (build → run → commit → repo) rather than
guessed. A segment that is not a valid Hashid is rejected by garnix with `400`,
not `404`, which the probe treats as a miss.

## Retry

garnix has no retry API of its own. It re-runs a commit when GitHub sends it a
`check_suite` `rerequested` webhook, which is what the "Re-run all jobs" button
does. GitHub accepts that rerequest only from the App that owns the suite, so a
user token gets a 404. With the garnix App's id and key, this service makes the
rerequest as the App, scoped to one repository and to checks.

The whole commit re-runs, not just what failed: builds that passed come back
from cache, failed ones really run, and garnix actions fire again. A red commit's
view ends with the exact `retry:` command when retries are enabled. `GET` on the
retry URL only prints that command, so a guessed or crawled link never starts
CI.

## Access

It joins the tailnet as its own node via `tsnet`, and also listens on loopback.
There is no per-user authentication: **the tailnet ACL is the entire trust
boundary**, and every reader sees every log the configured token can fetch,
private repositories included. garnix redacts nothing from build output. The
same holds for retries: any peer the ACL lets in can re-run CI on any repository
the App is installed on.

## Configuration

Flags, or the matching `GARNIXLOGS_`-prefixed environment variables. Secrets
come only from the environment:

- `GARNIX_TOKEN` — garnix API access token. Without it only public repositories
  resolve; the service still starts.
- `TS_AUTHKEY` — tailnet auth key for unattended enrolment.

The GitHub App comes from files, so the key never sits in the environment:
`--github-app-id-file` and `--github-app-key-file` (PEM) turn retries on, and
the module's `githubAppIdFile` and `githubAppKeyFile` pass them as systemd
credentials.

Run it without joining the tailnet while developing:

```sh
GARNIX_TOKEN=… go run ./cmd/garnixlogs --dev --local-addr=127.0.0.1:9099
```

## Deploying

The flake ships `nixosModules.default`:

```nix
services.garnixlogs = {
  enable = true;
  environmentFile = config.age.secrets.garnixlogs.path;
};
```
