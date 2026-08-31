// Command garnixlogs serves garnix CI build logs as plain text over the
// tailnet, so an agent can read CI output with one unauthenticated curl instead
// of minting a JWT and walking the JSON API.
//
// It runs as its own tsnet node, which makes it reachable from anywhere on the
// tailnet regardless of which host runs it. The tailnet ACL is the only access
// control: every reader sees every log this node's token can fetch.
//
// Configuration is via flags or the matching GARNIXLOGS_-prefixed environment
// variables (e.g. GARNIXLOGS_HOSTNAME). Secrets come only from the environment:
// GARNIX_TOKEN (the garnix API access token) and TS_AUTHKEY (unattended tailnet
// enrolment).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"

	"github.com/kradalby/kra/web"

	"github.com/kradalby/garnixlogs/garnix"
	"github.com/kradalby/garnixlogs/server"
)

func main() { os.Exit(run()) }

func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	lvl := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log)

	cmd := newCommand(log, lvl)

	err := cmd.ParseAndRun(ctx, os.Args[1:], ff.WithEnvVarPrefix("GARNIXLOGS"))
	if err != nil {
		if errors.Is(err, ff.ErrHelp) {
			fmt.Fprintln(os.Stderr, ffhelp.Command(cmd))

			return 0
		}

		log.Error("garnixlogs failed", "err", err)

		return 1
	}

	return 0
}

type config struct {
	hostname     *string
	stateDir     *string
	localAddr    *string
	garnixURL    *string
	garnixUser   *string
	defaultOwner *string
	poll         *time.Duration
	logLevel     *string
	dev          *bool
}

func newCommand(log *slog.Logger, lvl *slog.LevelVar) *ff.Command {
	fs := ff.NewFlagSet("garnixlogs")
	cfg := &config{
		hostname:     fs.StringLong("hostname", "garnixlogs", "tailnet hostname to serve as"),
		stateDir:     fs.StringLong("state-dir", "", "directory for tsnet state (required unless --dev)"),
		localAddr:    fs.StringLong("local-addr", "127.0.0.1:9099", "loopback address for the local HTTP listener"),
		garnixURL:    fs.StringLong("garnix-url", "https://garnix.kradalby.no", "base URL of the garnix server"),
		garnixUser:   fs.StringLong("garnix-user", "kradalby", "GitHub login the access token belongs to"),
		defaultOwner: fs.StringLong("default-owner", "kradalby", "repository owner assumed for a bare repo name"),
		poll:         fs.DurationLong("poll", 2*time.Second, "how often a followed build is re-checked"),
		logLevel:     fs.StringLong("log-level", "info", "log level: debug, info, warn or error"),
		dev:          fs.BoolLong("dev", "serve plain local HTTP only, without joining the tailnet; for testing"),
	}

	return &ff.Command{
		Name:  "garnixlogs",
		Usage: "garnixlogs [FLAGS]",
		Flags: fs,
		Exec: func(ctx context.Context, _ []string) error {
			if err := lvl.UnmarshalText([]byte(*cfg.logLevel)); err != nil {
				return fmt.Errorf("invalid --log-level %q: %w", *cfg.logLevel, err)
			}

			return serve(ctx, log, cfg)
		},
	}
}

var errStateDirRequired = errors.New("--state-dir (or GARNIXLOGS_STATE_DIR) is required")

func serve(ctx context.Context, log *slog.Logger, cfg *config) error {
	if *cfg.stateDir == "" && !*cfg.dev {
		return errStateDirRequired
	}

	token := os.Getenv("GARNIX_TOKEN")
	if token == "" {
		// Not fatal: public repositories resolve anonymously, so a
		// misconfigured token degrades instead of taking the service down.
		log.Warn("GARNIX_TOKEN is unset; only public repositories will resolve")
	}

	gx, err := garnix.New(*cfg.garnixURL, *cfg.garnixUser, token, garnix.WithLogger(log))
	if err != nil {
		return fmt.Errorf("build garnix client: %w", err)
	}

	logs, err := server.New(gx,
		server.WithLogger(log),
		server.WithDefaultOwner(*cfg.defaultOwner),
		server.WithBaseURL("http://"+*cfg.hostname),
		server.WithPollInterval(*cfg.poll))
	if err != nil {
		return fmt.Errorf("build log server: %w", err)
	}

	opts := []web.Option{web.WithLogger(log)}
	if *cfg.stateDir != "" {
		// tsnet defaults its state to $HOME/.config, which is /var/empty under
		// the hardened unit; keep it in the writable state directory instead.
		opts = append(opts, web.WithTailscaleStateDir(filepath.Join(*cfg.stateDir, "tsnet")))
	}

	srv, err := web.NewServer(web.ServerConfig{
		Hostname:        *cfg.hostname,
		LocalAddr:       *cfg.localAddr,
		AuthKey:         os.Getenv("TS_AUTHKEY"),
		EnableTailscale: !*cfg.dev,
	}, opts...)
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}

	srv.HandleFunc("/", logs.Handle)

	log.Info("serving garnix logs",
		"hostname", *cfg.hostname,
		"local_addr", *cfg.localAddr,
		"garnix", *cfg.garnixURL,
		"authenticated", gx.Authenticated(),
		"tailscale", !*cfg.dev)

	return srv.ListenAndServe(ctx)
}
