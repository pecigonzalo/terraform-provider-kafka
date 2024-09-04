{
  description = "A Nix wrapped development environment";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-24.05-small";
    nixpkgs-unstable.url = "github:nixos/nixpkgs/nixos-unstable-small";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, nixpkgs-unstable, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; config.allowUnfree = true; };
        pkgs-unstable = import nixpkgs-unstable { inherit system; config.allowUnfree = true; };
      in
      {
        devShell = pkgs.mkShell
          rec {
            buildInputs = with pkgs;
              [
                go_1_22
                gopls
                goreleaser
                gnumake
                terraform
                topicctl
                kcat
                apacheKafka
              ];
          };
      });
}
