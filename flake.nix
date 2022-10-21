{
  description = "A Nix wrapperd development environment";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/22.05";
    nixpkgs-unstable.url = "github:nixos/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, nixpkgs-unstable, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        pkgs-unstable = nixpkgs-unstable.legacyPackages.${system};
      in
      {
        devShell = pkgs.mkShell
          rec {
            buildInputs = with pkgs;
              [
                go_1_18
                gopls
                goreleaser
                gnumake
                terraform
                topicctl
              ];
          };
      });
}
