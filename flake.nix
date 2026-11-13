{
  description = "A Nix wrapped development environment";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-25.11-small";
    # Newer channel. It provides Go and the lint and vulnerability tools. The
    # older channel has no Go release with the security fixes.
    nixpkgs-tools.url = "github:nixos/nixpkgs/nixos-unstable-small";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, nixpkgs-tools, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; config.allowUnfree = true; };
        tools = import nixpkgs-tools { inherit system; };
      in
      {
        devShells.default = pkgs.mkShell {
          buildInputs = [
            tools.go_1_27
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
