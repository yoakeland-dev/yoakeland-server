# Yoakeland

Yoakeland is an autonomous, sandboxed code harness backend written in Go. It operates as a standalone headless process designed to communicate with any frontend (CLI, Flutter, QML, or web) over newline-delimited JSON IPC on `stdin`/`stdout`.

---

## Table of Contents

1. [Architecture Overview](#architecture-overview)
2. [Building the Binaries](#building-the-binaries)
3. [Connecting Model Providers](#connecting-model-providers)
   - [LM Studio / Local OpenAI Server](#1-lm-studio--local-openai-compatible-ollama-vllm)
   - [Anthropic Claude](#2-anthropic-claude)
   - [OpenRouter](#3-openrouter-deepseek-llama-etc)
   - [OpenAI](#4-openai)
4. [Interactive CLI Usage (`yoake-client`)](#interactive-cli-usage-yoake-client)
   - [Starting a Session](#starting-a-session)
   - [Handling Permissions](#handling-permissions)
   - [Restoring Snapshots (Undo Mutations)](#restoring-snapshots-undo-mutations)
5. [Core Subsystems & Security Model](#core-subsystems--security-model)
   - [Sandbox Boundary](#sandbox-boundary)
   - [Normalizer & Adversarial Defense](#normalizer--adversarial-defense)
   - [Permission Engine & Persistence](#permission-engine--persistence)
   - [Snapshot Engine](#snapshot-engine)
   - [Context Window Manager](#context-window-manager)
6. [Tools Available to Models](#tools-available-to-models)
7. [IPC Wire Protocol Reference](#ipc-wire-protocol-reference)

---

## Architecture Overview

```text
┌─────────────────────────────────────────────────────────┐
│              Frontend / Client (e.g. yoake-client)       │
└───────────────────────────┬─────────────────────────────┘
                            │ stdin / stdout (JSON-Lines)
┌───────────────────────────▼─────────────────────────────┐
│                   yoakeland (Backend Daemon)            │
│  ┌─────────────────┐ ┌───────────────────┐ ┌─────────┐ │
│  │   Agent Loop    │ │ Context Manager   │ │ Session │ │
│  └────────┬────────┘ └───────────────────┘ └─────────┘ │
│           │                                             │
│  ┌────────▼────────┐ ┌───────────────────┐              │
│  │ Model Provider  │ │ Permission Engine │              │
│  │ (Anthropic/OAI) │ │ (.yoake/perm.json)│              │
│  └─────────────────┘ └─────────┬─────────┘              │
│                                │                        │
│  ┌─────────────────────────────▼─────────────────────┐  │
│  │                    Tools Engine                   │  │
│  │  [file_read] [file_write] [file_edit] [bash]      │  │
│  └───────┬──────────────┬───────────────┬────────────┘  │
│          │              │               │               │
│  ┌───────▼────────┐ ┌───▼────────────┐ ┌▼────────────┐  │
│  │   Normalizer   │ │ Snapshot Store │ │   Sandbox   │  │
│  │(Symlink/Escape)│ │(.yoake/snaps/) │ │ (bwrap/ns)  │  │
│  └────────────────┘ └────────────────┘ └─────────────┘  │
└─────────────────────────────────────────────────────────┘


---

## Building the Binaries

Ensure you have **Go 1.22+** installed:

```bash
# Build the backend daemon
go build -o yoakeland ./cmd/yoakeland

# Build the CLI test client
go build -o yoake-client ./cmd/yoake-client
```

To run the automated adversarial test suite:

```bash
go test -v ./...
```

---

## Connecting Model Providers

`yoakeland` natively supports streaming SSE for **Anthropic**, **OpenAI**, and **OpenRouter** (which unblocks DeepSeek, Mistral, Llama, and Z.ai), plus local servers like **LM Studio** and **Ollama**.

### 1. LM Studio / Local OpenAI-Compatible (Ollama, vLLM)

Start your local server in LM Studio (default port `1234`), then pass `-base-url`:

```bash
./yoake-client -bin "./yoakeland -provider openai -base-url http://localhost:1234 -model google/gemma-4-12b-qat"
```

*Note: When using local URLs (`localhost` or `127.0.0.1`), API key validation is automatically relaxed.*

### 2. Anthropic Claude

```bash
export ANTHROPIC_API_KEY="sk-ant-..."

./yoake-client -bin "./yoakeland -provider anthropic -model claude-3-5-sonnet-20241022"
```

### 3. OpenRouter (DeepSeek, Llama, etc.)

```bash
export OPENROUTER_API_KEY="sk-or-..."

./yoake-client -bin "./yoakeland -provider openrouter -model deepseek/deepseek-chat"
```

### 4. OpenAI

```bash
export OPENAI_API_KEY="sk-..."

./yoake-client -bin "./yoakeland -provider openai -model gpt-4o"
```

---

## Interactive CLI Usage (`yoake-client`)

### Starting a Session

Launch `yoake-client` pointing to your project directory. Sessions persist automatically:

```bash
./yoake-client -bin "./yoakeland -project /path/to/repo" -session feature-login
```

* Session history is automatically maintained in `.yoake/sessions/<session_id>.json`.
* Restarting `yoake-client` with the same `-session` flag restores conversation context.

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
| `yr` | Allow **and remember** this decision in `.yoake/permissions.json`. |
| `nr` | Deny **and remember** this decision. |

### Restoring Snapshots (Undo Mutations)

Before any file is created or edited via `file_write` or `file_edit`, Yoakeland creates a pre-mutation snapshot.

To list or view snapshots, inspect `.yoake/snapshots/`. To rollback the filesystem to a snapshot, type `restore <snapshot_id>` in the client prompt:

```text
> restore snap_1726001234567890000
[SNAPSHOT RESTORED] Snapshot ID: snap_1726001234567890000
```

---

## Core Subsystems & Security Model

### Sandbox Boundary
`yoakeland` isolates subprocess execution via a tiered model:
1. **Bubblewrap (`bwrap`) Container**: If installed on Linux, subprocesses run in unprivileged containers with read-only root system mounts (`/usr`, `/bin`, `/lib`), `/proc`, `/dev`, `/tmp` tmpfs, and offline network isolation (`--unshare-net`).
2. **Linux User/Mount Namespaces**: Fallback on Linux systems without `bwrap`.
3. **Local Restricted Boundary**: Directory locking to project root (`cwd`) and sanitized environment variables on macOS/Windows.

### Normalizer & Adversarial Defense
The normalizer (`internal/normalizer/`) sanitizes and checks paths and commands before execution:
- **Path Escape Prevention**: Resolves symlinks progressively to guarantee actions stay within `projectRoot` (blocks `../../etc/passwd` and symlink breakout tricks).
- **POSIX Shell Lexer**: Tokenizes command lines to inspect actual binaries and arguments without shell evaluation. Flags command chaining (`;`, `&&`, `||`, pipes, subshells) for mandatory human permission.

### Permission Engine & Persistence
- **Read-Only**: Operations like `file_read` and safe read-only queries (`ls`, `pwd`, `git status`) are allowed automatically.
- **Mutations & Shells**: `file_write`, `file_edit`, and `bash` default to `ask`.
- **Persistence**: Approved rules saved with `yr` are stored in `.yoake/permissions.json` and persist across backend restarts.

### Snapshot Engine
Live disk snapshots are captured under `.yoake/snapshots/<snap_id>/`:
- File contents prior to modification are backed up.
- Newly created files that didn't exist prior to the turn are tracked so that `restore` completely removes them.
- File permissions are preserved across restores.

### Context Window Manager
- Estimates token usage using a streaming character heuristic.
- Automatically truncates large tool outputs (e.g. giant logs or minified files) to prevent blowing the context window.
- Employs a sliding-window message pruner that preserves system directives and recent turns while compacting middle turns.

---

## Tools Available to Models

| Tool | Permission | Description |
|---|---|---|
| `file_read` | **Allow** | Reads file contents relative to project root. Symlink safe. |
| `file_write` | **Ask** | Creates or overwrites a file. Triggers a pre-mutation snapshot. |
| `file_edit` | **Ask** | Exact string search-and-replace (`oldString` -> `newString`). Validates single vs multiple occurrences. Triggers a snapshot. |
| `bash` | **Ask / Dynamic** | Executes shell commands in the container sandbox. |

---

## IPC Wire Protocol Reference

The backend communicates strictly over `stdin` and `stdout` using newline-delimited JSON frames:

```json
{
  "type": "event_name",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "toolCallId": "call-123",
  "data": { ... }
}
```

### Inbound Events (Client -> Backend)

- **`user_prompt`**: Start a turn.
  ```json
  {"type": "user_prompt", "sessionId": "s1", "turnId": "t1", "data": {"prompt": "Fix bug in main.go"}}
  ```
- **`permission_response`**: Answer a permission query.
  ```json
  {"type": "permission_response", "sessionId": "s1", "turnId": "t1", "toolCallId": "call-1", "data": {"allowed": true, "remember": true}}
  ```
- **`restore_snapshot`**: Roll back to a previous snapshot ID.
  ```json
  {"type": "restore_snapshot", "sessionId": "s1", "data": {"snapshotId": "snap_1726001234"}}
  ```

### Outbound Events (Backend -> Client)

- **`model_delta`**: Streaming text chunk from the LLM.
  ```json
  {"type": "model_delta", "turnId": "t1", "data": {"content": "I will examine..."}}
  ```
- **`tool_call_requested`**: Model has requested a tool.
  ```json
  {"type": "tool_call_requested", "toolCallId": "c1", "data": {"name": "file_read", "args": {"path": "main.go"}}}
  ```
- **`permission_request`**: Backend paused waiting for human authorization.
  ```json
  {"type": "permission_request", "toolCallId": "c1", "data": {"toolName": "file_write", "description": "file_write on hello.txt", "requiresAsk": true}}
  ```
- **`tool_result`**: Output of tool execution.
  ```json
  {"type": "tool_result", "toolCallId": "c1", "data": {"toolName": "file_read", "output": "package main..."}}
  ```
- **`turn_completed`**: Turn finished.
  ```json
  {"type": "turn_completed", "turnId": "t1", "data": {"finishReason": "stop", "sessionId": "s1"}}
  ```
- **`snapshot_restored`**: Snapshot restoration confirmation.
  ```json
  {"type": "snapshot_restored", "data": {"snapshotId": "snap_1726001234"}}
  ```
- **`error`**: Error notification.
  ```json
  {"type": "error", "data": {"message": "Invalid argument", "fatal": false}}
  ```
```