# Yoakeland

Yoakeland is an autonomous, sandboxed code harness backend written in Go. It's a headless daemon that any frontend (CLI, Flutter, QML, web) can drive over a newline-delimited JSON IPC protocol. Running it plainly (`./yoakeland`) always opens a local Unix domain socket that any local client can dial into immediately, no flags required — many clients, sessions, and projects can share one running daemon at once. An optional TCP listener (`-listen`) adds remote/multi-device access on top. `-stdio` opts into a different, narrower contract instead: a single spawned subprocess speaking the protocol over its own `stdin`/`stdout`.

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
8. [Multi-Session, Multi-Project](#multi-session-multi-project)
9. [Where to Go Next](#where-to-go-next)

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
./yoakeland -project /path/to/repo -provider anthropic -model claude-3-5-sonnet-20241022
```

This runs as a long-lived daemon (exits on `SIGINT`/`SIGTERM`, not when any one client disconnects) with a local Unix domain socket always open, at a default path requiring no configuration: `$XDG_RUNTIME_DIR/yoake/yoakeland.sock` on Linux, `$TMPDIR/yoake/yoakeland.sock` on macOS, falling back to `<-state-dir>/yoakeland.sock` (`~/.yoake/yoakeland.sock` by default) when neither env var is set. Pass `-listen unix:/explicit/path.sock` to override that path. No auth token is needed on this socket — see [SPEC.md §2a](SPEC.md) for why (the socket file's `0600` permissions are the boundary — the kernel checks connecting-process UID/GID before the connection is even accepted, same as `ssh-agent`/`dockerd`/Postgres).

`yoake-client` can dial it directly:

```bash
./yoake-client -connect local -session my-session
```

`-connect local` uses the same default-path logic as the daemon; `-connect /explicit/path.sock` connects to a specific socket instead. Without `-connect`, `yoake-client` falls back to spawning `yoakeland -stdio` itself via `-bin` (see below).

### Remote (TCP)

`-listen <addr>` **adds** a TCP listener alongside the always-on local socket — it doesn't replace it:

```bash
./yoakeland -project /path/to/repo -listen :7890 -tls-cert cert.pem -tls-key key.pem
```

Every TCP connection must send an `auth` envelope with the daemon's token as its first frame (auto-generated and logged at startup if `-token` isn't passed). TCP is **plaintext by default** — pass `-tls-cert`/`-tls-key`, or tunnel the port (SSH/WireGuard), before exposing it beyond `localhost`. See [SPEC.md §2a](SPEC.md).

### Spawn-per-client (`-stdio`)

A different, narrower contract for when a frontend wants to own the whole backend process's lifecycle itself, rather than talk to a persistent daemon — no socket is opened in this mode at all:

```bash
./yoakeland -stdio -project /path/to/repo -provider anthropic -model claude-3-5-sonnet-20241022
```

A frontend spawns this as a child process and speaks the wire protocol over its `stdin`/`stdout` pipes directly; `stderr` carries operational logs, not protocol frames. The process exits when its `stdin` pipe closes (`EOF`), rather than only on `SIGINT`/`SIGTERM`. This is what `yoake-client`'s default `-bin` spawn mode uses under the hood.

---

## Connecting Model Providers

`yoakeland` supports **Anthropic**, **OpenAI**, and **OpenRouter** (which unblocks DeepSeek, Llama, Mistral, Z.ai, etc.) as direct HTTP API providers, local OpenAI-compatible servers (LM Studio, Ollama, vLLM) via `-base-url`, and two subscription-billed CLI-subprocess providers (`cli_claude`, `cli_codex`) that shell out to your own already-authenticated `claude`/`codex` CLI instead of calling an API directly — see [SPEC.md §6b](SPEC.md) for why (Anthropic/OpenAI both prohibit using a subscription OAuth token against their APIs directly; driving the official CLI as a subprocess is the sanctioned pattern).

A provider can be baked in at launch (flags below), or registered at runtime over the wire (`register_provider`/`set_provider`, [SPEC.md §3.1](SPEC.md)) — the normal path for a client that lets the user add credentials after connecting, rather than an operator hardcoding them into the launch command.

```bash
# Anthropic
export ANTHROPIC_API_KEY="sk-ant-..."
./yoakeland -project . -provider anthropic -model claude-3-5-sonnet-20241022

# OpenRouter (DeepSeek, Llama, etc.)
export OPENROUTER_API_KEY="sk-or-..."
./yoakeland -project . -provider openrouter -model deepseek/deepseek-chat

# OpenAI
export OPENAI_API_KEY="sk-..."
./yoakeland -project . -provider openai -model gpt-4o

# Local server (LM Studio, Ollama, vLLM)
./yoakeland -project . -provider openai -base-url http://localhost:1234/v1 -model google/gemma-4-12b-qat
```

No default provider is required to start the server at all — `-provider` can be left unset entirely, in which case sessions must `register_provider` + `set_provider` before their first prompt (SPEC.md's `no_provider` finish reason covers the alternative).

Credentials registered over the wire live in `<-state-dir>/providers.json` (default `~/.yoake/providers.json`), **not** inside any project — they're server-level state shared by every project and session. Pass `-master-key` (or set `YOAKELAND_MASTER_KEY`) to encrypt that file at rest (AES-256-GCM); without it, the server logs a plaintext warning at startup and the file is protected only by its `0600` permissions.

---

## Interactive CLI Usage (`yoake-client`)

`yoake-client` is a minimal terminal client for exercising the protocol — either spawning `yoakeland -stdio` itself (`-bin`, the default; `-stdio` is appended automatically) or connecting to an already-running daemon's local unix socket (`-connect`, see above).

```bash
./yoake-client -bin "./yoakeland -project /path/to/repo" -session feature-login
```

* Session history is persisted server-side at `.yoake/sessions/<session_id>.json` — not held by the client. On connect, `yoake-client` fetches and prints it via `get_session_history` before prompting, so resuming a session shows its prior turns.
* Restarting `yoake-client` with the same `-session` (and, in `-connect` mode, dialing the same server) resumes that conversation.

### Connecting to a Remote Daemon (Multiple Devices, Shared Chats)

Since chat history lives on whichever backend process handles it, the only way for two devices (or two client instances) to see the same conversations is to point them at the **same running daemon** instead of each spawning its own. For the local case, use `-connect local` (see above). For a daemon reachable over the network (`yoakeland -listen :7890`, see [Remote (TCP)](#remote-tcp)):

```bash
./yoake-client -connect tcp://your-server:7890 -token <the -listen token> -ca ~/.yoake/tls/ca.pem -session shared-1
```

* `-ca` is the server's self-managed CA (`<-state-dir>/tls/ca.pem`) — copy it to the client machine once, out-of-band, the same way you'd pair any internal CA (see [SPEC.md §2a](SPEC.md)). Omit `-ca` only if the server was started with a real, publicly-trusted certificate (`-tls-cert`/`-tls-key`).
* `yoake-client` remembers `-connect`/`-token`/`-ca`/`-name`/`-session` in `~/.yoake/client.json` after the first successful run, so later invocations on the same device need no flags at all.
* Any other device that dials the same `-connect`/`-token`/`-session` sees the same persisted history via `get_session_history` — that's the whole mechanism, no accounts involved.
* If more than one actual person will connect, give each their own credential instead of sharing one `-token`: register one from a local `yoake-client -connect local` session with `{"type": "register_token", "data": {"name": "aveline", "email": "aveline@example.com"}}`, and hand out the returned token per-person (see [SPEC.md §3.1](SPEC.md), `register_token`/`list_tokens`/`remove_token`). Each named token's identity is then attributed on shared chat messages (`session_history`'s `author` field) and on any `git commit` the agent makes on that person's behalf (`GIT_AUTHOR_NAME`/`EMAIL`), and can be individually revoked (`remove_token`) without rotating everyone else's credential.

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

### Normalizer & Adversarial Defense

The normalizer (`internal/normalizer/`) sanitizes paths and commands before execution:
- **Path Escape Prevention**: Resolves symlinks progressively to guarantee actions stay within `projectRoot` (blocks `../../etc/passwd` and symlink breakout tricks).
- **POSIX Shell Lexer**: Tokenizes command lines to inspect actual binaries and arguments without shell evaluation. Flags command chaining (`;`, `&&`, `||`, pipes, subshells, `` ` ``, `$`) for mandatory human permission.

### Permission Engine & Persistence

- **Read-Only**: `file_read` and a small safe-command allowlist (`ls`, `pwd`, `echo`, `whoami`, `git status`, `git diff`) are allowed automatically.
- **Mutations & Shells**: `file_write`, `file_edit`, and any other `bash` command default to `ask`.
- **Persistence**: decisions saved with `remember: true` are stored in `.yoake/permissions.json` as an exact `toolName:target` key and persist across backend restarts — not a glob or path prefix (see above).

### Snapshot Engine

Live disk snapshots are captured under `.yoake/snapshots/<snap_id>/` before every `file_write`/`file_edit`:
- File contents prior to modification are backed up.
- Newly created files that didn't exist prior to the turn are tracked so that `restore` completely removes them.
- File permissions are preserved across restores.

### Context Window Manager

- Estimates token usage with a character-count heuristic (~4 chars/token).
- Truncates large tool outputs (giant logs, minified files) to a bounded head+tail excerpt so one tool call can't blow the whole budget.
- Compacts history once it exceeds budget: keeps the first user message and the most recent 4 messages verbatim, and replaces everything in between with an actual text summary of what happened (not just a "N messages dropped" notice) — so a long session degrades to a shorter working memory instead of the model losing that stretch of work outright.

### Session Storage

Turn history is **server-side state**, persisted per-project at `.yoake/sessions/<sessionId>.json`, and is what makes reconnecting from a different device or after a restart work at all. The wire protocol does not automatically push a session's past turns to a newly connected client, though — only new events from the point a connection starts watching a session. Call `get_session_history` to fetch a session's stored transcript explicitly; it returns an empty list (not an error) for a session with no history yet, so a client can call it unconditionally on open. This is the same mechanism whether the client is local or remote — the server is always the source of truth, and any client-side cache of a transcript should be treated as just that, a cache, reconciled against `get_session_history` rather than assumed correct.

---

## Tools Available to Models

| Tool | Permission | Description |
|---|---|---|
| `file_read` | **Allow** | Reads file contents relative to project root. Symlink safe. |
| `file_write` | **Ask** | Creates or overwrites a file. Triggers a pre-mutation snapshot. |
| `file_edit` | **Ask** | Exact string search-and-replace (`oldString` -> `newString`). Validates single vs multiple occurrences. Triggers a snapshot. |
| `bash` | **Ask / Dynamic** | Executes shell commands through the sandbox tier described above. |
| `web_search` / `web_fetch` | **Allow** | Client-side web search/fetch; togglable per project (`set_web_tools_enabled`), output wrapped as untrusted content. |
| `ask_user_question` | **Allow** | Pauses the turn for user input; available in every mode. |
| `write_plan` | **Allow** | Plan mode's own output channel — records a plan to `.yoake/plans/<sessionId>.md`. |

**Four session modes** (`set_mode`, [SPEC.md §4c](SPEC.md)) — `read_only`, `read_write` (the default), `autonomous`, and `plan` — control which tools the model is even given and how bash's permission behaves: `read_only`/`plan` restrict the model to a read-only allowlist by not sending the model any other tool's definition at all, rather than exposing every tool and denying mutations at call time; `read_write` forces `bash` to always ask (bypassing the safe-command auto-allowlist, with `remember` never persisted for that forced ask); `autonomous` bypasses every permission check outright; `plan` additionally swaps in a distinct system prompt themed around producing a plan, and allowlists `write_plan` for recording it.

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
