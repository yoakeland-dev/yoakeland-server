# Yoakeland

Yoakeland is an autonomous, sandboxed code harness backend written in Go. It's a headless daemon that any frontend (CLI, Flutter, QML, web) can drive over a newline-delimited JSON IPC protocol. Running it plainly (`./yoakeland`) always opens a local Unix domain socket that any local client can dial into immediately, no flags required — many clients, sessions, and projects can share one running daemon at once. An optional TCP listener, switched on at runtime with `set_remote_access`, adds remote/multi-device access on top. Nothing about it is tuned with launch flags: the daemon is always running, and its settings are changed over the protocol and remembered. `-stdio` opts into a different, narrower contract instead: a single spawned subprocess speaking the protocol over its own `stdin`/`stdout`.

For the full, authoritative wire protocol, see **[SPEC.md](SPEC.md)** — this file is an orientation and build guide, not the protocol reference.

---

## Table of Contents

1. [Architecture Overview](#architecture-overview)
2. [Building the Binaries](#building-the-binaries)
3. [Running the Backend](#running-the-backend)
   - [Local (the default)](#local-the-default)
   - [Remote (TCP)](#remote-tcp)
   - [Spawn-per-client (-stdio)](#spawn-per-client--stdio)
4. [Connecting Model Providers](#connecting-model-providers)
5. [Interactive CLI Usage (`yoake-client`)](#interactive-cli-usage-yoake-client)
6. [Core Subsystems & Security Model](#core-subsystems--security-model)
   - [Sandbox Boundary](#sandbox-boundary)
   - [Normalizer & Adversarial Defense](#normalizer--adversarial-defense)
   - [Permission Engine & Persistence](#permission-engine--persistence)
   - [Snapshot Engine](#snapshot-engine)
   - [Context Window Manager](#context-window-manager)
   - [Session Storage](#session-storage)
7. [Tools Available to Models](#tools-available-to-models)
8. [MCP Servers & Skills](#mcp-servers--skills)
9. [Multi-Session, Multi-Project](#multi-session-multi-project)
10. [Where to Go Next](#where-to-go-next)

---

## Architecture Overview

```text
┌─────────────────────────────────────────────────────────────┐
│         Frontend / Client (e.g. yoake-client, a GUI)         │
└──────────────────────────┬────────────────────────────────────┘
                            │ local unix socket (always), TCP
                            │ (optional), or -stdio (JSON-Lines)
┌───────────────────────────▼────────────────────────────────────┐
│                    yoakeland (Backend Daemon)                  │
│                                                                  │
│  One session registry × one Agent loop per session, and one    │
│  fully isolated stack (sandbox, permissions, snapshots,        │
│  sessions) per open project — see "Multi-Session, Multi-       │
│  Project" below.                                                │
│                                                                  │
│  ┌─────────────┐ ┌────────────────┐ ┌────────────────────────┐ │
│  │ Agent Loop  │ │ Context Manager│ │ Provider Registry       │ │
│  └──────┬──────┘ └────────────────┘ │ (server-level, ~/.yoake)│ │
│         │                            └────────────────────────┘ │
│  ┌──────▼──────────┐ ┌───────────────────┐                      │
│  │ Model Provider  │ │ Permission Engine │                      │
│  │ (per session)   │ │ (.yoake/perm.json)│                      │
│  └─────────────────┘ └─────────┬──────────┘                     │
│                                 │                                │
│  ┌──────────────────────────────▼──────────────────────────┐   │
│  │                     Tools Engine                        │   │
│  │  [file_read] [file_write] [file_edit] [bash]            │   │
│  └───────┬──────────────┬───────────────┬───────────────────┘   │
│          │              │               │                       │
│  ┌───────▼────────┐ ┌───▼────────────┐ ┌▼─────────────────┐    │
│  │   Normalizer   │ │ Snapshot Store │ │      Sandbox      │    │
│  │(Symlink/Escape)│ │(.yoake/snaps/) │ │ (bwrap/ns/none —  │    │
│  │                │ │                │ │  see below)       │    │
│  └────────────────┘ └────────────────┘ └───────────────────┘    │
└───────────────────────────────────────────────────────────────┘
```

A single server process can serve **many projects at once**, each with its own independent sandbox/permissions/snapshots/sessions, plus **many concurrent sessions**, each running its own turn state machine and free to use a different provider. See [SPEC.md §4a/§4b](SPEC.md) for the full model.

---

## Building the Binaries

Ensure you have **Go 1.22+** installed:

```bash
# Build the backend daemon
go build -o yoakeland ./cmd/yoakeland

# Build the CLI test client
go build -o yoake-client ./cmd/yoake-client
```

To run the test suite:

```bash
go test ./...
```

---

## Running the Backend

### Local (the default)

Just run it — no flags needed to get something a local client can talk to:

```bash
./yoakeland
```

Everything you would tune — the default provider, the model-call and spend limits, whether bash is allowed without a sandbox, remote access — is a runtime setting a client changes over the wire and the daemon persists under `<-state-dir>` (`server.json`); see [SPEC.md §3.1](SPEC.md) (`set_runtime_settings`, `set_default_provider`, `set_remote_access`). The launch flags that remain are the ones that cannot be told over a connection that does not exist yet: `-state-dir`, `-stdio`, `-listen unix:...`, and the `YOAKELAND_MASTER_KEY` environment variable. The old `-provider`, `-model`, `-api-key`, `-base-url`, `-max-session-cost-usd`, `-listen host:port`, `-tls-cert`, `-tls-key` and `-web-root` still work for now as **deprecated seeds**: the first launch that finds nothing stored copies the value into the setting and logs a warning, and after that the stored value wins.

This runs as a long-lived daemon (exits on `SIGINT`/`SIGTERM`, not when any one client disconnects) with a local Unix domain socket always open, at a default path requiring no configuration: `$XDG_RUNTIME_DIR/yoake/yoakeland.sock` on Linux, `$TMPDIR/yoake/yoakeland.sock` on macOS, falling back to `<-state-dir>/yoakeland.sock` (`~/.yoake/yoakeland.sock` by default) when neither env var is set. Pass `-listen unix:/explicit/path.sock` to override that path. No auth token is needed on this socket — see [SPEC.md §2a](SPEC.md) for why (the socket file's `0600` permissions are the boundary — the kernel checks connecting-process UID/GID before the connection is even accepted, same as `ssh-agent`/`dockerd`/Postgres).

`yoake-client` can dial it directly:

```bash
./yoake-client -connect local -session my-session
```

`-connect local` uses the same default-path logic as the daemon; `-connect /explicit/path.sock` connects to a specific socket instead. Without `-connect`, `yoake-client` falls back to spawning `yoakeland -stdio` itself via `-bin` (see below).

### Remote (TCP)

Remote access **adds** a TCP listener alongside the always-on local socket — it doesn't replace it. Turn it on from a client on the local socket (Settings, or `set_remote_access`), and it is remembered across restarts:

```json
{"type": "set_remote_access", "data": {"enabled": true, "addr": ":7890"}}
{"type": "set_remote_password", "data": {"password": "at least sixteen characters"}}
```

Every TCP connection must send an `auth` envelope with the daemon's token as its first frame (generated and persisted the first time remote access is enabled, unless you set a password with `set_remote_password` or register per-person tokens). The listener is always TLS — yoakeland's own name-constrained CA under `<-state-dir>/tls/` unless you give `set_remote_access` a real `tlsCert`/`tlsKey`. See [SPEC.md §2a](SPEC.md).

### Spawn-per-client (`-stdio`)

A different, narrower contract for when a frontend wants to own the whole backend process's lifecycle itself, rather than talk to a persistent daemon — no socket is opened in this mode at all:

```bash
./yoakeland -stdio -project /path/to/repo
```

A frontend spawns this as a child process and speaks the wire protocol over its `stdin`/`stdout` pipes directly; `stderr` carries operational logs, not protocol frames. The process exits when its `stdin` pipe closes (`EOF`), rather than only on `SIGINT`/`SIGTERM`. This is what `yoake-client`'s default `-bin` spawn mode uses under the hood.

---

## Connecting Model Providers

`yoakeland` supports **Anthropic**, **OpenAI**, and **OpenRouter** (which unblocks DeepSeek, Llama, Mistral, Z.ai, etc.) as direct HTTP API providers, local OpenAI-compatible servers (LM Studio, Ollama, vLLM) via `-base-url`, and subscription-billed coding-agent CLIs that run with their own tools — `cli_claude` (Claude Code), `cli_codex` (Codex), the ACP agents `cli_cursor`, `cli_kimi`, `cli_opencode`, `cli_gemini`, `cli_grok`, `cli_deepseek`, and `cli_amp`, `cli_pi` and `cli_ohmypi` (full access only) — driving your own already-authenticated CLI instead of calling an API directly; yoakeland's built-in tools are for the plain API providers. See [SPEC.md §6b](SPEC.md) for why (Anthropic/OpenAI both prohibit using a subscription OAuth token against their APIs directly; driving the official CLI as a subprocess is the sanctioned pattern).

Providers are registered at runtime over the wire (`register_provider`, then `set_provider` for one chat or `set_default_provider` for every new one, [SPEC.md §3.1](SPEC.md)) — the normal path for a client that lets the user add credentials after connecting. Examples of what a client registers:

```json
{"type": "register_provider", "data": {"name": "anthropic", "kind": "api_key", "httpProvider": "anthropic", "apiKey": "sk-ant-..."}}
{"type": "register_provider", "data": {"name": "openrouter", "kind": "api_key", "httpProvider": "openrouter", "apiKey": "sk-or-..."}}
{"type": "register_provider", "data": {"name": "lm-studio", "kind": "api_key", "httpProvider": "openai", "baseUrl": "http://localhost:1234/v1"}}
{"type": "set_default_provider", "data": {"providerName": "anthropic", "model": "claude-sonnet-5"}}
```

No default provider is required to start the server at all: with none configured, sessions must `register_provider` + `set_provider` before their first prompt (SPEC.md's `no_provider` finish reason covers the alternative). If `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY` or `OPENAI_API_KEY` is set in the daemon's environment, it is used as a fallback default until you configure one.

Credentials registered over the wire live in `<-state-dir>/providers.json` (default `~/.yoake/providers.json`), **not** inside any project — they're server-level state shared by every project and session. Set `YOAKELAND_MASTER_KEY` (the `-master-key` flag also works, but is visible in `ps`) to encrypt that file at rest (AES-256-GCM); without it, the server logs a plaintext warning at startup and the file is protected only by its `0600` permissions.

---

## Interactive CLI Usage (`yoake-client`)

`yoake-client` is a minimal terminal client for exercising the protocol — either spawning `yoakeland -stdio` itself (`-bin`, the default; `-stdio` is appended automatically) or connecting to an already-running daemon's local unix socket (`-connect`, see above).

```bash
./yoake-client -bin "./yoakeland -project /path/to/repo" -session feature-login
```

* Session history is persisted server-side at `.yoake/sessions/<session_id>.json` — not held by the client. On connect, `yoake-client` fetches and prints it via `get_session_history` before prompting, so resuming a session shows its prior turns.
* Restarting `yoake-client` with the same `-session` (and, in `-connect` mode, dialing the same server) resumes that conversation.

### Connecting to a Remote Daemon (Multiple Devices, Shared Chats)

Since chat history lives on whichever backend process handles it, the only way for two devices (or two client instances) to see the same conversations is to point them at the **same running daemon** instead of each spawning its own. For the local case, use `-connect local` (see above). For a daemon reachable over the network (remote access enabled, see [Remote (TCP)](#remote-tcp)):

```bash
./yoake-client -connect tcp://your-server:7890 -token <the remote password or token> -ca ~/.yoake/tls/ca.pem -session shared-1
```

* `-ca` is the server's self-managed CA (`<-state-dir>/tls/ca.pem`) — copy it to the client machine once, out-of-band, the same way you'd pair any internal CA (see [SPEC.md §2a](SPEC.md)). Omit `-ca` only if the server was started with a real, publicly-trusted certificate (`tlsCert`/`tlsKey` in `set_remote_access`).
* `yoake-client` remembers `-connect`/`-token`/`-ca`/`-name`/`-session` in `~/.yoake/client.json` after the first successful run, so later invocations on the same device need no flags at all.
* Any other device that dials the same `-connect`/`-token`/`-session` sees the same persisted history via `get_session_history` — that's the whole mechanism, no accounts involved.
* If more than one actual person will connect, give each their own credential instead of sharing one password: register one from a local `yoake-client -connect local` session with `{"type": "register_token", "data": {"name": "aveline", "email": "aveline@example.com"}}`, and hand out the returned token per-person (see [SPEC.md §3.1](SPEC.md), `register_token`/`list_tokens`/`remove_token`). Each named token's identity is then attributed on shared chat messages (`session_history`'s `author` field) and on any `git commit` the agent makes on that person's behalf (`GIT_AUTHOR_NAME`/`EMAIL`), and can be individually revoked (`remove_token`) without rotating everyone else's credential.

### Handling Permissions

When a model requests to mutate a file or execute shell commands, the client pauses and prompts for authorization:

```text
[PERMISSION REQUIRED] file_write (file_write on hello.txt)
Allow? (y/n, append 'r' to remember forever, e.g. 'y' or 'yr'):
```

| Response | Action |
|---|---|
| `y` | Allow this specific invocation once. |
| `n` | Deny this execution (returns an error message to the model). |
| `yr` | Allow **and remember** this exact tool+target pair in `.yoake/permissions.json`. |
| `nr` | Deny **and remember** this exact tool+target pair. |

"Remember" is an exact match (e.g. `file_write:src/main.go`), not a glob or folder-wide rule — allowing one file doesn't cover a sibling file, and allowing one shell command doesn't cover the same binary with different arguments. See [SPEC.md §3.1](SPEC.md).

### Restoring Snapshots (Undo Mutations)

Before any file is created or edited via `file_write` or `file_edit`, Yoakeland creates a pre-mutation snapshot under `.yoake/snapshots/`. Type `restore <snapshot_id>` in the client prompt to roll the filesystem back to it:

```text
> restore snap_1726001234567890000
[SNAPSHOT RESTORED] Snapshot ID: snap_1726001234567890000
```

---

## Core Subsystems & Security Model

### Sandbox Boundary

`bash` tool calls run through a tiered fallback, probed once per project at startup:

1. **Bubblewrap (`bwrap`)**: unprivileged containers with read-only root mounts (`/usr`, `/bin`, `/lib`), `/proc`, `/dev`, a `/tmp` tmpfs, and network isolation (`--unshare-net`). Used if installed.
2. **Linux user/mount namespaces**: fallback if `bwrap` isn't available but namespace cloning is.
3. **No isolation** (`local_restricted_fallback`): if neither of the above works — e.g. macOS/Windows, or a Linux host where both are blocked — commands run directly on the host with nothing beyond the project-root path check the normalizer already does for file tools. **The server logs a warning at startup for any project that falls back to this tier** (the same pattern as the no-TLS/no-master-key warnings), because "sandboxed" is not actually true at that point — a `bash` call can see and touch anything the OS user running yoakeland can. Install `bwrap` if you need real isolation.

Unsandboxed bash stays allowed by default, but it is never quiet about it: the frontend shows a red warning wherever the mode is chosen, marks the run policy as unsandboxed, and puts a chip in the status bar. To take the risk away, turn on the `disableBashWhenUnsandboxed` runtime setting (Settings, or `set_runtime_settings`): in any project on this tier the model is then not given `bash` or the `terminal_*` tools at all. A human's own terminal tab and the coding-agent CLIs (which run under their own sandbox) are not affected.

### Runaway turns

Two runtime settings keep a turn from running away without killing it. `maxRoundsPerTurn` pauses a turn after that many model calls, and `maxSessionCostUsd` pauses it once its chat has spent that much — checked before every model call, not only when a turn starts. Either way the turn asks "Continue?" through the ordinary question machinery (so it also appears in `pending_asks`); Continue allows one more allowance for that turn only, Stop ends it as `limit_reached`, and cancelling drops the question. `0` means no limit. Native coding-agent CLIs run their own loop and are not limited.

### Normalizer & Adversarial Defense

The normalizer (`internal/normalizer/`) sanitizes paths and commands before execution:
- **Path Escape Prevention**: Resolves symlinks progressively to guarantee actions stay within `projectRoot` (blocks `../../etc/passwd` and symlink breakout tricks).
- **POSIX Shell Lexer**: Tokenizes command lines to inspect actual binaries and arguments without shell evaluation. Flags command chaining (`;`, `&&`, `||`, pipes, subshells, `` ` ``, `$`) for mandatory human permission.

### Permission Engine & Persistence

- **Read-Only**: `file_read` is allowed automatically. No `bash` command is — even `git diff` can write (`--output=`), read outside the project (`--no-index`) or run a program (`--ext-diff`).
- **Mutations & Shells**: `file_write`, `file_edit`, and any other `bash` command default to `ask`.
- **Persistence**: decisions saved with `remember: true` are stored in `.yoake/permissions.json` as an exact `toolName:target` key and persist across backend restarts — not a glob or path prefix (see above).

### Snapshot Engine

Live disk snapshots are captured under `.yoake/snapshots/<snap_id>/` before every `file_write`/`file_edit`:
- File contents prior to modification are backed up.
- Newly created files that didn't exist prior to the turn are tracked so that `restore` completely removes them.
- File permissions are preserved across restores.

### Context Window Manager

- Keeps each request's prefix byte-identical to the last one's, so providers bill most of the resent conversation at their prompt-cache rate: cache breakpoints on Anthropic, `prompt_cache_key` on OpenAI, automatic caching on OpenRouter's Claude models.
- Measures fullness with the provider's own count of the last request, falling back to ~4 chars/token.
- Truncates large tool outputs to a head+tail excerpt that names the cut line range; `file_read` pages long files itself (numbered lines, 2000 lines or 14 KB per call, with where to continue).
- Compacts as recorded history events, never by rewriting history: first old tool results are stubbed, and only if that isn't enough is everything summarized by the `utility` model. Between compactions nothing changes, so the cache stays warm. See [SPEC.md §4g](SPEC.md).
- Appends the project's `AGENTS.md` (or `CLAUDE.md`) to the system prompt.

**Model roles** ([SPEC.md §4f](SPEC.md)): `set_model_roles` assigns other models to `plan` (plan-mode turns), `explore` (read-only subagents), `utility` (titles, compaction summaries) and `advisor` (Anthropic's advisor tool) — plan on a strong model, explore and summarize on a cheap one, carry out the plan on the session's own model.

### Session Storage

Turn history is **server-side state**, persisted per-project at `.yoake/sessions/<sessionId>.json`, and is what makes reconnecting from a different device or after a restart work at all. The wire protocol does not automatically push a session's past turns to a newly connected client, though — only new events from the point a connection starts watching a session. Call `get_session_history` to fetch a session's stored transcript explicitly; it returns an empty list (not an error) for a session with no history yet, so a client can call it unconditionally on open. This is the same mechanism whether the client is local or remote — the server is always the source of truth, and any client-side cache of a transcript should be treated as just that, a cache, reconciled against `get_session_history` rather than assumed correct.

---

## Tools Available to Models

| Tool | Permission | Description |
|---|---|---|
| `file_read` | **Allow** | Reads file contents relative to project root, with line numbers, a page at a time. Symlink safe. |
| `search` | **Allow** | Regex search across project files (`path:line: text`), or file listing; available in read-only modes too. |
| `file_write` | **Ask** | Creates or overwrites a file. Triggers a pre-mutation snapshot. |
| `file_edit` | **Ask** | Exact string search-and-replace (`oldString` -> `newString`). Validates single vs multiple occurrences. Triggers a snapshot. |
| `bash` | **Ask / Dynamic** | Executes shell commands through the sandbox tier described above. |
| `web_search` / `web_fetch` | **Allow** / **Ask** (per host) | Client-side web search/fetch; togglable per project (`set_web_tools_enabled`), output wrapped as untrusted content. A fetch asks because `file_read` doesn't — otherwise an injected instruction could read a secret and send it out in a URL; "remember" covers the whole host. |
| `ask_user_question` | **Allow** | Pauses the turn for user input; available in every mode. |
| `write_plan` | **Allow** | Plan mode's own output channel — records a plan to `.yoake/plans/<sessionId>.md`. |
| `terminal_start` / `terminal_output` / `terminal_stop` | **Ask** (start, typed input) / **Allow** (reading, stopping its own) | Runs a command in a real, persistent terminal the user also sees as a tab — for dev servers, watchers, long builds. See [SPEC.md §4e](SPEC.md). |
| `browser` | **Allow** (`evaluate`, and `navigate` off localhost: **Ask**, per host) | Drives the desktop app's browser panel — navigate, snapshot, click/type by ref — e.g. to check a dev server's page. Needs the desktop app connected. |
| `skill` | **Allow** | Loads a skill's instructions (see below). Only offered when a skill exists. What the instructions then ask for goes through the permission checks like anything else. |
| `mcp__<server>__<tool>` | **Ask** (every call) | A tool from an MCP server (see below). A permission rule can allow one tool or a whole server; read-only and plan chats are offered only the tools a server marks read-only. |
| `subagent` | **Allow** | Hands a self-contained task to a parallel helper agent (API providers; CLI agents use their own). `kind: "explore"` is read-only and runs on the `explore` model role. Its own tool calls still ask as usual. |

**Six session modes** (`set_mode`, [SPEC.md §4c](SPEC.md)) — `read_only`, `read_write` (the default), `accept_edits`, `auto` (a CLI agent's own reviewer decides; only where it has one), `autonomous`, and `plan` — control which tools the model is even given and how bash's permission behaves: `read_only`/`plan` restrict the model to a read-only allowlist by not sending the model any other tool's definition at all, rather than exposing every tool and denying mutations at call time; `read_write` forces `bash` to always ask (with `remember` never persisted for that forced ask); `accept_edits` is `read_write` with file edits let through unasked; `autonomous` bypasses every permission check outright; `plan` additionally swaps in a distinct system prompt themed around producing a plan, and allowlists `write_plan` for recording it.

---

## MCP Servers & Skills

**MCP servers** ([SPEC.md §4h](SPEC.md)) give the model tools beyond files and shell: a tracker, a database, a browser, your docs. Add one in the desktop app (Settings → MCP servers: a command, an `http` URL or the older `sse` one; or paste the JSON from a server's README), or list it in the project's `.mcp.json`, the file Claude Code uses.

- Servers you add live in `<state-dir>/mcp.json` (mode 0600) and are offered in every project. A repository's `.mcp.json` only *asks* to run servers: nothing starts until you approve exactly that command, and an edit to it asks again.
- A stdio server runs as you, without a shell, with a minimal environment (`PATH`, `HOME`, locale… plus the `env` you gave it, where `${NAME}` reads from yoakeland's own environment). Header and env values are never sent back to a client, only `••••••`; they stay out of the audit log, the chat and command lines.
- Remote servers sign in with OAuth 2.1 (PKCE, dynamic client registration); the redirect goes to the desktop app, so it works against a daemon on another machine.
- Every call asks, like `bash`, unless you allowed that tool or server with a permission rule. A server's own "read-only" hint only decides whether Plan and read-only chats are *offered* the tool. What a server returns is wrapped as untrusted content, and a server's own instructions are shown to you, never fed to the model.
- A server marked **Also give to coding agents** is handed to Claude Code (`--mcp-config`, through a private 0600 file) and to ACP agents, which then run it under their own permissions. Codex, Amp and Pi keep their own MCP configuration.

**Skills** ([SPEC.md §4h](SPEC.md)) are [Agent Skills](https://agentskills.io): folders with a `SKILL.md` that teach the model one kind of task. They are read where other tools keep them (`.agents/skills`, `.claude/skills`, `.codex/skills`, `.cursor/skills`, `.opencode/skills`, `.pi/skills`, and the same under your home folder, plus `<state-dir>/skills`), so a skill written for another tool needs no copying. The model is told each skill's name and description and loads the rest with the `skill` tool; type `$name` in a message to load one yourself. A skill is only offered to agents that would find that folder themselves, and loading one grants nothing: what it asks for is checked like anything else.

---

## Multi-Session, Multi-Project

A single `yoakeland` process is not limited to one conversation or one folder:

- **Sessions** run fully concurrently — a slow turn on one session never blocks another, and `cancel_turn` targets exactly one.
- **Projects** are opened on demand and cached for the life of the process, each with its own independent sandbox boundary, permission engine, snapshot store, and session store. A tool call in one project's sandbox cannot touch another project's files.
- A session is bound to whichever project its first `user_prompt` (or `open_project`) specified, permanently, for that session's lifetime — sending a different project path for an already-bound session is a conflict, not a switch.

See [SPEC.md §4a/§4b](SPEC.md) for the full model, and the wire-protocol reference below it for every event involved (`open_project`, `list_projects`, `list_sessions`, `get_session_history`, `list_files`).

---

## Where to Go Next

- **[SPEC.md](SPEC.md)** — the authoritative wire protocol: every inbound/outbound event, the turn state machine, budget/cost accounting, provider registry semantics, and the full frontend build checklist.
- **[CLIENT_CHECKLIST.md](CLIENT_CHECKLIST.md)** — a step-by-step plan for building a new client from scratch.
