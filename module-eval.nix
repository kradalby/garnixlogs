# Eval-time smoke test for the NixOS module: assemble a minimal system and
# assert the rendered ExecStart, so a broken option or service definition fails
# `nix flake check` without spinning up a VM.
{
  pkgs,
  self,
  nixpkgs,
  system,
}:
let
  inherit (pkgs) lib;

  serviceFor =
    cfg:
    (import (nixpkgs + "/nixos/lib/eval-config.nix") {
      inherit system;
      modules = [
        self.nixosModules.default
        {
          boot.loader.grub.enable = false;
          fileSystems."/" = {
            device = "/dev/sda1";
            fsType = "ext4";
          };
          system.stateVersion = "24.11";
          services.garnixlogs = cfg;
        }
      ];
    }).config.systemd.services.garnixlogs.serviceConfig;

  default = serviceFor {
    enable = true;
    environmentFile = "/run/secrets/garnixlogs";
  };

  custom = serviceFor {
    enable = true;
    hostname = "cilogs";
    garnixURL = "https://ci.example.com";
    defaultOwner = "someone";
  };

  retry = serviceFor {
    enable = true;
    githubAppIdFile = "/run/secrets/app-id";
    githubAppKeyFile = "/run/secrets/app-pk";
  };

  check = cond: msg: if cond then true else throw "module-eval: ${msg}";
in
assert check (lib.hasInfix "--hostname=garnixlogs" default.ExecStart)
  "default ExecStart missing --hostname: ${default.ExecStart}";
assert check (lib.hasInfix "--state-dir=/var/lib/garnixlogs" default.ExecStart)
  "default ExecStart missing --state-dir: ${default.ExecStart}";
assert check (lib.hasInfix "--garnix-url=https://garnix.kradalby.no" default.ExecStart)
  "default ExecStart missing --garnix-url: ${default.ExecStart}";
# The secret must arrive via EnvironmentFile, never as an argv flag, or the
# token lands in the process table and the Nix store.
assert check (
  default.EnvironmentFile == "/run/secrets/garnixlogs"
) "environmentFile did not become EnvironmentFile";
assert check (
  !lib.hasInfix "GARNIX_TOKEN" default.ExecStart
) "token must not appear in ExecStart: ${default.ExecStart}";
assert check (
  custom.EnvironmentFile or null == null
) "EnvironmentFile must be absent when no environmentFile is set";
assert check (lib.hasInfix "--hostname=cilogs" custom.ExecStart)
  "custom ExecStart missing overridden hostname: ${custom.ExecStart}";
assert check (lib.hasInfix "--garnix-url=https://ci.example.com" custom.ExecStart)
  "custom ExecStart missing overridden garnix url: ${custom.ExecStart}";
assert check (lib.hasInfix "--default-owner=someone" custom.ExecStart)
  "custom ExecStart missing overridden default owner: ${custom.ExecStart}";
# The App key reaches the DynamicUser through a systemd credential; its path
# never goes on argv.
assert check (
  retry.LoadCredential == [
    "github-app-id:/run/secrets/app-id"
    "github-app-key:/run/secrets/app-pk"
  ]
) "app files did not become LoadCredential";
assert check (lib.hasInfix "--github-app-key-file=%d/github-app-key" retry.ExecStart)
  "retry ExecStart missing the credential key path: ${retry.ExecStart}";
assert check (
  !lib.hasInfix "/run/secrets/app-pk" retry.ExecStart
) "key path must not appear in ExecStart: ${retry.ExecStart}";
assert check (
  !lib.hasInfix "github-app" default.ExecStart && default.LoadCredential or null == null
) "retry must stay off without app files: ${default.ExecStart}";
pkgs.runCommand "garnixlogs-module-eval-ok" { } "touch $out"
