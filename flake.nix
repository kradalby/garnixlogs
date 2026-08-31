{
  description = "garnixlogs — garnix CI build logs as plain text on the tailnet";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    flake-checks.url = "github:kradalby/flake-checks";
    flake-checks.inputs.nixpkgs.follows = "nixpkgs";
    flake-checks.inputs.flake-utils.follows = "flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      flake-checks,
    }:
    let
      hashes = builtins.fromJSON (builtins.readFile ./flakehashes.json);
    in
    {
      overlays.default = _final: prev: {
        garnixlogs = self.packages.${prev.system}.default;
      };
      nixosModules.default = import ./module.nix self;
    }
    // flake-utils.lib.eachDefaultSystem (
      system:
      let
        # nixpkgs' bare `go` is still 1.26 while this repo targets 1.27, so the
        # Go version is named explicitly everywhere as `go_latest`. The Go dev
        # tools that ship *wrapped with a `go` on PATH* (goimports, via gotools)
        # must be rebuilt against it too: otherwise that wrapper's older `go`
        # sees the 1.27 directive in go.mod and GOTOOLCHAIN=auto tries to fetch
        # a toolchain from inside the network-less treefmt sandbox.
        goOverlay = _final: prev: {
          gofumpt = prev.gofumpt.override { buildGoModule = prev.buildGoLatestModule; };
          gotools = prev.gotools.override {
            buildGoModule = prev.buildGoLatestModule;
            go = prev.go_latest;
          };
        };
        pkgs = import nixpkgs {
          inherit system;
          overlays = [ goOverlay ];
        };
        fc = flake-checks.lib;
        common = {
          inherit pkgs;
          root = ./.;
          pname = "garnixlogs";
          version = "0.1.0";
          vendorHash = hashes.vendor.sri;
          goPkg = pkgs.go_latest;
          subPackages = [ "cmd/garnixlogs" ];
        };
      in
      {
        packages.default = (fc.goBuild common).overrideAttrs (_: {
          meta.mainProgram = "garnixlogs";
        });
        formatter = fc.formatter common;
        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go_latest
            pkgs.gopls
            pkgs.golangci-lint
            pkgs.gofumpt
            (fc.formatter common)
            pkgs.prek
            pkgs.govulncheck
          ];
          shellHook = ''
            # Never fetch a toolchain: a go.mod ahead of nixpkgs' Go must be a
            # loud error, not a silent download from go.dev outside the store.
            export GOTOOLCHAIN=local
          '';
        };
        checks = {
          build = fc.goBuild common;
          gotest = fc.goTest (common // { goRace = true; });
          golangci-lint = fc.goLint common;
          formatting = fc.goFormat common;
        }
        # NixOS module evaluation needs a Linux system.
        // pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          module-eval = import ./module-eval.nix {
            inherit
              pkgs
              self
              nixpkgs
              system
              ;
          };
        };
      }
    );
}
