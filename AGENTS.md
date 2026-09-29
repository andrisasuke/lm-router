# AGENTS.md

## Purpose

This file gives coding agents the repository-specific context needed to make safe, consistent changes to LM Router. It applies to the entire repository unless a more specific nested `AGENTS.md` overrides it.

## Project Summary

LM Router is a local-first Go application that exposes OpenAI- and Anthropic-compatible APIs over multiple upstream subscription connections.

The repository contains three user interfaces over shared services and storage:

- CLI: `cmd/lm-router/`
- TUI: `internal/tui/`
- Desktop: Wails v3 in `desktop/` with Vue/TypeScript in `frontend/`

Production data defaults to `~/.lm-router/`. Desktop development uses `~/.lm-router-dev/` and port `19091` so it can run beside the packaged app.

## Start Here

Before changing code:

1. Read `README.md`, `Makefile`, and `Taskfile.yml`.
2. Check `git status --short --branch`.
3. Treat all existing modified and untracked files as user work.
4. Inspect the relevant tests before changing behavior.
5. Do not commit, push, merge, delete, or discard changes unless explicitly requested.

Do not launch the interactive desktop app during automated work unless the user explicitly asks. Prefer builds, unit tests, race tests, and vet; the user can perform visual testing.

## Architecture

### Shared backend

- `internal/store/`: SQLite schema, accounts, settings, API keys, cooldowns, and priority updates.
- `internal/app/`: provider services, OAuth orchestration, server lifecycle, logging, and client configuration helpers.
- `internal/proxy/`: HTTP routes, authentication, protocol conversion, streaming, failover, activity tracking, and quota observation.
- `internal/codex/`: Codex HTTP client, token management, request transformations, and quota parsing.
- `internal/anthropic/`: Claude Messages client, token usage, and quota handling.
- `internal/customprovider/`: passthrough clients for static-key compatible providers.
- `internal/tui/`: Bubble Tea UI.

### Desktop

- `desktop/`: Wails application service, window lifecycle, tray menu, development/production profiles, OAuth loopback, and DTOs.
- `frontend/src/`: Vue components, sections, composables, types, and styles.
- `frontend/bindings/`: generated Wails TypeScript bindings. Never edit these files manually.
- `frontend/assets.go`: embeds `frontend/dist/` into desktop builds.
- Desktop Go files use the `wailsapp` build tag.

### Routing invariants

- `gpt*` models route to OpenAI Codex.
- `claude*` models route to Anthropic Claude.
- `<prefix>/<model>` routes to one registered custom provider.
- Failover stays within a provider pool.
- Once streaming output begins, the router must not switch accounts.
- Custom providers are passthrough-only and do not participate in multi-account failover.

## Toolchain

- Go 1.25+
- Wails v3.0.0-beta.16
- Node.js 22+
- Vue 3.5
- TypeScript 6.0.3
- Vite 7

Use the versions declared in `go.mod`, `frontend/package.json`, and lockfiles as the source of truth.

## Common Commands

```bash
make test
make cli
make desktop-dev
make desktop-mac
make desktop-linux
npm run build --prefix frontend
go test -tags wailsapp ./desktop
```

Regenerate Wails bindings after exported desktop service methods or DTOs change:

```bash
wails3 task generate:bindings
```

The generated files must remain consistent with `desktop/`.

## Verification Matrix

Run the smallest relevant checks while iterating, then expand according to risk.

### Documentation or CSS-only

```bash
npm run build --prefix frontend
git diff --check
```

For documentation-only changes, `git diff --check` is usually sufficient unless documented commands or generated output also changed.

### Frontend logic

```bash
npm run build --prefix frontend
git diff --check
```

### Shared Go backend

```bash
go test ./cmd/... ./internal/...
go vet ./internal/...
git diff --check
```

### Desktop Go or Wails bindings

```bash
go test -tags wailsapp ./desktop
npm run build --prefix frontend
go vet -tags wailsapp ./desktop ./internal/...
git diff --check
```

### Concurrency, streaming, failover, activity, or quota

