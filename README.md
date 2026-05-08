# LM Router

`lm-router` is a local-first Go application that turns multiple OpenAI Codex OAuth accounts into a single OpenAI-compatible API endpoint.

It gives you:

- a local HTTP proxy for OpenAI-style clients
- automatic account failover when one Codex account is unavailable
- a terminal UI for managing providers, API keys, settings, and logs
- a CLI for scripting the same operations
- local persistence with SQLite

Today, the project is focused on the OpenAI Codex provider. The long-term shape is a broader multi-provider router, but the current implementation is intentionally narrow and usable.

## Features

- OpenAI-compatible local API:
  - `POST /v1/responses`
  - `POST /v1/chat/completions`
  - `POST /v1/messages`
  - `GET /v1/models`
- Multiple Codex OAuth accounts with ordered routing priority
- Automatic failover on retryable upstream errors such as `429` and `5xx`
- Local API key authentication for client access
- Terminal UI for:
  - adding Codex connections
  - renaming aliases
  - reordering priority with `Shift+Up` / `Shift+Down`
  - testing and refreshing accounts
  - generating local API keys
  - starting and stopping the embedded proxy
  - viewing request and upstream logs
- CLI commands for automation
- Embedded build metadata:
  - `Version`
  - `Commit`
  - `BuildDate`

## Current Scope

This repository currently implements:

- OpenAI Codex OAuth account management
- OpenAI-compatible local proxying
- local TUI and CLI administration
- Linux build automation for `amd64` and `arm64`

This repository does not currently implement:

- web dashboard
- Docker packaging
- non-Codex providers
- cloud tunnel or public hosted deployment flow

## Requirements

- Go `1.25+`
- a local machine that can complete the OpenAI OAuth flow in a browser
- at least one OpenAI Codex account

## Build From Source

```bash
git clone <your-repo-url>
cd lm-router
go build ./cmd/lm-router
```

You can also run directly without producing a binary:

```bash
go run ./cmd/lm-router version
```

## Version Metadata

The project exposes build metadata through:

```go
var (
    Version   = "0.0.1"
    Commit    = "dev"
    BuildDate = "unknown"
)
```

Show it from the CLI:

```bash
go run ./cmd/lm-router version
```

Example output:

```text
Version: 0.0.1
Commit: dev
BuildDate: unknown
```

Release builds can override these values with `-ldflags`.

## Quick Start

### 1. Add a Codex account

```bash
go run ./cmd/lm-router auth add openai-codex --name main --test
```

The CLI will print an OAuth URL. Open it in your browser, complete the authorization flow, then paste the callback URL back into the terminal.

### 2. Create a local API key

```bash
go run ./cmd/lm-router keys create --name local
```

This key is used by your apps to call `lm-router`. It is not your OpenAI key.

### 3. Start the local proxy

```bash
go run ./cmd/lm-router serve --host 127.0.0.1 --port 19090
```

Health check:

```bash
curl http://127.0.0.1:19090/health
```

### 4. Call it from an OpenAI client

Use:

- `base_url = "http://127.0.0.1:19090/v1"`
- `api_key = "<your lm-router local key>"`

## Terminal UI

Launch the TUI:

```bash
go run ./cmd/lm-router tui
```

Optional flags:

```bash
go run ./cmd/lm-router tui --data-dir ~/.lm-router --host 127.0.0.1 --port 19090
```

The TUI provides:

- `Providers`
- `API Keys`
- `Server`
- `Settings`
- `Logs`
- `Codex Config`

Useful TUI behaviors:

- `Up` / `Down` to move
- `Enter` to select
- `Esc` or `Backspace` to go back
- `q` to quit from the home screen
- `Shift+Up` / `Shift+Down` in `Providers` to reorder routing priority

### Add Provider Flow

When adding a provider from the TUI:

- a full OAuth URL is shown on screen
- the full URL is also written to:

```text
~/.lm-router/openai-codex-auth-url.txt
```

This avoids terminal clipping issues on long URLs.

## CLI Commands

### General

```bash
go run ./cmd/lm-router version
go run ./cmd/lm-router serve
go run ./cmd/lm-router tui
```

### Provider Management

