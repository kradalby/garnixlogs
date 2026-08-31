# NixOS module for garnixlogs. Import via the flake's nixosModules.default.
#
#   services.garnixlogs = {
#     enable = true;
#     environmentFile = config.age.secrets.garnixlogs.path; # GARNIX_TOKEN=... TS_AUTHKEY=...
#   };
self:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.garnixlogs;
in
{
  options.services.garnixlogs = {
    enable = lib.mkEnableOption "garnixlogs, garnix CI build logs as plain text";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "garnixlogs flake package";
      description = "The garnixlogs package to run.";
    };

    hostname = lib.mkOption {
      type = lib.types.str;
      default = "garnixlogs";
      description = "Tailnet hostname to serve as (its own tsnet node).";
    };

    localAddr = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:9099";
      description = "Loopback address for the local HTTP listener.";
    };

    garnixURL = lib.mkOption {
      type = lib.types.str;
      default = "https://garnix.kradalby.no";
      description = "Base URL of the garnix server to read from.";
    };

    garnixUser = lib.mkOption {
      type = lib.types.str;
      default = "kradalby";
      description = "GitHub login the access token belongs to.";
    };

    defaultOwner = lib.mkOption {
      type = lib.types.str;
      default = "kradalby";
      description = "Repository owner assumed when a URL names a repo without one.";
    };

    logLevel = lib.mkOption {
      type = lib.types.enum [
        "debug"
        "info"
        "warn"
        "error"
      ];
      default = "info";
      description = "Log level.";
    };

    environmentFile = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = ''
        Path to an EnvironmentFile sourced by the service. Carries the secrets:
        GARNIX_TOKEN=... (the garnix API access token, used as the password half
        of Basic auth to mint a JWT) and TS_AUTHKEY=... (unattended tailnet
        enrolment). Keep it out of the Nix store (e.g. an agenix/ragenix secret).

        Without GARNIX_TOKEN the service still starts, but only public
        repositories resolve.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.garnixlogs = {
      description = "garnixlogs — garnix CI build logs as plain text";
      wantedBy = [ "multi-user.target" ];
      after = [
        "network-online.target"
        "nss-lookup.target"
      ];
      wants = [
        "network-online.target"
        "nss-lookup.target"
      ];

      serviceConfig = {
        ExecStart = lib.escapeShellArgs [
          (lib.getExe cfg.package)
          "--hostname=${cfg.hostname}"
          "--state-dir=/var/lib/garnixlogs"
          "--local-addr=${cfg.localAddr}"
          "--garnix-url=${cfg.garnixURL}"
          "--garnix-user=${cfg.garnixUser}"
          "--default-owner=${cfg.defaultOwner}"
          "--log-level=${cfg.logLevel}"
        ];

        DynamicUser = true;
        StateDirectory = "garnixlogs";
        Restart = "always";
        RestartSec = "30s";

        # Graceful shutdown: SIGTERM to the main pid so in-flight followers
        # drain; SIGKILL only mops up stragglers at the stop timeout.
        KillMode = "mixed";
        TimeoutStopSec = "30s";

        # tsnet is a Tailscale node: it needs CAP_NET_ADMIN to set the bypass
        # socket mark (SO_MARK), or it breaks on multi-homed hosts.
        AmbientCapabilities = [ "CAP_NET_ADMIN" ];
        CapabilityBoundingSet = [ "CAP_NET_ADMIN" ];

        # Hardening: this reads one HTTP API and joins the tailnet. It needs
        # outbound network and its own state directory; nothing else.
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        # AF_UNIX is required for tsnet's local API socket.
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
          "AF_NETLINK"
        ];
        RestrictNamespaces = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallFilter = [ "@system-service" ];
        SystemCallErrorNumber = "EPERM";
      }
      // lib.optionalAttrs (cfg.environmentFile != null) {
        EnvironmentFile = cfg.environmentFile;
      };
    };
  };
}