```bash
go test ./cmd/... ./internal/...
go test -race -tags wailsapp ./desktop ./internal/codex ./internal/proxy
go vet -tags wailsapp ./desktop ./internal/...
npm run build --prefix frontend
git diff --check
```

Do not report success without stating which checks actually ran.

## Coding Guidelines

### General

- Keep changes scoped to the requested behavior.
- Preserve backward compatibility across CLI, TUI, and desktop unless the task explicitly changes shared behavior.
- Prefer shared services over duplicating business logic in a UI layer.
- Keep network work and long-running work asynchronous in the desktop app.
- Do not expose secrets in logs, errors, tests, screenshots, or fixtures.
- Use deterministic tests; avoid real provider calls.
- Use `httptest` for upstream behavior and `t.TempDir()` for SQLite tests.
- Use `gofmt` on changed Go files.

### Go

- Propagate `context.Context` through network and database operations.
- Keep provider-specific behavior inside the appropriate package.
- Treat response bodies and streams carefully: close them exactly once and do not buffer streaming paths unnecessarily.
- Preserve failover semantics for retryable versus non-retryable errors.
- Synchronize shared desktop state; quota and activity callbacks can arrive concurrently.
- Build-tag desktop-only code with `//go:build wailsapp`.

### Vue and TypeScript

- Keep `App.vue` focused on composition; place sections, components, and reusable state in their existing directories.
- Put backend calls behind `frontend/src/backend.ts` and composables.
- Update `frontend/src/types.ts` when event or view-model payloads change.
- Keep inputs usable with `autocapitalize="off"`, `autocorrect="off"`, and `spellcheck="false"` where free-form identifiers, URLs, keys, or models are entered.
- The app intentionally disables text selection outside input controls for a native-app feel.
- Preserve the fixed desktop viewport of `980 × 640`; test layouts against that size.
- Do not hand-edit generated bindings.

### SQLite and state

- Use methods on `internal/store.DB`; do not access the database from UI code.
- Store timestamps in UTC.
- Preserve development and production profile isolation.
- Treat cooldown, quota, priority, and failure-state transitions as concurrency-sensitive.
- Any quota window at or above 100% makes a Codex account unavailable until the relevant reset; both windows must recover before the account becomes active.

## Desktop Behavior to Preserve

- Production title/data/port: `LM Router`, `~/.lm-router`, `19090`.
- Development title/data/port: `LM Router Dev`, `~/.lm-router-dev`, `19091`.
- Tray enabled: closing the main window hides it.
- Tray disabled: closing the main window quits.
- The tray setting applies immediately on macOS and Windows; Linux may require restart because of Wails limitations.
- The app must refuse to take over a configured port that already serves LM Router health checks.
- Startup quota loading must not block the UI.
- Normal Codex responses may update quota and connection status asynchronously.

## Generated and Build Artifacts

Do not commit:

- `frontend/node_modules/`
- `frontend/dist/`
- `desktop/build/bin/`
- local SQLite, WAL, SHM, log, or PID files

Generated TypeScript under `frontend/bindings/` is checked in and should be regenerated through Wails when its Go source changes.

Editable icon sources and platform assets live under `desktop/build/`. Do not replace generated icon formats unless the source icon or packaging behavior changes.

## Security

- Never print or commit OAuth tokens, refresh tokens, upstream API keys, LM Router API-key secrets, or SQLite contents.
- Keep local API-key authentication enabled for model endpoints.
- Redact authorization and account-identifying headers in logs.
- Treat `~/.lm-router/` and `~/.lm-router-dev/` as sensitive.
- Reverse-proxy or tunnel documentation must retain TLS and credential warnings.

## Documentation

Update `README.md` when changing:

- supported endpoints or routing rules;
- setup commands or default ports;
- desktop build output or tray behavior;
- provider/model defaults;
- quota, cooldown, or failover behavior;
- toolchain versions or generated-binding workflow.

Keep documentation examples in English and ensure commands match the repository.

## Definition of Done

A change is complete when:

1. The requested behavior is implemented at the correct shared or UI layer.
2. Existing user changes are preserved.
3. Relevant tests and builds pass.
4. Generated bindings are current when required.
5. Documentation is updated for user-visible behavior.
6. No commit is created unless the user requested one.
