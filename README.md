# DeltaChat Notify Bot

A webhook-to-DeltaChat message forwarder. Receives HTTP POST requests and delivers them as Delta Chat messages to configured recipients. Supports Slack-compatible JSON payloads and multipart file uploads.

## How it works

The bot starts a `deltachat-rpc-server` subprocess (via the [deltabot-cli-go](https://github.com/deltachat-bot/deltabot-cli-go) framework) that handles all Delta Chat IMAP/SMTP operations.

On startup, each entry in `NOTIFY_BOT_RECIPIENTS` is resolved to a Delta Chat chat:

- **Plain email** — `CreateContact` + `CreateChatByContactId`. Messages are sent immediately but are not end-to-end encrypted.
- **`OPENPGP4FPR:` SecureJoin link** — triggers an async key-verification handshake. The chat is tracked as "pending" until the handshake completes. Webhook deliveries to pending chats are skipped with a `503 Retry-After` response.

The HTTP server runs in a goroutine alongside the Delta Chat event loop. Incoming requests are dispatched to all ready recipients (or a specific subset via the `recipient` field).

## Installation

### Nix flake

```nix
# flake.nix
{
  inputs.dc-notify-bot.url = "github:x3ps/dc-notify-bot";

  outputs = { nixpkgs, dc-notify-bot, ... }: {
    nixosConfigurations.myhost = nixpkgs.lib.nixosSystem {
      modules = [
        dc-notify-bot.nixosModules.default
        {
          services.dc-notify-bot = {
            enable = true;
            email = "bot@example.com";
            passwordFile = "/run/secrets/dc-notify-bot-password";
            recipients = [
              "alice@example.com"
              "OPENPGP4FPR:AABB1234...#a=bob@example.com"
            ];
          };
        }
      ];
    };
  };
}
```

The module creates a dedicated `dc-notify-bot` system user, auto-initializes the Delta Chat account on first start, and runs the bot under a hardened systemd unit.

### Docker

```bash
# Pull from GitHub Container Registry
docker pull ghcr.io/x3ps/dc-notify-bot:latest

# Initialize account (first time only)
docker run --rm -v dc-notify-data:/data \
  ghcr.io/x3ps/dc-notify-bot:latest \
  dc-notify-bot -f /data init bot@example.com PASSWORD

# Run the bot
docker run -d \
  -v dc-notify-data:/data \
  -e NOTIFY_BOT_RECIPIENTS="alice@example.com" \
  -p 8080:8080 \
  ghcr.io/x3ps/dc-notify-bot:latest
```

### From source

Requires Go 1.24+ and `deltachat-rpc-server` in `PATH`.

```bash
git clone https://github.com/x3ps/dc-notify-bot
cd dc-notify-bot
go build -o dc-notify-bot .
```

## Usage

### JSON payload (Slack-compatible)

```bash
# Simple text message to all recipients
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Hello from webhook"}' \
  http://localhost:8080/webhook

# Send to a specific recipient
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Hello","recipient":"alice@example.com"}' \
  http://localhost:8080/webhook

# Send to multiple specific recipients
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Hello","recipients":["alice@example.com","bob@example.com"]}' \
  http://localhost:8080/webhook
```

### Multipart file upload

```bash
# Text with file attachment
curl -F 'text=Check this out' -F 'file=@photo.jpg' \
  http://localhost:8080/webhook

# File only (text defaults to "(empty notification)")
curl -F 'file=@document.pdf' http://localhost:8080/webhook

# File with specific recipient
curl -F 'text=For you' -F 'file=@photo.jpg' -F 'recipient=alice@example.com' \
  http://localhost:8080/webhook

# Send to multiple recipients
curl -F 'text=Team update' -F 'recipient=alice@example.com' -F 'recipient=bob@example.com' \
  http://localhost:8080/webhook
```

## JSON fields

| Field | Required | Description |
|-------|----------|-------------|
| `text` | Yes | Message text (markdown passes through as-is) |
| `recipient` | No | Email address of a specific recipient (must be in `NOTIFY_BOT_RECIPIENTS`) |
| `recipients` | No | Array of recipient email addresses (merged with `recipient` if both present) |

## Multipart fields

| Field | Required | Description |
|-------|----------|-------------|
| `text` | One of `text` or `file` required | Message text |
| `file` | One of `text` or `file` required | File attachment |
| `recipient` | No | Email address of a specific recipient (repeatable for multiple) |

## Error responses

| Status | Meaning |
|--------|---------|
| 400 | Invalid JSON, missing required fields, or unknown recipient |
| 405 | Method not allowed |
| 413 | Payload too large |
| 415 | Unsupported Content-Type (use `application/json` or `multipart/form-data`) |
| 500 | All message sends failed |
| 503 | All recipients have pending SecureJoin handshakes (includes `Retry-After` header) |

## Endpoints

- `POST /webhook` — Send a notification
- `GET /health` — Liveness probe
- `GET /recipients` — List configured recipients with status

## Configuration

| Environment variable | Default | Description |
|---------------------|---------|-------------|
| `NOTIFY_BOT_RECIPIENTS` | (required) | Comma-separated list of recipient email addresses or `OPENPGP4FPR:` SecureJoin URIs |
| `NOTIFY_BOT_LISTEN` | `127.0.0.1:8080` | HTTP listen address |
| `NOTIFY_BOT_MAX_PAYLOAD_BYTES` | `1048576` (1 MiB) | Maximum request body size |

Requires `deltachat-rpc-server` in `PATH`.

## SecureJoin

SecureJoin establishes an end-to-end encrypted chat with key verification. To get an `OPENPGP4FPR:` invite link from the Delta Chat app:

1. Open Delta Chat → **Settings** → **Invite to Delta Chat** (or **QR code**)
2. Tap **Copy link** to get the `OPENPGP4FPR:…` URI
3. Add the URI to `NOTIFY_BOT_RECIPIENTS`

On first contact, the bot initiates a SecureJoin handshake. The chat is marked "pending" until the recipient accepts in their Delta Chat client and the protocol completes automatically. After that, the contact is verified and messages are end-to-end encrypted.

On subsequent restarts, a verified contact is recognized immediately (no new handshake needed) and the chat is ready right away.

## Development

```bash
# Enter dev shell with Go toolchain, gopls, and deltachat-rpc-server
nix develop

# Run tests
go test -v ./...

# Build
nix build

# Manual testing
dc-notify-bot init bot@example.com PASSWORD
dc-notify-bot serve &
curl -X POST http://127.0.0.1:8080/webhook \
  -H 'Content-Type: application/json' \
  -d '{"text":"hello"}'
```

## License

This project is licensed under the **GNU Affero General Public License v3.0**.
See [LICENSE](./LICENSE).

## Third-party libraries (Go modules)

| Library | Version | License | Authors / Copyright holders |
|---|---|---|---|
| `github.com/chatmail/rpc-client-go/v2` | `v2.0.1` | MPL-2.0 | Chatmail contributors |
| `github.com/deltachat-bot/deltabot-cli-go/v2` | `v2.0.0-20260308000653-bc7d68bb83c1` | MPL-2.0 | DeltaChat Bot contributors |
| `github.com/spf13/cobra` | `v1.8.0` | Apache-2.0 | spf13/cobra maintainers and contributors |
| `github.com/cpuguy83/go-md2man/v2` | `v2.0.3` | MIT | Brian Goff and contributors |
| `github.com/creachadair/jrpc2` | `v1.1.2` | BSD-3-Clause | Michael J. Fromberger |
| `github.com/creachadair/mds` | `v0.8.2` | BSD-2-Clause | Michael J. Fromberger |
| `github.com/davecgh/go-spew` | `v1.1.1` | ISC | Dave Collins |
| `github.com/fortytw2/leaktest` | `v1.3.0` | BSD-3-Clause | The Go Authors |
| `github.com/google/go-cmp` | `v0.6.0` | BSD-3-Clause | The Go Authors |
| `github.com/inconshreveable/mousetrap` | `v1.1.0` | Apache-2.0 | inconshreveable contributors |
| `github.com/kr/text` | `v0.2.0` | MIT | Keith Rarick |
| `github.com/pmezard/go-difflib` | `v1.0.0` | BSD-2-Clause | Patrick Mezard |
| `github.com/russross/blackfriday/v2` | `v2.1.0` | BSD-2-Clause | Russ Ross and contributors |
| `github.com/spf13/pflag` | `v1.0.5` | BSD-3-Clause | Alex Ogier; The Go Authors |
| `github.com/stretchr/testify` | `v1.8.2` | MIT | Mat Ryer, Tyler Bunnell, and contributors |
| `go.uber.org/goleak` | `v1.2.0` | MIT | Uber Technologies, Inc. and contributors |
| `go.uber.org/multierr` | `v1.11.0` | MIT | Uber Technologies, Inc. and contributors |
| `go.uber.org/zap` | `v1.26.0` | MIT | Uber Technologies, Inc. and contributors |
| `golang.org/x/sync` | `v0.6.0` | BSD-3-Clause | The Go Authors |
| `gopkg.in/check.v1` | `v0.0.0-20161208181325-20d25e280405` | BSD-2-Clause | Gustavo Niemeyer |
| `gopkg.in/yaml.v3` | `v3.0.1` | MIT or Apache-2.0 | Kirill Simonov and contributors |

## Third-party products and tooling

| Product | Role | License | Authors / Maintainers |
|---|---|---|---|
| `deltachat-rpc-server` | Runtime RPC backend used by the bot | MPL-2.0 | Delta Chat / Chatmail contributors |
| Go toolchain (`go`) | Build and test toolchain | BSD-3-Clause | The Go Authors |
| Nixpkgs (`NixOS/nixpkgs`) | Package source for Nix builds/dev shell | MIT | Nixpkgs contributors |
| `numtide/flake-utils` | Flake output helpers | MIT | Numtide contributors |
| Debian `bookworm-slim` images | Docker build/runtime base image | Mixed free-software licenses | Debian contributors |

Notes:
- Go module versions are taken from `go list -m all`.
- Licenses and authors are based on upstream `LICENSE` metadata and package manifests.
