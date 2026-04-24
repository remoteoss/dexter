{
  description = "dexter - Elixir LSP server";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
        };
      in
      {
        packages.default = pkgs.buildGoModule {
          pname = "dexter";
          version = "0.6.0";

          src = ./.;

          nativeBuildInputs = with pkgs; [
            pkg-config
          ];

          buildInputs = with pkgs; [
          ];

          go = pkgs.go_1_26;

          vendorHash = "sha256-18Cuyn9BhoGPVzElUGmE4GUKybm1qV/lA0nVpiGyOOY=";

          overrideModAttrs = old: {
            postBuild = (old.postBuild or "") + ''
              # go mod vendor strips directories without .go files; restore CGO C sources
              chmod -R u+w vendor
              local gomodcache="$GOPATH/pkg/mod"
              local ts_go="$gomodcache/github.com/tree-sitter/go-tree-sitter@v0.25.0"
              local ts_ex="$gomodcache/github.com/elixir-lang/tree-sitter-elixir@v0.3.5"
              cp -r "$ts_go/include" vendor/github.com/tree-sitter/go-tree-sitter/
              cp -r "$ts_go/src" vendor/github.com/tree-sitter/go-tree-sitter/
              cp -r "$ts_ex/src" vendor/github.com/tree-sitter/tree-sitter-elixir/
            '';
          };
        };
      });
}