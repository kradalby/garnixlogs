# garnixlogs

garnix CI build logs as plain text, on the tailnet, with no authentication.

One curl gets an agent the output it needs:

```sh
curl http://garnixlogs/dotfiles                    # recent commits
curl http://garnixlogs/$(git rev-parse HEAD)       # summary + every failed build's log
curl http://garnixlogs/qgx4megl                    # one build's log
curl "http://garnixlogs/qgx4megl?follow"           # stream until it finishes
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

Query flags: `?follow` stream until finished · `?all` list successes too ·
`?ansi` keep colour escapes · `?ts` prefix timestamps.

Commit hashes must be the **full 40 characters** — garnix does not resolve
abbreviated hashes, so the repo listing prints them in full.

Build ids and action-run ids are Hashids with a minimum length of 8 and an
alphanumeric alphabet, so a single path segment cannot be classified by shape
alone. Ambiguous segments are probed (build → run → commit → repo) rather than
guessed. A segment that is not a valid Hashid is rejected by garnix with `400`,
not `404`, which the probe treats as a miss.

## Access

It joins the tailnet as its own node via `tsnet`, and also listens on loopback.
There is no per-user authentication: **the tailnet ACL is the entire trust
boundary**, and every reader sees every log the configured token can fetch,
private repositories included. garnix redacts nothing from build output.

## Configuration

Flags, or the matching `GARNIXLOGS_`-prefixed environment variables. Secrets
come only from the environment:

- `GARNIX_TOKEN` — garnix API access token. Without it only public repositories
  resolve; the service still starts.
- `TS_AUTHKEY` — tailnet auth key for unattended enrolment.

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
