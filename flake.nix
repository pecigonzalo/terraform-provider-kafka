{
  description = "A Nix wrapped development environment";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-25.11-small";
    # Newer channel. It only provides the lint and vulnerability tools.
    nixpkgs-tools.url = "github:nixos/nixpkgs/nixos-unstable-small";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, nixpkgs-tools, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; config.allowUnfree = true; };
        tools = import nixpkgs-tools { inherit system; };

        # The channel has no Go 1.25 release with the security fixes yet.
        # Remove this override when `go_1_25` in the channel is version
        # 1.25.14 or newer, or when the module moves to Go 1.26.
        go = pkgs.go_1_25.overrideAttrs (_: rec {
          version = "1.25.14";
          src = pkgs.fetchurl {
            url = "https://go.dev/dl/go${version}.src.tar.gz";
            hash = "sha256-noP0T1/ClzeIYbThbMaqEUvordeZP7POssUSOAqk1YI=";
          };
        });
      in
      {
        devShells.default = pkgs.mkShell {
          buildInputs = [
            go
            tools.golangci-lint
            tools.govulncheck
          ] ++ (with pkgs; [
            gopls
            goreleaser
            gnumake
            terraform
            kcat
            apacheKafka
          ]);
        };
      });
}
