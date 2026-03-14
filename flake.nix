{
  description = "Delta Chat Notify Bot — webhook receiver that forwards notifications via Delta Chat";

  inputs = {
    # nixos-unstable has the latest deltachat-rpc-server and Go.
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    # flake-utils simplifies multi-platform output generation.
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    # eachSystem generates per-system outputs (packages, devShells)
    # for both x86_64 and aarch64 Linux.
    flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" ] (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        dcNotifyBot = pkgs.callPackage ./package.nix { };
      in
      {
        packages = {
          dc-notify-bot = dcNotifyBot;
          default = dcNotifyBot;

          # Unwrapped binary without the deltachat-rpc-server PATH
          # wrapper — used inside the Docker image where
          # deltachat-rpc-server is installed separately.
          dc-notify-bot-bin = dcNotifyBot.overrideAttrs (_: {
            postInstall = "";
          });

          # OCI image built with Nix. buildLayeredImage produces
          # efficient layer caching. cacert provides CA certificates
          # so the bot can verify TLS when connecting to mail servers.
          docker = pkgs.dockerTools.buildLayeredImage {
            name = "dc-notify-bot";
            tag = "latest";
            contents = [
              dcNotifyBot
              pkgs.cacert
            ];
            config = {
              Cmd = [ "${dcNotifyBot}/bin/dc-notify-bot" "serve" ];
              Env = [
                # SSL_CERT_FILE is required because the minimal image
                # has no system certificate store.
                "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
                "NOTIFY_BOT_LISTEN=0.0.0.0:8080"
              ];
              ExposedPorts."8080/tcp" = {};
              Volumes."/data" = {};
              WorkingDir = "/data";
            };
          };
        };

        # nix develop — development shell with Go toolchain, language
        # server, and the bot binary for manual testing.
        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
            pkgs.deltachat-rpc-server
            dcNotifyBot
          ];
          shellHook = ''
            echo "Delta Chat Notify Bot dev shell"
            echo "  go mod tidy                           # update go.sum"
            echo "  dc-notify-bot init bot@example.com pw # configure account"
            echo "  dc-notify-bot serve                   # start the bot"
            echo ""
            echo "Test webhook:"
            echo "  curl -X POST http://127.0.0.1:8080/webhook -H 'Content-Type: application/json' -d '{\"text\":\"hello\"}'"
          '';
        };
      }
    ) // {
      # Overlay adds dc-notify-bot to the nixpkgs package set so
      # that the NixOS module can find it via mkPackageOption.
      overlays.default = final: prev: {
        dc-notify-bot = final.callPackage ./package.nix { };
      };

      # NixOS module — import module.nix and apply the overlay so
      # that the default package option resolves correctly.
      nixosModules.default = {
        imports = [ ./module.nix ];
        nixpkgs.overlays = [ self.overlays.default ];
      };
    };
}
