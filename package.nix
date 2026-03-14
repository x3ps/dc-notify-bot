{
  lib,
  buildGoModule,
  makeWrapper,
  deltachat-rpc-server,
}:

# buildGoModule fetches Go dependencies, builds the project, and
# installs the resulting binary into $out/bin.
buildGoModule {
  pname = "dc-notify-bot";
  version = "0.0.1";

  # lib.cleanSource filters out VCS metadata, editor temp files, and
  # other non-source artifacts so that Nix store paths are
  # reproducible and don't change on irrelevant file modifications.
  src = lib.cleanSource ./.;

  # Hash of the vendored dependencies (go.sum content). When
  # dependencies change, build will fail with a hash mismatch —
  # update this value with the hash from the error message or set
  # to lib.fakeHash to get the correct one.
  vendorHash = "sha256-5mCIZgWScxmHhGjgNylXIN40hLz5a0NaV+t2QsiPNHo=";

  # makeWrapper is needed at build time to create the PATH wrapper.
  nativeBuildInputs = [ makeWrapper ];

  # The bot spawns deltachat-rpc-server as a subprocess, so it must
  # be discoverable in PATH. wrapProgram creates a shell wrapper
  # around the binary that prepends deltachat-rpc-server's bin dir.
  postInstall = ''
    wrapProgram $out/bin/dc-notify-bot \
      --prefix PATH : ${lib.makeBinPath [ deltachat-rpc-server ]}
  '';

  meta = with lib; {
    description = "Delta Chat notification bot — forwards webhook payloads as Delta Chat messages";
    homepage = "https://github.com/x3ps/dc-notify-bot";
    license = licenses.mpl20;
    # mainProgram tells nix which binary to use for `nix run`.
    mainProgram = "dc-notify-bot";
    platforms = platforms.linux;
  };
}