```bash
go run ./cmd/lm-router auth add openai-codex --name main
go run ./cmd/lm-router auth list
go run ./cmd/lm-router auth test --name main
go run ./cmd/lm-router auth refresh <account-id>
go run ./cmd/lm-router auth enable <account-id>
go run ./cmd/lm-router auth disable <account-id>
go run ./cmd/lm-router auth move <account-id> --priority 1
go run ./cmd/lm-router auth remove <account-id>
```

### API Keys

```bash
go run ./cmd/lm-router keys create --name local
go run ./cmd/lm-router keys list
go run ./cmd/lm-router keys revoke <key-id>
```

### Codex Client Config Helper

```bash
go run ./cmd/lm-router codex print-config --port 19090 --api-key sk-lm-router-REPLACE_ME
```

## OpenAI SDK Examples

### Python

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:19090/v1",
    api_key="sk-lm-router-REPLACE_ME",
)

response = client.responses.create(
    model="gpt-5.3-codex",
    input="Write a short hello world program in Go."
)

print(response.output_text)
```

### Node.js

```js
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://127.0.0.1:19090/v1",
  apiKey: "sk-lm-router-REPLACE_ME",
});

const response = await client.responses.create({
  model: "gpt-5.3-codex",
  input: "Write a short hello world program in Go.",
});

console.log(response.output_text);
```

## Anthropic SDK Compatibility

`lm-router` accepts Anthropic Messages API requests at `/v1/messages` and routes them through your OpenAI Codex connections.

### Python

```python
from anthropic import Anthropic

client = Anthropic(
    base_url="http://127.0.0.1:19090",
    api_key="sk-lm-router-REPLACE_ME",
)

message = client.messages.create(
    model="gpt-5.5",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello"}],
)

print(message.content[0].text)
```

Use the root server URL as `base_url`; do not append `/v1`.

### Streaming

```python
from anthropic import Anthropic

client = Anthropic(
    base_url="http://127.0.0.1:19090",
    api_key="sk-lm-router-REPLACE_ME",
)

with client.messages.stream(
    model="gpt-5.5",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello"}],
) as stream:
    for text in stream.text_stream:
        print(text, end="")
```

## Routing and Failover

`lm-router` keeps an ordered list of active Codex accounts.

Request flow:

1. try the highest-priority active account
2. if it succeeds, return that response
3. if it fails with a retryable upstream error, try the next account
4. stop when one succeeds or all routable accounts fail

Typical retryable cases include:

- `429`
- upstream `5xx`
- some auth failures after a refresh attempt

Non-retryable request-shape errors usually do not fail over, because the next account would likely return the same result.

## Local Data and Paths

By default, local state is stored under:

```text
~/.lm-router/
```

Important files:

- `lm-router.db`
- `openai-codex-auth-url.txt`

SQLite sidecar files may also appear:

- `lm-router.db-wal`
- `lm-router.db-shm`

These runtime files are excluded by `.gitignore`.

## Logging

The proxy logs:

- inbound local requests
- upstream OpenAI request and response activity

Sensitive `Authorization` headers are redacted before logging.

In TUI mode, logs are shown in the `Logs` screen. In `serve` mode, logs are written to stdout.

## GitHub Actions Build

This repository includes a GitHub Actions workflow at [.github/workflows/build.yml](.github/workflows/build.yml).

Triggers:

- push to `main`
- manual `workflow_dispatch`

Outputs:

- `lm-router-linux-amd64`
- `lm-router-linux-arm64`

The workflow:

- runs `go test ./...`
- injects build metadata with `-ldflags`
- uploads build artifacts

Manual runs can optionally override the version string through the `version` workflow input.

## Development

Run tests:

```bash
go test ./...
```

Build:

```bash
go build ./cmd/lm-router
```

Run the proxy:

```bash
go run ./cmd/lm-router serve
```

Run the TUI:

```bash
go run ./cmd/lm-router tui
```

## Limitations

- The current `models` list is intentionally small and static.
- The proxy is currently centered on the Codex provider flow.
- The project is local-first and not yet packaged as a production deployment system.
- Secrets are stored for local use; treat the local SQLite database as sensitive machine state.

## License

This project is licensed under the [MIT License](./LICENSE).
