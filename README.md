# LM Router

`lm-router` turns multiple OpenAI Codex OAuth accounts into one local, OpenAI-compatible API endpoint.

It provides ordered account routing, automatic failover, local API-key authentication, a terminal UI, and SQLite persistence. The project currently focuses on OpenAI Codex accounts.

## Features

- OpenAI-compatible endpoints:
  - `POST /v1/responses`
  - `POST /v1/chat/completions`
  - `GET /v1/models`
- Anthropic-compatible `POST /v1/messages`
- Multiple Codex accounts with configurable priority
- Failover on retryable errors such as `429` and upstream `5xx`
- Account testing, token refresh, and quota display
- CLI and terminal UI for providers, API keys, settings, and logs
- Sensitive authorization headers are redacted from logs

## Requirements

- Go 1.25+
- A browser for the Codex OAuth flow
- At least one OpenAI Codex account

## Quick Start

Add a Codex account:

```bash
go run ./cmd/lm-router auth add openai-codex --name main --test
```

Open the printed OAuth URL, authorize the account, and paste the callback URL into the terminal.

Create a local API key:

```bash
go run ./cmd/lm-router keys create --name local
```

Save the printed `Secret`. This key authenticates clients to `lm-router`; it is not an OpenAI API key.

Start the proxy:

```bash
go run ./cmd/lm-router serve --host 127.0.0.1 --port 19090
```

Verify it:

```bash
curl http://127.0.0.1:19090/health
```

## Use with Codex CLI

Keep `lm-router` running, then export the local key:

```bash
export LM_ROUTER_API_KEY="sk-lm-router-REPLACE_ME"
```

Add the provider to the user-level `~/.codex/config.toml`. Merge it with any existing configuration:

```toml
model = "gpt-5.3-codex"
model_provider = "lm-router"

[model_providers.lm-router]
name = "LM Router"
base_url = "http://127.0.0.1:19090/v1"
env_key = "LM_ROUTER_API_KEY"
wire_api = "responses"
```

Use the user-level file: Codex ignores `model_provider` and `model_providers` in project-level `.codex/config.toml` files. See the [Codex configuration reference](https://developers.openai.com/codex/config-reference/).

Using `env_key` keeps the router key separate from `~/.codex/auth.json`, preserving any existing Codex login.

Test the authenticated endpoint:

```bash
curl \
  -H "Authorization: Bearer $LM_ROUTER_API_KEY" \
  http://127.0.0.1:19090/v1/models
```

Start a new Codex session:

```bash
codex
```

## Client URLs

| Client | Base URL | API |
| --- | --- | --- |
| Codex CLI | `http://127.0.0.1:19090/v1` | Responses |
| OpenAI SDK | `http://127.0.0.1:19090/v1` | Responses or Chat Completions |
| Anthropic SDK | `http://127.0.0.1:19090` | Messages |

The Anthropic SDK base URL must not include `/v1`; the SDK appends the Messages path itself.

Example with the OpenAI Python SDK:

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:19090/v1",
    api_key="sk-lm-router-REPLACE_ME",
)

response = client.responses.create(
    model="gpt-5.3-codex",
    input="Write a hello world program in Go.",
)
print(response.output_text)
```

## Terminal UI

```bash
go run ./cmd/lm-router tui
```

Optional:

```bash
go run ./cmd/lm-router tui \
  --data-dir ~/.lm-router \
  --host 127.0.0.1 \
  --port 19090
```

The TUI manages providers, routing priority, API keys, the server, settings, logs, and Codex configuration. Provider details can fetch current 5-hour and weekly quota windows.

Long OAuth URLs are also saved to `~/.lm-router/openai-codex-auth-url.txt` to avoid terminal clipping.

## CLI Reference

```bash
# General
go run ./cmd/lm-router version
go run ./cmd/lm-router serve
go run ./cmd/lm-router tui

# Providers
go run ./cmd/lm-router auth add openai-codex --name main
go run ./cmd/lm-router auth list
go run ./cmd/lm-router auth test --name main
go run ./cmd/lm-router auth refresh <account-id>
go run ./cmd/lm-router auth enable <account-id>
go run ./cmd/lm-router auth disable <account-id>
go run ./cmd/lm-router auth move <account-id> --priority 1
go run ./cmd/lm-router auth remove <account-id>

# API keys
go run ./cmd/lm-router keys create --name local
go run ./cmd/lm-router keys list
go run ./cmd/lm-router keys revoke <key-id>

# Codex config helper
go run ./cmd/lm-router codex print-config \
  --port 19090 \
  --api-key sk-lm-router-REPLACE_ME
```

The config helper prints authentication material; treat its output as sensitive. Prefer the environment-variable setup above if you want to preserve an existing Codex login.

## Routing and Local Data

For each request, the router tries enabled accounts in priority order. Retryable failures move to the next available account; non-retryable request errors stop immediately.

State is stored under `~/.lm-router/` by default:

- `lm-router.db`
- `openai-codex-auth-url.txt`
- SQLite `-wal` and `-shm` sidecar files when active

Treat this directory as sensitive because it contains local account credentials.

## Development

```bash
go test ./...
go build ./cmd/lm-router
go run ./cmd/lm-router version
```

GitHub Actions builds Linux `amd64` and `arm64` artifacts on pushes to `main` and manual workflow runs.

The project is local-first and does not currently include a web dashboard, Docker packaging, cloud tunneling, or non-Codex providers.

## License

Licensed under the [MIT License](./LICENSE).
