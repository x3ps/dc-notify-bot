# NixOS module for Delta Chat Notify Bot.
# Declares service options, creates a dedicated system user, and sets
# up a hardened systemd unit that auto-initializes the Delta Chat
# account on first start.
{ config, lib, pkgs, ... }:

let
  cfg = config.services.dc-notify-bot;
in
{
  options.services.dc-notify-bot = {
    # mkEnableOption creates a boolean option defaulting to false.
    enable = lib.mkEnableOption "Delta Chat Notify Bot";

    # mkPackageOption creates a package option that defaults to
    # pkgs.dc-notify-bot (provided by the overlay in flake.nix).
    package = lib.mkPackageOption pkgs "dc-notify-bot" { };

    recipients = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      description = ''
        List of notification recipients. Each entry is either an email address
        or a Delta Chat SecureJoin invite link (OPENPGP4FPR:...).
        To get the link: Delta Chat → Settings → QR code → copy text.

        OPENPGP4FPR links trigger SecureJoin() — end-to-end encrypted chat.
        On restart, already-verified contacts (QrFprOk) are recognized
        automatically — no new handshake is needed.
        Plain email addresses use CreateContact() + CreateChatByContactId() —
        messages in such chats are NOT end-to-end encrypted.

        dclogin: and dcaccount: links are not supported — those are
        account login/setup links, not contact invites.
      '';
      example = [
        "OPENPGP4FPR:AABB1234...#a=alice@example.com"
        "bob@example.com"
      ];
    };

    listenAddress = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:8080";
      description = ''
        Address and port for incoming webhook requests.
        POST /webhook — send notification
        GET  /health  — health check
      '';
      example = "0.0.0.0:9000";
    };

    email = lib.mkOption {
      type = lib.types.str;
      description = ''
        Email address for the bot's Delta Chat account.
        Used only during initial account setup (first start).
      '';
      example = "bot@example.com";
    };

    passwordFile = lib.mkOption {
      type = lib.types.path;
      description = ''
        Path to a file containing the password for the bot's Delta Chat account.
        Used only during initial account setup (first start).
        The file must contain just the password, with no trailing newline.
        Must NOT be a path in the Nix store.
        Loaded via systemd LoadCredential.
      '';
      example = "/run/secrets/dc-notify-bot-password";
    };

    dataDir = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/dc-notify-bot";
      readOnly = true;
      description = "Directory for bot state storage (Delta Chat accounts).";
    };

    maxPayloadBytes = lib.mkOption {
      type = lib.types.nullOr lib.types.ints.positive;
      default = null;
      description = ''
        Maximum webhook payload size in bytes.
        null means use the built-in default of 1 MiB (1048576 bytes).
        Requests exceeding this limit receive a 413 response.
      '';
      example = 524288;
    };
  };

  config = lib.mkIf cfg.enable {
    # Fail early at NixOS evaluation time rather than at runtime.
    assertions = [
      {
        # The bot is useless without at least one recipient.
        assertion = cfg.recipients != [];
        message = "services.dc-notify-bot.recipients must not be empty; provide at least one email address or OPENPGP4FPR: SecureJoin invite link";
      }
      {
        # Nix store paths are world-readable — passwords must not
        # live there. LoadCredential expects a real filesystem path.
        assertion = !lib.hasPrefix "/nix/store" (toString cfg.passwordFile);
        message = "services.dc-notify-bot.passwordFile must not be in /nix/store";
      }
      {
        # StateDirectory is derived by stripping the /var/lib/ prefix,
        # so dataDir must start with it.
        assertion = lib.hasPrefix "/var/lib/" cfg.dataDir;
        message = "services.dc-notify-bot.dataDir must start with /var/lib/ because StateDirectory is derived from it.";
      }
    ];

    # Dedicated system user/group. isSystemUser selects a UID from the
    # system range (< 1000). home is set to dataDir so that
    # deltachat-rpc-server can write its database there.
    users.users.dc-notify-bot = {
      isSystemUser = true;
      group = "dc-notify-bot";
      home = cfg.dataDir;
      description = "Delta Chat Notify Bot service user";
    };

    users.groups.dc-notify-bot = { };

    systemd.services.dc-notify-bot = {
      description = "Delta Chat Notify Bot";
      documentation = [ "https://github.com/x3ps/dc-notify-bot" ];
      # Start after the network is fully online — the bot needs to
      # reach a mail server for Delta Chat IMAP/SMTP.
      after = [ "network-online.target" ];
      # wants (not requires) so the service still starts if
      # network-online.target is not available (e.g. in containers).
      wants = [ "network-online.target" ];
      # Start automatically on boot.
      wantedBy = [ "multi-user.target" ];

      # Pass configuration as env vars. lib.optionalAttrs adds the
      # payload limit only when explicitly set (null = use default).
      environment = {
        NOTIFY_BOT_RECIPIENTS = lib.concatStringsSep "," cfg.recipients;
        NOTIFY_BOT_LISTEN = cfg.listenAddress;
      } // lib.optionalAttrs (cfg.maxPayloadBytes != null) {
        NOTIFY_BOT_MAX_PAYLOAD_BYTES = toString cfg.maxPayloadBytes;
      };

      # gnugrep is needed by ExecStartPre to check whether an account
      # is already configured. deltachat-rpc-server is omitted here
      # because the package wrapper already prepends it to PATH.
      path = [ pkgs.gnugrep ];

      serviceConfig = {
        Type = "simple";
        User = "dc-notify-bot";
        Group = "dc-notify-bot";

        # StateDirectory tells systemd to create and own
        # /var/lib/<name> with correct permissions before the service
        # starts. We strip the /var/lib/ prefix because systemd adds
        # it automatically.
        StateDirectory = lib.removePrefix "/var/lib/" cfg.dataDir;
        StateDirectoryMode = "0750";

        # LoadCredential securely passes the password file into the
        # service's credential store ($CREDENTIALS_DIRECTORY). This
        # avoids exposing the password in environment variables or
        # the process command line.
        LoadCredential = [ "dc-notify-bot-password:${cfg.passwordFile}" ];

        # Account initialization script — runs before the main
        # process. Checks whether a configured account exists; if
        # not, creates one using "dc-notify-bot init". Runs on every
        # start but is idempotent (skips init if account exists).
        ExecStartPre = pkgs.writeShellScript "dc-notify-bot-init" ''
          set -euo pipefail
          BOT="${lib.getExe cfg.package}"
          DATA_DIR="${cfg.dataDir}"

          # "list" outputs lines like "#1 - bot@example.com".
          # If no such line exists, the account is not configured yet.
          if ! "$BOT" -f "$DATA_DIR" list 2>/dev/null | grep -v '(not configured)' | grep -q '^#'; then
            PASS=$(cat "$CREDENTIALS_DIRECTORY/dc-notify-bot-password")
            echo "dc-notify-bot: initializing account ${cfg.email} ..."
            # NOTE: the password is passed as a CLI argument, which means it is
            # briefly visible in /proc/<pid>/cmdline during the init subprocess.
            # This is a known limitation — the upstream CLI does not support
            # reading the password from stdin or a file. Exposure is short-lived
            # (the subprocess exits after init) and mitigated by the dedicated
            # system user, ProtectHome, and PrivateTmp hardening above.
            "$BOT" -f "$DATA_DIR" init "${cfg.email}" "$PASS"
          fi

          # Verify that a configured account exists after init.
          if ! "$BOT" -f "$DATA_DIR" list 2>/dev/null | grep -v '(not configured)' | grep -q '^#'; then
            echo "dc-notify-bot: ERROR — no configured account after init" >&2
            exit 1
          fi
        '';

        # -f sets the data directory; "serve" starts the bot event
        # loop and the HTTP webhook server.
        ExecStart = "${lib.getExe cfg.package} -f ${cfg.dataDir} serve";

        Restart = "on-failure";
        RestartSec = "15s";

        # --- Systemd hardening ---
        # Prevent privilege escalation via setuid/setgid binaries.
        NoNewPrivileges = true;
        # Isolate /tmp so other services cannot read our temp files.
        PrivateTmp = true;
        # Make the entire filesystem read-only except StateDirectory.
        ProtectSystem = "strict";
        # Hide /home, /root, and /run/user from the service.
        ProtectHome = true;
        # Block writes to /proc/sys, /sys, and similar kernel knobs.
        ProtectKernelTunables = true;
        # Deny access to the cgroup filesystem.
        ProtectControlGroups = true;
        # Prevent creating new namespaces (no sandboxing escape).
        RestrictNamespaces = true;
        # Lock the execution domain so personality(2) cannot be used.
        LockPersonality = true;
        # Block mmap(PROT_WRITE|PROT_EXEC) — the bot has no JIT or
        # dynamic code generation, so W^X can be enforced safely.
        MemoryDenyWriteExecute = true;
        # Prevent loading kernel modules.
        ProtectKernelModules = true;
        # Hide kernel log ring buffer (/dev/kmsg, /proc/kmsg).
        ProtectKernelLogs = true;
        # Deny access to physical devices; the bot uses no hardware.
        PrivateDevices = true;
        # Restrict to system-service syscall group (safe baseline).
        SystemCallFilter = [ "@system-service" ];
        # Drop all ambient capabilities — the bot needs none.
        CapabilityBoundingSet = "";
        # Prevent real-time scheduling, which can starve other processes.
        RestrictRealtime = true;
      };
    };
  };
}
