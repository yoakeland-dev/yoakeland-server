# Yoakeland Backend IPC Specification (v2.9)

This document is the complete, authoritative specification for the `yoakeland` backend daemon. It provides all information necessary to build a graphical frontend (e.g., Tauri, Flutter, Electron) that communicates with `yoakeland` over any of **three** transports: a local Unix domain socket (**always available**, one server process, many local clients), an optional TCP socket (remote/multi-device access), or standard input/output (a single spawned subprocess, opt-in via `-stdio`).

All three carry the *identical* newline-delimited JSON envelope protocol and dispatch logic — including the multi-session (§4a) and multi-project (§4b) support. The practical differences: a TCP connection must complete an `auth` handshake first (§2a); `-stdio` mode is inherently limited to one physical connection (there's only one stdin/stdout pipe) and exits when that pipe closes, where the daemon (unix socket, optionally plus TCP) accepts many connections at once and only exits on `SIGINT`/`SIGTERM`. The unix socket is not something you opt into — every invocation of `yoakeland` that isn't `-stdio` opens one automatically, at a default path requiring no configuration (§2a); TCP is the one genuinely optional extra, added on top via `-listen`, never instead of the local socket.

---

## 1. Architectural Overview & Process Model

```text
┌──────────────────────────────────────────────────────────────────┐
│                   Frontend Process (GUI)                         │
│                                                                  │
│  [State Store] ◄─── JSON-lines Parser ◄──── Unix socket / TCP    │
│  [User Action] ───► JSON-lines Writer ────► (or, via -stdio,     │
│                                               a child process's  │
│                                               stdin/stdout)      │
└─────────────────────────────────┬────────────────────────────────┘
                                  │ newline-delimited JSON,
                                  │ any of the three transports (§2a)
┌─────────────────────────────────▼────────────────────────────────┐
│                  yoakeland Daemon (Backend)                      │
│                                                                  │
│  • Normalizer (Argv & Path Sandbox)                              •
│  • Myers Diff Engine                                             •
│  • Permission Engine (.yoake/permissions.json)                   •
│  • Snapshot & Rollback Store (.yoake/snapshots/)                 •
│  • Multi-Turn Session Store (.yoake/sessions/)                   •
│  • Isolated Subprocess Boundary (Bubblewrap / Linux Namespaces)  •
└──────────────────────────────────────────────────────────────────┘
```

**Sandbox tier is a best-effort ladder, not a guarantee.** `bash` tool calls run inside bubblewrap if it's installed, else Linux user/mount namespaces if the kernel allows them, else — if neither is available — directly on the host with **no isolation at all** beyond the path-escape checks the normalizer already does on `file_read`/`file_write`/`file_edit`. The server logs a startup warning per project when it falls back to this last tier, the same pattern as the no-master-key warning (§6c) — insecure-by-omission is always visible, never silent. A frontend that cares about this (e.g. before offering `bash` on an unfamiliar host) can also read `sandboxTier` off `project_opened` (§3.2) rather than only watching the backend's own log output.

### Process Lifecycle
Two fundamentally different contracts, chosen by whether `-stdio` is passed:

- **Daemon mode (the default, no `-stdio`)**: `yoakeland` runs as a long-lived process with a local Unix domain socket always open (default path, or overridden — §2a), and optionally a second TCP listener if `-listen <addr>` is given. It accepts any number of connections concurrently and keeps running regardless of any one of them disconnecting. It exits only on `SIGINT`/`SIGTERM` (or `SIGKILL`), like any ordinary daemon — never because a client went away.
- **`-stdio` mode**: the frontend launches `yoakeland -stdio` as a child process and uses its `stdin`/`stdout` pipes directly as the one and only connection. This is the spawn-a-subprocess-per-client contract: closing the child's `stdin` pipe is a clean shutdown signal (backend receives `EOF` and exits), and `SIGTERM`/`SIGKILL` is the hard-stop path. There is no socket in this mode, local or otherwise — it is not a lighter version of daemon mode, it's a different embedding model entirely, useful when a frontend wants to own the whole process lifecycle itself rather than talk to a persistent background daemon.

1. **Spawn/Connect**: A frontend either spawns `yoakeland -stdio` and uses its pipes directly, or connects to an already-running (or freshly launched, e.g. `yoakeland -project X &`) daemon's local unix socket — no flags needed on the daemon side to make that socket available — or, for remote/multi-device access, dials a daemon's optional TCP listener (`-listen <addr>`) with the `auth` handshake (§2a).
2. **Communication**: All IPC happens strictly via the transport's byte stream (`stdin`/`stdout` in `-stdio` mode, the unix socket or TCP connection otherwise).
3. **Stderr**: In `-stdio` mode, the backend reserves `stderr` for operational logs, panic traces, and low-level sandbox launch logs. The frontend should stream `stderr` to a debugging console or log file. Daemon mode logs to its own process's stderr/stdout (the operator's terminal, a log file, etc.), not to any client connection.
4. **Shutdown**:
   - `-stdio` mode, clean: Close the child process's `stdin` pipe (backend receives `EOF` and cleanly shuts down).
   - `-stdio` mode, hard: Send `SIGTERM` (or `SIGKILL` on timeout).
   - daemon mode: A client simply closes its connection (unix socket or TCP); the server process and any other connected clients are unaffected. The server process itself is stopped like any long-running daemon (`SIGTERM`/`SIGKILL`).

### Invocation & Command-Line Flags

```bash
yoakeland [flags]
```

| Flag | Type | Default | Description |
|---|---|---|---|
| `-project` | `string` | `.` | Path to a project root, pre-opened at startup (failing fast if invalid) and used as the **fallback** project for any session/request that doesn't specify its own `projectPath`. This is not a hard limit — one server process can serve many projects at once, opened on demand; see §4b. |
| `-state-dir` | `string` | `~/.yoake` | Directory for server-level state that belongs to whoever runs the server, not to any one project: `providers.json` (§6b/§6c), `server.json` (identity + remote-access password, §2a), and `tls/` (the auto-managed CA + server certificate for the optional TCP listener, §2a). |
| `-master-key` | `string` | `""` | Passphrase encrypting `providers.json` at rest (AES-256-GCM, scrypt-derived key). Falls back to the `YOAKELAND_MASTER_KEY` env var if unset (prefer the env var — a flag value is visible in `ps`/shell history). Without either, `providers.json` is plaintext, protected only by its 0600 permissions — the server logs a startup warning when this is the case. See §6c. |
| `-provider` | `string` | `auto` | **Optional.** Default LLM provider for sessions that haven't picked a registered one: `anthropic`, `openai`, `openrouter`, or `auto` (auto-detects from environment variables). If this can't be resolved (no matching env var, no flags), the server still starts — it just logs a warning and starts with no default; sessions must `register_provider` + `set_provider` (§3.1) before their first `user_prompt`, which fails with `finishReason: "no_provider"` otherwise (§3.2). This is the normal path when credentials are meant to be registered from the client after connecting, rather than baked into how the process is launched. |
| `-model` | `string` | *(provider default)* | Model ID (e.g. `claude-3-5-sonnet-20241022`, `gpt-4o`, `deepseek/deepseek-chat`). |
| `-api-key` | `string` | `""` | Optional API key override. If omitted, uses environment variables (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, etc.). Leave everything provider-related unset entirely if you intend to register providers only via the wire protocol. |
| `-base-url` | `string` | *(provider default)* | Custom API endpoint. Enables local endpoints like LM Studio (`http://localhost:1234/v1`) or Ollama (`http://localhost:11434/v1`). |
| `-stdio` | `bool` | `false` | Opt into the spawn-a-subprocess contract (§1 Process Lifecycle) instead of the default daemon: speak the protocol directly over this process's own `stdin`/`stdout`, and exit when `stdin` hits `EOF`. No unix socket is opened in this mode. Mutually exclusive with `-listen` in practice (a `-stdio` process never listens on anything). |
| `-listen` | `string` | `""` | Optionally **also** listen here, in addition to the always-on local unix socket (§2a) — ignored if `-stdio` is set. Either a TCP address (e.g. `:7890` or `0.0.0.0:7890`) to add a second, remote/multi-device-reachable listener, or `unix:/path/to/socket` (or bare `unix`/`unix:` to name the default path explicitly) to override where the local socket itself lives, instead of adding anything. |
| `-token` | `string` | *(auto-generated, or whatever was last set via `set_remote_password`)* | Shared secret clients must present in the `auth` handshake (§2a) before any other traffic is processed, for the optional **TCP** listener only. An explicit `-token` always overrides whatever's persisted in `server.json`. If omitted and nothing was ever persisted, one is generated, printed to the server's log at startup, and persisted so it survives a restart. Not used, and not generated, for any unix socket. Can also be rotated live via `set_remote_password` (§3.1) without a restart. |
| `-tls-cert` | `string` | `""` | TLS certificate file for the optional TCP listener. Requires `-tls-key`. Without both, that listener uses yoakeland's own auto-generated, self-managed CA instead of an operator-supplied certificate — never plaintext. See §2a. |
| `-tls-key` | `string` | `""` | TLS private key file for the optional TCP listener. Requires `-tls-cert`. |
| `-max-session-cost-usd` | `float64` | `0` (unlimited) | Refuse to start a session's next turn once it's already spent at or above this amount (per `.yoake/usage.json`). A turn already in flight is never interrupted mid-way — see the `budget_exceeded` finish reason in §3.2. |

---

## 2. Framing & Envelope Protocol

### Framing Rules
- **Encoding**: UTF-8.
- **Delimiter**: Strictly Newline-Delimited JSON (`\n` or `0x0A`). Carriage returns (`\r`) must not precede the newline.
- **Maximum Frame Size**: 10 MB per line (allows for large file reads and tool outputs).
- **Concurrency**: Inbound frames can be sent at any time (e.g., an `EventCancelTurn` can be sent while the backend is streaming `EventModelDelta`).

### The Universal Envelope Schema

Every frame sent in either direction conforms to the following JSON structure:

```typescript
interface Envelope {
  type: EventType;            // Identifies the payload shape
  sessionId?: string;         // Identifies the conversation session
  turnId?: string;            // Correlates a specific turn lifecycle
  toolCallId?: string;        // Correlates a specific tool invocation
  data?: Record<string, any>; // Event-specific payload object
}
```

---

## 2a. Transport Modes & Authentication

### Local Unix domain socket (the default — always available)
Any `yoakeland` invocation that isn't `-stdio` opens a Unix domain socket automatically, with **no flags required** — this is the primary local transport, not an opt-in extra. No `auth` frame, no token, at all: the first envelope on the connection can be a `user_prompt` directly. A client that wants to show up with a real name/picture in the peer list (§3.2 `peer_list`) *may* still send `auth` as its first frame purely to self-report `name`/`deviceType`/`avatar` — `token` is ignored there, since file permissions are already the boundary — but this is opt-in, never required.

- **Default path**: `<runtime-dir>/yoakeland.sock`, where `runtime-dir` prefers the OS's own per-session runtime/temp directory over `-state-dir` — `$XDG_RUNTIME_DIR/yoake` on Linux (tmpfs, per-user, already `0700`, the same convention `ssh-agent`/`pipewire` use for their own sockets), `$TMPDIR/yoake` on macOS (set per login session by the OS) — falling back to `<-state-dir>/yoakeland.sock` when neither env var is set (plain SSH sessions, cron, minimal containers without a login session exporting those vars). The reasoning: a socket is ephemeral runtime state, not persistent data, so it shouldn't default to sitting next to `providers.json` in a directory that gets backed up or may live on a network-mounted home directory, when the OS already offers a better-suited location.
- **Overriding the path**: pass `-listen unix:/path/to/socket` (or bare `-listen unix`/`unix:` to name the default path explicitly). This *replaces* where the local socket lives; it does not toggle whether one exists — one always does, unless `-stdio` is set.
- The parent directory is created (`0700`) if missing, and any stale socket file left by a previous crashed run is removed before binding — a fresh `net.Listen` on a path that already exists otherwise fails with "address already in use."
- `-tls-cert`/`-tls-key` don't apply to a unix socket (there's no network segment for TLS to protect) and are ignored if the local socket path is overridden alongside them.

**Why this is safe without a token:** a TCP listener can't tell *who* dialed it — even bound to `127.0.0.1`, any local process (or, via DNS rebinding, a browser tab) can open it, which is why the token exists there. A Unix domain socket is a filesystem object: the kernel checks the connecting process's UID/GID against the socket file's permissions *before* the connection is even accepted, the same mechanism `dockerd`, PostgreSQL, and `ssh-agent` all rely on for their local sockets. yoakeland creates the socket file at mode `0600` (owner read/write only) — so only the user account that started the server can connect, full stop. There's no analogous per-client credential to check on top of that, which is why the `auth` step is skipped entirely rather than just made optional.

- This only ever means "local" in the sense of "same machine, same OS user." It is not a substitute for the TCP+token+TLS path below for anything crossing a process/user/machine boundary — including a container boundary, where "the same machine" doesn't imply "the same trust domain."
- Cross-platform note: `net.Listen("unix", ...)` is POSIX (Linux/macOS); Windows would need a named-pipe equivalent, which yoakeland does not currently provide — daemon mode on Windows today means the *optional* TCP listener (below) is the only way to reach it locally too, until that gap is closed.

### `-stdio` mode
The opt-in alternative to the daemon above: no socket at all. The first envelope the frontend writes to the child process's `stdin` can be a `user_prompt` directly, same as the unix socket. See §1 Process Lifecycle for why this is a different embedding contract, not a lighter version of the same one.

### Optional TCP listener (remote/multi-device)
`-listen <addr>` (anything not starting with `unix`) **adds** a TCP listener alongside the always-on local socket — it's the one genuinely optional transport, for reaching the daemon across machines or devices. It can also be opened/closed *live*, without restarting the process — see `set_remote_access` (§3.1). Every new TCP connection **must** send an `auth` envelope as its very first frame, before anything else:

```json
{"type": "auth", "data": {"token": "<the -token value>", "name": "aveline's phone", "deviceType": "web", "avatar": "data:image/png;base64,..."}}
```

- If the token matches, the connection proceeds exactly like a local-socket session from that point on — same envelope types, same semantics.
- If it doesn't match (or the first frame isn't `auth`, or it's malformed), the server sends one `error` envelope with `data.fatal: true` and closes the connection. No other envelope type is processed before authentication succeeds.
- One shared token authenticates all TCP clients; there is currently no per-client/per-user token or revocation. Treat the token like a root password to the project: anyone who has it can read/write any file in the project and run shell commands in the sandbox. It can be rotated live via `set_remote_password` (§3.1) — already-connected TCP clients keep their session, only the *next* handshake needs the new one.
- `name`/`deviceType`/`avatar` are optional self-reported identity for the peer registry (§3.2 `peer_list`) — `"web"`, `"desktop"`, or `"server"` for `deviceType`; the backend can't otherwise tell what kind of client dialed in from a raw socket. An omitted `name` gets a generated placeholder.
- `avatar` is a data URL (`data:image/png;base64,...` or similar) — **the client is responsible for downscaling it before sending**, the server does no image processing at all. 96x96 is the house convention (small enough that a base64 PNG comes in well under a kilobyte to a few KB). An oversized value (over ~200KB encoded — see `maxAvatarLen`) is silently dropped rather than failing the whole connection, since it's cosmetic, not functional; the peer just shows up without a picture. This travels in every `peer_list` push to every other peer, so don't send a full-resolution image "just in case" — it costs everyone's bandwidth, not just yours.

### Transport encryption
The optional TCP listener is **TLS by default** — there is no plaintext mode. If you don't pass `-tls-cert`/`-tls-key`, yoakeland generates and manages its own private certificate authority the first time a TCP listener is started, persisted under `<-state-dir>/tls/`:

- `tls/ca.pem` / `tls/ca.key` — a self-signed root CA (ECDSA P-256, ~20-year validity), generated once and reused for the life of the install. `ca.pem` is the file to hand a client for pairing (see below); `ca.key` never leaves the server and is protected the same way `providers.json`/`server.json` are (0600, owner-only).
- `tls/leaf.pem` / `tls/leaf.key` — the actual TLS server certificate the listener presents, signed by that CA (ECDSA P-256, ~1-year validity). Its SAN list covers `localhost`, `127.0.0.1`, `::1`, the machine's hostname, and every non-loopback IP found on a local interface. It's regenerated automatically (no restart needed to trigger it — the next time the TCP listener starts) whenever those SANs change (e.g. the box gets a new LAN IP via DHCP) or the certificate is expiring soon; the CA itself is untouched by this.

This is **not** TOFU/pinning — a client isn't trusting "whatever cert it saw first," it's trusting one specific, durable CA the same way an enterprise or homelab internal CA works. **Pairing a new client**: copy `<-state-dir>/tls/ca.pem` to it once (a local admin can just read the file directly; a remote user is told to copy it over out-of-band — email, a USB stick, a paste into a chat you trust, whatever's convenient) and configure the client's TLS stack to trust *only* that CA for connections to this server (e.g. a non-default `RootCertStore`/root-cert-pool containing just that one certificate) — not the public trust store. The server logs the CA's SHA-256 fingerprint (`AA:BB:CC:...` form) at startup alongside the auth token, so a human can double-check they copied the right file.

Passing both `-tls-cert`/`-tls-key` overrides all of the above with an operator-supplied real certificate instead (unchanged from before) — useful if you have a domain and a certificate from a real CA, or your own internal PKI already. This has no bearing on the local unix socket, which is never wrapped in TLS (there's no network segment to protect).

### Keeping a connection alive
Once connected, a local-socket or TCP connection that's been idle may receive an unsolicited `ping` (§3.2) from the backend roughly every 30 seconds; reply with `pong` echoing the same `nonce` (a client can also send its own `ping` at any time and expects a `pong` back, e.g. to measure round-trip time). Any traffic at all — a real `ping`/`pong`, or just ordinary chat activity — resets the backend's liveness clock for that connection. If nothing is heard from a connection for about 90 seconds, the backend closes it outright, on the assumption it dropped without a clean close (e.g. a phone losing wifi). The backend also closes the connection immediately, without waiting out the 90 seconds, if sending one of these periodic pings itself fails (the write erroring means the connection is already gone). A client that wants to detect a dead backend rather than wait to be dropped should apply the same idea in reverse: if it doesn't hear anything (data or `ping`) from the backend for a while, treat the connection as dead and reconnect. (`-stdio` mode gets no heartbeat — it's a single dedicated pipe, not a pool of independent connections, and EOF is already the liveness signal there.)
- A server process has exactly one auth token (for its optional TCP listener) and one provider registry (§6b/§6c), shared by every session and every connected client — but **not** exactly one project: it can open and serve many projects at once, opened on demand, alongside many concurrent sessions (§4a) and connections, local-socket and TCP alike. See §4b for the full model; don't assume "one project per server" when designing a client.

### Reconnection & session ownership
A client identifies which conversation it wants with `sessionId` on `user_prompt` (and, implicitly, on `permission_response`/`cancel_turn` via the envelope's top-level `sessionId`). If a **different connection** sends a `user_prompt` for a `sessionId` that's already bound to another live connection, the new connection **takes over** — last-writer-wins:

- The new connection starts receiving that session's `model_delta`/`tool_call_requested`/`permission_request`/`turn_completed`/etc. events from then on.
- The connection that got displaced receives a `session_taken_over` event (see §3.2) so it can show "this session is now open elsewhere" instead of silently going dark, or waiting for a `turn_completed` that will never come on that connection again.
- There is no concept of read-only followers or a warning before takeover — the frontend should treat opening the same session from two places as an explicit, immediate handoff, and design its UI accordingly (e.g. confirm before opening a session that might be active elsewhere, if that matters to your UX).

### Peer registry & remote-access settings
Every live connection (unix and TCP alike; `-stdio`'s single pipe never registers one) is a **peer** — see `peer_list` (§3.2) for the full roster shape, `set_peer_name`/`set_peer_avatar` for a peer updating its own profile, and `disconnect_peer` for force-closing another one. `disconnect_peer` has no extra permission check beyond having completed the handshake at all: any peer may disconnect any other, since everyone past that point already shares the one trust boundary (unix file perms, or the single shared TCP token) — there's no per-user identity yet to scope it more tightly against.

A peer's name/avatar aren't fixed at connect time: `set_peer_name`/`set_peer_avatar` update them live (e.g. when the user changes their profile in Settings after already being connected), and every peer sees the change on the next `peer_list` push — a client's own profile UI and its peer-list row for itself should be driven by the same underlying state, not kept as two separate copies.

`set_remote_access` and `set_remote_password` (§3.1) are **local-socket-only** — sent over TCP, both are rejected with `error`, since a remote peer shouldn't be able to grant itself continued remote access or rotate the credential that controls it. `server_identity`/`set_server_identity` (a display name for the server itself, persisted under `-state-dir`) has no such restriction — it's not a security boundary, just a label.

---

## 3. Event Dictionary (Wire Contract)

### 3.1 Inbound Events (Frontend → Backend)

#### `user_prompt`
Initiates a new conversational turn or prompt submission.
- **Correlation**: Frontend provides a unique `turnId` (e.g. `turn-1`) and `sessionId`.
- **`projectPath`**: only meaningful the *first* time this `sessionId` is used — it opens (or reuses) that project and binds the session to it for good. Omit it on later prompts for the same session; a conflicting value later is rejected (§4b).
- **`authorName`/`authorAvatar`** *(optional)*: who is sending this message — the same profile a client already self-reports in the `auth` frame (`name`/`avatar`, §2a). Stored with the user message so `session_history` (§3.2) can render the sender on a transcript repainted later (reload, second device, fresh install), which is the only way an avatar survives at all: it lives in the sending client's own settings and is never otherwise sent. A token-authenticated TCP sender's name still wins over `authorName` (a real credential isn't overridden by a self-report, §2a); on the unix socket — where file permissions are the boundary and there's no credential to attribute to — this is the only source of attribution, and a client that sends neither field gets a stored message with no sender rather than an error. Oversized `authorAvatar` (see `maxAvatarLen`, §2a) is dropped, not rejected: it's cosmetic, and it's stored per message.
```json
{
  "type": "user_prompt",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "data": {
    "prompt": "Refactor the file reader to support streaming",
    "sessionId": "session-main",
    "projectPath": "/home/user/projects/my-repo",
    "authorName": "aveline",
    "authorAvatar": "🐣",
    "autoGenerateTitle": true
  }
}
```
- **`autoGenerateTitle`** *(optional, default the server's own setting — itself `true` unless changed via `set_auto_generate_title`)*: only meaningful on a session's first prompt (the one that leaves it with an empty title) — see "Auto-generated titles" under `set_session_title` below. Ignored on every later prompt in the same session, the same way `projectPath` is.

#### `permission_response`
Answers an outstanding `permission_request` from the backend.
- **Correlation**: `toolCallId` **must** match the `toolCallId` received in `permission_request`.
```json
{
  "type": "permission_response",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "toolCallId": "call_abc123",
  "data": {
    "allowed": true,
    "remember": true,
    "feedback": ""
  }
}
```
- `allowed`: If `true`, the backend executes the tool. If `false`, execution is denied and a denial message is fed to the model.
- `remember`: If `true`, saves the decision in `<project>/.yoake/permissions.json` so the user is not prompted again for this **exact** tool+target pair — the cache key is `toolName:target` (e.g. `file_write:src/main.go`, or a bash command's literal command string), not a glob or directory prefix. "Always Allow" on one file does not cover a sibling file, and "Always Allow" on one shell command does not cover the same binary with different arguments. A frontend's copy shouldn't imply broader scope than that (e.g. avoid "always allow this folder").
- `feedback` *(optional)*: Only meaningful alongside `allowed: false`. Free text explaining *why* — e.g. "use the streaming API instead" — fed to the model as part of the same turn's denial message (`tool_result.data.error`, §3.2) instead of the fixed `"permission denied by user or policy"` string, so the model can course-correct in its next response rather than needing a follow-up prompt to learn the reason. Ignored when `allowed: true`.

#### `cancel_turn`
Immediately halts an ongoing turn.
- **Correlation**: Targets the active `turnId`.
```json
{
  "type": "cancel_turn",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "data": {
    "sessionId": "session-main",
    "turnId": "turn-1"
  }
}
```
*Effect*: Aborts the model's streaming HTTP connection, terminates any child processes executing inside the sandbox via `SIGKILL`, cancels outstanding permission waits, and emits `turn_completed` with `finishReason: "cancelled"`.

#### `restore_snapshot`
Reverts the project filesystem to a previous snapshot state. Resolves its project the same way `user_prompt` does (§4b): `data.projectPath` if given, else whatever project the envelope's `sessionId` is already bound to.
```json
{
  "type": "restore_snapshot",
  "sessionId": "session-main",
  "turnId": "",
  "data": {
    "snapshotId": "snap_1710950400000000"
  }
}
```

#### `list_snapshots`
Enumerates snapshots captured for **one project** (i.e. everything under that project's `.yoake/snapshots/`), for populating a restore-snapshot picker instead of requiring the user to already know a `snapshotId`. Resolves its project the same way `list_sessions` does.
```json
{"type": "list_snapshots", "data": {"projectPath": "/home/user/projects/my-repo"}}
```
- `projectPath`: required unless the server has a default project (`-project`), in which case omitting it lists that default project's snapshots.
- Response: `snapshot_list`.

#### `auth` *(optional TCP listener only, first frame on a new connection)*
See §2a.
```json
{"type": "auth", "data": {"token": "a1b2c3..."}}
```

#### `register_provider`
Registers (or overwrites) a named provider **credential** — not a model choice. See §6b for the full model: one `api_key` config unlocks every model that key can call, so `model` is never part of this payload.
```json
{
  "type": "register_provider",
  "data": {
    "name": "my-anthropic",
    "kind": "api_key",
    "httpProvider": "anthropic",
    "apiKey": "sk-ant-...",
    "authMethod": "manual"
  }
}
```
- `kind`: `"api_key"` (needs `httpProvider` + `apiKey`), `"cli_claude"`, or `"cli_codex"` (subscription-billed, no credential — see §6b).
- Response: `provider_registered` on success, `error` on failure (e.g. missing `name`).

#### `list_providers`
Requests the current provider/credential roster. Takes no data (`{}` is fine).
- Response: `provider_list`, with every `apiKey` scrubbed.

#### `list_models`
Requests the models available through one registered provider's own credential — the frontend should call this to populate a model picker per provider, never hardcode a model list.
```json
{"type": "list_models", "data": {"providerName": "my-anthropic"}}
```
- For `api_key` providers this calls that HTTP backend's own models endpoint live (Anthropic/OpenAI/OpenRouter each expose one, keyed by the same credential used for chat completions) — so newly released models show up with no server-side changes. `cli_claude`/`cli_codex` have no key to query, so their list instead comes from a cached copy of [T3 Code's](https://github.com/pingdotgg/t3code) `model-manifest.json` (MIT-licensed, public, no auth) — see §6c.
- For OpenRouter, any entry whose upstream record carries a populated `alias_target` (a floating alias for another, pinned model — e.g. `anthropic/claude-3.7-sonnet` aliasing `anthropic/claude-3-7-sonnet`) is filtered out server-side, rather than left for the frontend to recognize by convention (the `~` prefix is only how you'd *reference* a floating alias when calling chat completions, not how the `/models` listing marks one). Entries whose `id` collides case-insensitively with one already returned are also deduped, keeping the first.
- Response: `model_list` on success, `error` if `providerName` isn't registered or the upstream call fails (e.g. a revoked key). Each returned model carries `isFavorite`, set from that provider's server-persisted favorites (see `set_favorite_models` below), and an optional `capabilities` object — see `model_list` (§3.2) for its shape.

#### `set_favorite_models`
Replaces the favorite-model list for a provider. Server-persisted (alongside the provider's credential in `providers.json`), not local frontend storage — so favorites follow the user to any client/device, not just the one that starred them.
```json
{"type": "set_favorite_models", "data": {"providerName": "my-anthropic", "models": ["claude-sonnet-5", "claude-opus-5"]}}
```
- `models` is the full replacement list (not a single toggle) — send the whole set including any previously-favorited IDs you want kept.
- Model IDs aren't validated against `list_models`; a favorite for a model that's temporarily missing from the live list (upstream hiccup, deprecated ID) is kept rather than silently dropped.
- Response: `favorite_models_set` on success, `error` if `providerName` isn't registered.

#### `set_provider`
Hot-swaps the provider **and model** a session uses, effective on that session's *next* turn (a turn already in flight keeps using whichever provider/model it started with).
```json
{
  "type": "set_provider",
  "sessionId": "session-main",
  "data": {"providerName": "my-anthropic", "model": "claude-sonnet-5"}
}
```
- `model` should be one of the `id`s from that provider's `list_models` response. Omit it only for `cli_claude`/`cli_codex`, which fall back to their tool's own default model.
- **`projectPath`** *(optional)*: resolves this session against the project its turns will actually run in, binding the session if it wasn't bound yet — the same rules as `user_prompt` above (§4b). Send it when configuring a session that hasn't been prompted yet: a client normally picks a model for a brand-new chat *before* its first message, and since each project keeps its own session registry (§4b), a provider set against the server's default-project fallback lands on a registry that session's turn will never use. The turn then runs unconfigured and comes back `finishReason: "no_provider"`, contradicting a provider the client could see selected.
- Response: `provider_set` on success (echoing back the `model` that was set), `error` if `providerName` isn't registered or fails to build (e.g. a `cli_claude` provider on a machine without `claude` on `PATH`).

#### `set_model_options`
Tunes the *current* provider's request shape for a session's next turn — reasoning effort, an extended-thinking token budget, and/or the session's own context-window budget — without swapping the provider/model itself (that's `set_provider`, above).
```json
{
  "type": "set_model_options",
  "sessionId": "session-main",
  "data": {"reasoningEffort": "high", "thinkingBudget": 4096, "contextWindow": 100000}
}
```
- All three fields are optional; a zero/empty field is left unchanged, it doesn't reset anything.
- `reasoningEffort` is sent through as OpenAI/OpenRouter's `reasoning_effort` request parameter; ignored by Anthropic providers.
- `thinkingBudget` (tokens) is sent through as Anthropic's extended-thinking `thinking: {type: "enabled", budget_tokens}` block; ignored by OpenAI/OpenRouter providers.
- `contextWindow` resizes the session's own context-fullness budget (`ContextManager.MaxHistoryTokens`, reported back via `turn_completed.context`, §3.2) — this is local bookkeeping, not a provider request field. When a turn's history grows past this budget, the server compacts it (first user message kept verbatim, an in-between stretch summarized, the most recent messages kept intact) before the next round-trip — and that most-recent window is always widened backward far enough that it never starts on a lone tool result whose originating tool-call message got summarized away, since both Anthropic and OpenAI reject a request shaped like that outright (a 400 error, not a graceful degradation) rather than just ignoring the orphaned entry. A long, tool-heavy turn is the case this matters for: it's the one shape naive fixed-size compaction would otherwise slice through.
- Both `reasoningEffort` and `thinkingBudget` are silently ignored by `cli_claude`/`cli_codex` providers, which have no request shape to tune (the CLI subprocess owns that). `sessionId` may be given in `data.sessionId` instead of at the envelope level, same fallback rules as `set_plan_mode` below.
- **`projectPath`** *(optional)*: resolves the session the same way `set_provider` above does, and matters for the same reason — model options can be tuned on a brand-new chat before its first prompt has bound the session to a project.
- Response: `model_options_set`, always — it doesn't report whether the active provider actually understood `reasoningEffort`/`thinkingBudget`; check `capabilities` on the model you set (from `model_list`) before assuming either one takes effect.

#### `set_plan_mode`
Toggles a session between normal execution and **plan mode** (§4c), effective on that session's *next* turn (a turn already in flight keeps whichever tool set it started with).
```json
{
  "type": "set_plan_mode",
  "sessionId": "session-main",
  "data": {"enabled": true}
}
```
- `sessionId` may be given in `data.sessionId` instead of at the envelope level (`data.sessionId` wins if both are set); if neither is given, it falls back to `"default"`.
- **`projectPath`** *(optional)*: resolves the session the same way `set_provider` above does — plan mode can be toggled on a brand-new chat before its first prompt has bound the session to a project, and applying it to the wrong project's registry would leave the session's actual turn unrestricted.
- Response: `plan_mode_set`.

#### `openrouter_oauth_start`
Begins an OpenRouter PKCE login (§6b). `name` is the provider name to register once it completes.
```json
{
  "type": "openrouter_oauth_start",
  "data": {"name": "my-openrouter", "callbackUrl": "https://your-app/callback"}
}
```
- Response: `openrouter_oauth_url`, carrying the URL to open in a browser and a `requestId` to echo back.

#### `openrouter_oauth_complete`
Finishes a login after the user approves it and your callback captures the `code` query parameter.
```json
{
  "type": "openrouter_oauth_complete",
  "data": {"requestId": "<from openrouter_oauth_url>", "code": "<from the callback redirect>"}
}
```
- Response: `provider_registered` on success, `error` if the request ID is unknown/already used or the code exchange fails.

#### `remove_provider`
Deletes a registered provider config.
```json
{"type": "remove_provider", "data": {"name": "my-anthropic"}}
```
- Response: `provider_removed` (this succeeds even if the name didn't exist — removal is idempotent).

#### `list_sessions`
Enumerates persisted sessions for **one project** (i.e. everything under that project's `.yoake/sessions/`, regardless of whether it currently has a live connection).
```json
{"type": "list_sessions", "data": {"projectPath": "/home/user/projects/my-repo"}}
```
- `projectPath`: required unless the server has a default project (`-project`), in which case omitting it lists that default project's sessions.
- Response: `session_list`.

#### `get_session_history`
Fetches the full persisted message history for **one session**, resolving its project the same way `user_prompt` does (§4b): `data.projectPath` if given, else whatever project the session is already bound to, else the server's default project.
```json
{"type": "get_session_history", "data": {"sessionId": "session-main"}}
```
- Chat history is server state, not client state (see §6): it's persisted per-project at `.yoake/sessions/<sessionId>.json` and survives process restarts, but the wire protocol otherwise never pushes a session's *past* turns to a client — only new events from the point a connection starts watching that session (§2a). A client that opens an existing session cold — a second device, a fresh install, a reconnect after being offline — has nothing to render until this is called; `list_sessions` alone only gives metadata (`updatedAt`/`messageCount`), not content.
- Response: `session_history`. A `sessionId` with no persisted history yet is **not** an error — it returns an empty `messages` list, so a client can call this unconditionally on open without first checking whether the session exists.
- This is the same mechanism for local and remote connections: don't special-case "local" as "the client that wrote the messages is probably still open" — the server is the single source of truth either way, and a local client that keeps its own cache should still treat it as a cache, reconciled against this response, not as authoritative.
- `session_history` carries only `sessionId`/`messages` — no `title`/`archived`/`linkedSessionId`, consistent with title and archived already being `session_list`/`session_updated`-only. A client opening a session it doesn't already have metadata for (cold start, second device) should call `list_sessions` for the summary and `get_session_history` for the content, rather than expecting either call to hand back both.

#### `set_session_title`
Renames a persisted session. This still wins over anything the backend generates on its own (see `autoGenerateTitle` below): it only ever fills in a title that's empty, so a client that wants full control can just always call this instead of relying on generation.
```json
{"type": "set_session_title", "data": {"sessionId": "session-main", "title": "Streaming file reader refactor"}}
```
- `sessionId`: required, and must already have persisted history (`.yoake/sessions/<sessionId>.json` must exist) — `error` if it doesn't. Resolves its project the same way `get_session_history` does.
- Response: `session_updated`.

##### Auto-generated titles
A session's first `user_prompt` (above) may carry `autoGenerateTitle` (default `true`). When the session has no title yet, the backend races two things in parallel with that first turn rather than blocking it: a lightweight, non-streaming request to the same model/provider already handling the turn, asking it to summarize the prompt into a short title, and — if that call is disabled, errors, or the provider doesn't support it — a plain client-independent fallback (the prompt's first ~8 words, truncated to the same character limit as the generated case). Whichever resolves is what gets saved; there's no separate "it failed" signal on the wire, since the fallback always produces something.
- This only ever sets a **currently-empty** title — same precedent as `set_session_title`s "single source of truth" framing: a session that already has one (an earlier `set_session_title`, or generation from a previous turn) is left untouched.
- The result — generated or fallback — is pushed as a `session_updated` event (same shape as `set_session_title`'s response), not just written to disk silently. A client doesn't poll `list_sessions` to discover its own new title; it arrives on the same connection shortly after the turn starts, independent of `turn_completed`.
- Set `"autoGenerateTitle": false` on that first `user_prompt` to skip the model call entirely and go straight to the fallback (still delivered via `session_updated`) — e.g. for a client that always calls `set_session_title` itself and doesn't want the extra request.

#### `set_session_archived`
Sets or clears a session's archived flag, for a client-side "hide from the active list" without deleting anything.
```json
{"type": "set_session_archived", "data": {"sessionId": "session-main", "archived": true}}
```
- `sessionId`: required, same existence/resolution rules as `set_session_title`.
- Response: `session_updated`.

#### `set_session_layout`
Pairs (or unpairs) two sessions for a client-side split/sidebar layout — e.g. a main thread with a linked scratch thread shown alongside it. This is a pure layout hint: the backend only stores and echoes it back, never reads or acts on it itself.
```json
{"type": "set_session_layout", "data": {"sessionId": "session-main", "linkedSessionId": "session-scratch"}}
```
- `sessionId`: required, same existence/resolution rules as `set_session_title`.
- `linkedSessionId`: the paired session's id, or `null` to unpair. Not validated against an existing session, and never cleaned up if the partner is later archived or deleted (once `delete_session` exists) — same precedent as `set_favorite_models`'s stale model IDs (§3.1): kept, not silently dropped, on the assumption a client that can create the dangling reference can also handle resolving it to nothing. A client deref'ing `linkedSessionId` should already treat a missing/gone partner as "no linked session" rather than an error.
- Response: `session_updated`, same shape as `set_session_title`/`set_session_archived` — no separate broadcast to other connected peers. A second device picks up a layout change the same way it picks up a title or archive change: on its next `list_sessions` (or by re-resolving the session it already has open). If a peer is actively viewing the paired session live, treat that as a `list_sessions` refresh case rather than expecting a push.
- Only `sessionId`'s own record is touched — pairing is one-directional bookkeeping, not a two-way link maintained on both sessions. A client that wants the reverse pairing to show up on the other session's summary should send a second `set_session_layout` for that id.
- Deliberately **not** persisted here: sidebar collapse state, split ratio, or other pure screen-space prefs. Those vary per device/window size and belong in client-local storage — only the pairing itself (which two sessions go together) is shared session data.

#### `delete_session`
Permanently deletes a persisted session — archived or not. Unlike `set_session_archived`, this is not reversible: it removes `.yoake/sessions/<sessionId>.json` outright.
```json
{"type": "delete_session", "data": {"sessionId": "session-main"}}
```
- `sessionId`: required, same existence/resolution rules as `set_session_title` — `error` if there's no persisted session by that id. Idempotency matches `set_session_title`/`set_session_archived` rather than `remove_provider`: deleting an id that's already gone is an `error`, not a silent success, since (unlike a provider credential) there's no natural "absent" state for a session id to already be in — a second `delete_session` almost always means a client's local state is stale, which is worth surfacing rather than swallowing.
- If the session has a turn in flight, that turn is cancelled first (as if `cancel_turn` had been sent) and its in-memory `Agent` is dropped, then the persisted file is removed — deletion always wins over "let it finish."
- Response: `session_deleted`, echoing `sessionId` back.

#### `list_directory`
Lists one directory's immediate entries on **the machine running the backend** — not the frontend's machine, which may differ (remote/TCP connections, §2a). This is what a project-picker "choose a folder" flow should call instead of assuming a native OS file dialog reflects the right filesystem; a native dialog only works when the backend and frontend share a filesystem (e.g. local unix-socket use), and is simply wrong for a TCP connection to a remote server.
```json
{"type": "list_directory", "data": {"path": "/home/user/projects"}}
```
- `path`: optional; omitted (or `""`) lists the server process's own home directory, a reasonable starting point for a picker.
- Response: `directory_list`.

#### `open_project`
Opens (if not already open) and validates a project path, without starting any turn — e.g. to confirm a folder is usable, or to `list_sessions` against it, before creating a new session in it.
```json
{"type": "open_project", "data": {"path": "/home/user/projects/my-repo"}}
```
- Response: `project_opened` on success, `error` if the path doesn't exist/isn't a directory/isn't accessible.

#### `list_projects`
Enumerates every project opened on this server process since it started (not a filesystem scan — see §4b). Takes no data (`{}` is fine).
- Response: `project_list`.

#### `list_files`
Enumerates every regular file under **one project**'s root, for client-side `@file` mention autocomplete.
```json
{"type": "list_files", "data": {"projectPath": "/home/user/projects/my-repo"}}
```
- `projectPath`: required unless the server has a default project (`-project`), in which case omitting it lists that default project's files.
- Paths are relative to the project root, forward-slash separated, sorted. VCS/dependency/build directories (`.git`, `node_modules`, `target`, `dist`, `build`, `vendor`, `__pycache__`) and anything dot-prefixed (including `.yoake`) are skipped, walking the live filesystem — not `.gitignore`-aware beyond that fixed list.
- Capped at 20,000 files (silently truncated past that) to stay well under the 10MB line limit from §2 on a huge monorepo.
- Response: `file_list`.

#### `detect_cli`
Checks whether the `claude`/`codex` binaries are on `PATH` **on the machine running the backend** — relevant before offering `cli_claude`/`cli_codex` as a provider kind (§6b). Takes no data (`{}` is fine).
- Response: `cli_availability`.

#### `ping`
Either side may send this at any time, on any connection state, to check liveness / measure round-trip time. See "Keeping a connection alive" in §2a.
```json
{"type": "ping", "data": {"nonce": "anything-you-want-echoed-back"}}
```
- Response: `pong`, echoing the same `nonce`.

#### `set_peer_name`
Renames *this* connection's own peer entry — there's no target `peerId`, a peer can only rename itself.
```json
{"type": "set_peer_name", "data": {"name": "aveline's phone"}}
```
- Response: no direct ack — every peer (including the renamer) sees the change on the next `peer_list` push. `error` if this connection has no registered peer (only possible on a transport that skips peer registration, currently none of unix/TCP).
- `error` also for a connection authenticated with a token (`register_token`): that identity's name is the credential's name, fixed at registration — rename by revoking (`remove_token`) and registering a new name instead, so "who is this" never has two conflicting answers (what the token says vs. what was last typed) mid-session.

#### `set_peer_avatar`
Sets *this* connection's own avatar — same self-only shape as `set_peer_name`, for updating the picture chosen at connect time (`auth`'s `avatar`, §2a) without reconnecting.
```json
{"type": "set_peer_avatar", "data": {"avatar": "data:image/png;base64,..."}}
```
- Same size/scaling conventions as `auth`'s `avatar` field (§2a): client-side downscale to 96x96, server does no image processing, oversized values are rejected here (unlike at connect time, where they're silently dropped) — you get a clear `error` to retry with something smaller, rather than a picture that silently never took.
- For a connection authenticated with a token (`register_token`/the legacy `"default"` one), this also persists the avatar against that token (`server.json`), so it's still there on the *next* reconnect rather than resetting to blank — the one piece of `set_peer_avatar` that outlives the connection. A unix-socket peer's avatar is still purely live/in-memory, same as before.
- Response: no direct ack — every peer sees the change on the next `peer_list` push. `error` if this connection has no registered peer, or if `avatar` exceeds the size cap.

#### `disconnect_peer`
Force-closes another live connection. See "Peer registry & remote-access settings" (§2a) for the permission model.
```json
{"type": "disconnect_peer", "data": {"peerId": "a1b2c3d4"}}
```
- Response: no direct ack on success — the disconnected peer drops off the next `peer_list` push. `error` if `peerId` isn't a live peer.

#### `set_remote_access`
Opens or closes the optional TCP listener live, without restarting the process. **Local-socket-only** — `error` if sent over TCP.
```json
{"type": "set_remote_access", "data": {"enabled": true}}
```
- Enabling with no address ever configured (the process was started without `-listen`) binds a default port (`:7890`) rather than requiring one. Disabling closes the listener and drops every currently-connected TCP peer (not just new ones) — treat this as the "cut off remote access now" action it is, not a soft toggle.
- Response: `remote_access_set` on success, `error` on failure (e.g. the address is already in use) or if sent over TCP.

#### `set_remote_password`
Rotates the TCP listener's shared auth token, persisted under `-state-dir` so it survives a restart. **Local-socket-only** — `error` if sent over TCP.
```json
{"type": "set_remote_password", "data": {"password": "a-new-shared-secret"}}
```
- Already-connected TCP clients keep their session uninterrupted; only the *next* `auth` handshake needs the new password.
- Response: `remote_password_set` on success, `error` on failure or if sent over TCP.

#### `register_token`
Mints a new **named**, per-person TCP auth credential — an alternative to sharing the one `-token`/`set_remote_password` secret among everyone who connects remotely. **Local-socket-only** — `error` if sent over TCP, same rationale as `set_remote_password`: minting a new credential is an admin action.
```json
{"type": "register_token", "data": {"name": "aveline", "email": "aveline@example.com"}}
```
- `name`: required, must not already be registered (including the implicit `"default"` token that `-token`/`set_remote_password` manage) — `error` on a duplicate. Fixed for the life of the credential: there's no rename, by design — revoke it (`remove_token`) and register a new name instead, so "which person is this" never has two possible answers mid-flight.
- `email`: optional, used only to seed `GIT_AUTHOR_EMAIL`/`GIT_COMMITTER_EMAIL` on a `bash` call this identity's turn makes (see `tool_call_requested`'s git-attribution note below) — a `<name>@yoake.local` placeholder is used if omitted.
- Once this token authenticates a TCP `auth` handshake, its `name` becomes that connection's peer identity — overriding whatever `name`/`avatar` the client self-reports in the `auth` frame, since a named token is a real credential and a self-reported string isn't. Its avatar starts empty and is cached going forward via `set_peer_avatar` (§3.1) — see there for the persistence detail. A peer authenticated this way can't `set_peer_name` itself (that call errors for a token-backed identity); it can still `set_peer_avatar`.
- Response: `token_registered`, carrying the newly generated token — **shown only this once**, the same "reveal at creation, scrub afterward" pattern as any other bearer credential. Copy it somewhere before disconnecting; there's no way to retrieve it again short of `remove_token` + `register_token` a replacement.

#### `list_tokens`
Enumerates every registered named token (plus the implicit `"default"` one), without the raw token value — same scrubbing precedent as `provider_list` omitting `apiKey`. **Local-socket-only.** Takes no data (`{}` is fine).
- Response: `token_list`.

#### `remove_token`
Revokes one named credential — idempotent, like `remove_provider`: removing a name that's already gone still succeeds, since (like a provider credential, unlike a session id) there's no meaningful distinction between "never existed" and "already revoked." **Local-socket-only.**
```json
{"type": "remove_token", "data": {"name": "aveline"}}
```
- Anyone still connected under that token at the moment it's removed keeps their current connection (same "no forced disconnect" behavior as `set_remote_password`'s rotation) — only the *next* `auth` handshake with that token is rejected.
- Response: `token_removed`, echoing `name` back.

#### `set_server_identity`
Sets this server's display name, shown to peers (and broadcast to all of them immediately). Persisted under `-state-dir`; defaults to the machine's hostname if never set.
```json
{"type": "set_server_identity", "data": {"name": "aveline's living room"}}
```
- Response: no direct ack — every peer (including the setter) sees the change via `server_identity`. `error` on failure to persist.

#### `set_auto_generate_title`
Sets the server-wide default applied to a first `user_prompt` (§3.1) that omits its own `autoGenerateTitle` — an operator-level on/off switch for chat title generation, broadcast to all connected peers immediately. Persisted under `-state-dir`; defaults to `true` if never set. A client that always passes its own `autoGenerateTitle` on `user_prompt` is unaffected either way — that per-request field still wins.
```json
{"type": "set_auto_generate_title", "data": {"enabled": false}}
```
- Response: no direct ack — every peer (including the setter) sees the change via `auto_generate_title`. `error` on failure to persist.

---

### 3.2 Outbound Events (Backend → Frontend)

#### `model_delta`
A real-time token or text chunk streamed by the model. Exactly one of `data.content`/`data.reasoning` is set per event.
```json
{
  "type": "model_delta",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "data": {
    "content": "I will inspect the "
  }
}
```
`data.reasoning` carries the model's own reasoning/chain-of-thought text instead of visible answer text — Anthropic extended thinking (`thinkingBudget` in `set_model_options`, §3.1) or an OpenAI-compatible provider's `reasoning_content` (DeepSeek-reasoner and similar; no opt-in needed, it streams whenever the upstream API sends it):
```json
{
  "type": "model_delta",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "data": {
    "reasoning": "The user wants me to check "
  }
}
```
*UI Action*: Append `data.content` directly to the active assistant message widget. Append `data.reasoning` to a separate collapsible section instead — it's not part of the answer. A client that doesn't care about reasoning can just ignore events where `data.content` is absent. Not emitted at all by `cli_claude`/`cli_codex` providers. A completed reasoning section is also included in a reloaded transcript, as `reasoning` on the relevant entry of `get_session_history`'s `messages` (§3.2 below).

#### `tool_call_requested`
Notifies the frontend that the model has formally requested a tool call.
```json
{
  "type": "tool_call_requested",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "toolCallId": "call_abc123",
  "data": {
    "name": "file_write",
    "args": {
      "path": "lib/main.dart",
      "content": "void main() => runApp(App());"
    }
  }
}
```
*UI Action*: Render an in-progress tool execution card/indicator.

**Git attribution, `bash` calls only:** when this turn's `user_prompt` came from a TCP connection authenticated with a token (`register_token` or the legacy `"default"` one — §3.1), any `bash` call in that turn gets `GIT_AUTHOR_NAME`/`GIT_AUTHOR_EMAIL`/`GIT_COMMITTER_NAME`/`GIT_COMMITTER_EMAIL` set in its environment from that identity, so a `git commit` the agent runs is attributed to the human who asked for it rather than an anonymous or ambient identity. Set unconditionally on every `bash` call in that turn, not just ones that look like `git commit` — harmless for anything else. Nothing is injected for a unix-socket-originated turn (no credential to attribute to); the sandbox's ambient git config, if any, applies unchanged there, exactly as before this existed.

#### `permission_request`
The backend pauses execution and requires user approval before executing a tool.
```json
{
  "type": "permission_request",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "toolCallId": "call_abc123",
  "data": {
    "toolName": "file_write",
    "description": "file_write on lib/main.dart",
    "requiresAsk": true,
    "cacheKey": "file_write:lib/main.dart",
    "diff": {
      "filePath": "lib/main.dart",
      "unified": "--- a/lib/main.dart\n+++ b/lib/main.dart\n@@ -1,3 +1,1 @@\n-void old() {}\n+void main() => runApp(App());\n",
      "hunks": [
        {
          "oldStart": 1,
          "oldLines": 3,
          "newStart": 1,
          "newLines": 1,
          "lines": [
            { "type": "-", "content": "void old() {}" },
            { "type": "+", "content": "void main() => runApp(App());" }
          ]
        }
      ]
    }
  }
}
```
*UI Action*: Display an interactive modal or inline prompt. If `data.diff` is non-null, render a side-by-side or unified diff inspector.

#### `tool_result`
The result of executing a tool (after approval or if pre-approved by policy).
```json
{
  "type": "tool_result",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "toolCallId": "call_abc123",
  "data": {
    "toolName": "file_write",
    "output": "Successfully wrote 32 bytes to lib/main.dart",
    "error": ""
  }
}
```
*Note*: If the user denied permission, `data.error` contains `"permission denied by user or policy"` — or, if the `permission_response` carried non-empty `feedback` (§3.1), `"permission denied by user: <feedback>"` instead, so the model sees the reason in the same turn.

#### `turn_completed`
Signifies that the iterative turn loop has ended.
```json
{
  "type": "turn_completed",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "data": {
    "finishReason": "stop",
    "sessionId": "session-main",
    "usage": {
      "inputTokens": 1420,
      "outputTokens": 380,
      "totalTokens": 1800,
      "costUsd": 0.0246
    },
    "context": {
      "estimatedTokens": 8342,
      "maxTokens": 64000
    },
    "spend": {
      "sessionTotalUsd": 0.87,
      "projectTotalUsd": 12.4
    },
    "durationMs": 2340
  }
}
```
- `finishReason`: `"stop"` (normal completion), `"length"` (the model's own final round-trip was cut off at its output-token limit before it finished on its own — Anthropic's `stop_reason: "max_tokens"` and OpenAI/OpenRouter's `finish_reason: "length"` are both normalized to this one value here, so a client only ever has to branch on one string regardless of provider), `"cancelled"` (user aborted), `"budget_exceeded"` (the session was already at/over its `-max-session-cost-usd` cap), or `"no_provider"` (this session has neither a default provider from server startup nor its own `set_provider` call — register one and retry, see §6b). In the latter two cases the model was never called for this turn; `usage`/`context` reflect the session's existing state, not this rejected turn.
- **`"length"` caveat**: this is only ever reported for a turn's *final* round-trip (the one with no tool calls) — a round-trip that got cut off mid-tool-call instead (rare: it needs a tool call's arguments themselves to straddle the token limit) still executes whatever (possibly malformed) JSON the model had emitted so far, same as before this value existed; the tool's own error on unparseable arguments is the fallback signal in that case. A client that wants to recover a `"length"`-truncated answer that still has *some* content should resend the conversation with a higher `thinkingBudget`/larger model output allowance rather than assume the harness retries automatically — for that case, it doesn't. The one exception: a round-trip that hit the token limit with *zero* content and *zero* tool calls (a reasoning model spending its whole budget on hidden reasoning before writing anything visible) is retried internally, up to twice, before `"length"` is ever reported — a fresh independently-sampled generation each time, not a resend of the same request. Only once all three attempts come back empty does the client see `"length"` with nothing to show for it.
- `usage`: Token consumption for this turn. For API-key providers, `costUsd` is priced against a live-fetched per-model rate table (LiteLLM's `model_prices_and_context_window.json`, refreshed daily) first, falling back to a small built-in table when a model is in neither. A CLI-subprocess provider (§6b) reports its own authoritative cost instead when it has one — `claude` always does; `codex` doesn't, so its turns fall through to the same rate-table pricing as an API-key provider (not a flat `0` as such) and only actually come out `0` if the model it used isn't in either table.
- `context`: How full the session's context window estimate is right now (`estimatedTokens` / `maxTokens`). `estimatedTokens` prefers the provider's own reported token count for this turn (exact, and the number that reflects what's actually resent next turn) over yoakeland's own char-count heuristic, used only when the provider hasn't reported usage at all. *UI Action*: render as a context meter/progress bar.
- `spend`: Running totals persisted in `.yoake/usage.json`, surviving process restarts — *UI Action*: render as a running cost display, per-session and/or project-wide.
- `durationMs`: Wall-clock execution time for the entire turn.

#### `snapshot_capturing`
Sent right before a `file_write`/`file_edit` tool call takes its automatic pre-mutation snapshot — normally an invisible, near-instant step, but this brackets it so a frontend can show a "snapshotting…" indicator on a large file instead of a silent gap between `tool_call_requested` and the eventual `tool_result`.
```json
{
  "type": "snapshot_capturing",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "toolCallId": "call_abc123",
  "data": {
    "turnId": "turn-1",
    "description": "file_write: lib/main.dart"
  }
}
```
*UI Action*: Optional — a client that doesn't care about this level of detail can ignore it entirely and just wait for `tool_result` as before.

#### `snapshot_created`
Confirms a turn's snapshot was created or extended to additionally cover a file — see §6's per-turn snapshot model. Sent once per `file_write`/`file_edit` call that actually captures a *new* file's pre-mutation state; a second mutation of a file already tracked earlier in the same turn captures nothing new and emits no event (its pre-turn state is already recorded and must not be overwritten by an intermediate one).
```json
{
  "type": "snapshot_created",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "toolCallId": "call_abc123",
  "data": {
    "snapshotId": "snap_1710950400000000",
    "description": "file_write: lib/main.dart; file_edit: lib/reader.dart",
    "files": ["lib/main.dart", "lib/reader.dart"]
  }
}
```
- `files` is the snapshot's **full current file list** — every path captured for this turn so far, not just the one this particular event was triggered by.
- `description` accumulates across every mutation folded into this turn's snapshot (semicolon-separated), so a snapshot picker shows everything a turn touched, not just its first mutation.
*UI Action*: Optional visibility into an otherwise-invisible backend step — e.g. a subtle "snapshot saved" toast, or logged for a debug/activity view. Not required to populate the restore-snapshot picker; that's still `list_snapshots`/`snapshot_list`.

If a snapshot capture itself fails (e.g. a full disk), the backend sends a plain `error` envelope (`data.message` starting with `"snapshot capture failed: "`) scoped to the same `sessionId`/`turnId`/`toolCallId` **before** the tool call's own `tool_result.data.error` — this is additive visibility, not a replacement for the normal tool-error path.

#### `snapshot_restored`
Confirms successful restoration of a filesystem snapshot.
```json
{
  "type": "snapshot_restored",
  "sessionId": "session-main",
  "data": {
    "snapshotId": "snap_1710950400000000"
  }
}
```
*UI Action*: Refresh any open editor buffers or tree views; notify user of restoration.

#### `snapshot_list`
Response to `list_snapshots`.
```json
{
  "type": "snapshot_list",
  "data": {
    "snapshots": [
      {"snapshotId": "snap_1710950400000000", "description": "file_write on lib/main.dart", "createdAt": "2026-09-11T10:03:22Z"}
    ]
  }
}
```
*UI Action*: Populate a restore-snapshot picker for the project you asked about — most-recent-first is a reasonable default sort (the backend doesn't guarantee an order). Pass the chosen `snapshotId` to `restore_snapshot` (§3.1).

#### `error`
Reports operational or fatal backend errors.
```json
{
  "type": "error",
  "sessionId": "session-main",
  "turnId": "turn-1",
  "data": {
    "message": "anthropic error (401): invalid api key",
    "fatal": false
  }
}
```
`fatal: true` currently only occurs on a failed `auth` handshake (§2a), immediately followed by the server closing the connection.

#### `session_taken_over` *(daemon mode only — not `-stdio`)*
Sent to a connection that just lost ownership of a session to a different connection (§2a).
```json
{
  "type": "session_taken_over",
  "sessionId": "session-main",
  "data": {"sessionId": "session-main"}
}
```
*UI Action*: Mark that session as "opened elsewhere" and stop expecting further events for it on this connection.

#### `provider_registered`
Confirms `register_provider` or a completed `openrouter_oauth_complete`.
```json
{"type": "provider_registered", "data": {"name": "my-anthropic"}}
```

#### `provider_list`
Response to `list_providers`. Credentials are never included, and note there's no `model` here any more — a provider is a credential, not a model choice; call `list_models` per provider to get its models.
```json
{
  "type": "provider_list",
  "data": {
    "providers": [
      {"name": "my-anthropic", "kind": "api_key", "httpProvider": "anthropic", "authMethod": "manual", "favoriteModels": ["claude-sonnet-5"]},
      {"name": "claude-subscription", "kind": "cli_claude", "authMethod": "manual"}
    ]
  }
}
```
*UI Action*: For each `api_key` provider row, lazily fetch `list_models` (e.g. when its picker is opened, and cache the result) rather than eagerly resolving every provider's models up front.

#### `model_list`
Response to `list_models`.
```json
{
  "type": "model_list",
  "data": {
    "providerName": "my-anthropic",
    "models": [
      {"id": "claude-sonnet-5", "displayName": "Claude Sonnet 5", "isFavorite": true},
      {
        "id": "anthropic/claude-opus-5",
        "displayName": "Anthropic: Claude Opus 5",
        "capabilities": {"context_length": 200000, "pricing": {"prompt": "0.000005", "completion": "0.000015"}, "reasoning": {"supported_efforts": ["low", "medium", "high"]}}
      },
      {"id": "o4-mini", "displayName": "o4-mini", "capabilities": {"reasoningEfforts": ["low", "medium", "high"]}},
      {"id": "claude-haiku-4-5", "displayName": "Claude Haiku 4.5", "isLegacy": true}
    ]
  }
}
```
- `capabilities`, when present, mirrors that upstream's *own* shape rather than one unified schema — OpenRouter's own `context_length`/`pricing`/`top_provider`/`reasoning` fields are passed through close to verbatim; OpenAI has no such endpoint field, so reasoning-capable models (`o1`/`o3`/`o4-mini`/`gpt-5*`) instead carry a hand-maintained `reasoningEfforts` list; `cli_claude` models carry `reasoningEfforts`/`fastMode`/`contextWindows` cross-walked from T3 Code's manifest (§6b) where that manifest's profile for the model has them; `cli_codex` and Anthropic models carry no `capabilities` today. Don't assume a common set of keys across providers — branch on which provider you're looking at, not on which keys happen to be present.
- `isLegacy` (omitted, i.e. `false`, when absent) is only ever set for `cli_claude` models whose T3 manifest entry has `status: "legacy"` — still fully usable, just no longer T3's current pick for that profile. Every other provider's models are never marked legacy.
*UI Action*: Populate a per-provider model picker from this — never hardcode a fixed model set, since it's meant to reflect whatever the upstream account currently has access to. Use `isFavorite` to sort/star favorites in the picker; let the user toggle it and send the updated set via `set_favorite_models`. Use `capabilities` to decide whether to offer a reasoning-effort/thinking-budget control for a model before calling `set_model_options` (§3.1) — sending it to a model that doesn't support it is silently ignored, not an error, so the UI is the only thing preventing a confusing no-op. Use `isLegacy` to sort/group legacy `cli_claude` models below current ones (or behind a "show legacy" toggle) rather than hiding them outright — a session already pinned to one must still be able to re-select it.

#### `favorite_models_set`
Confirms `set_favorite_models`.
```json
{"type": "favorite_models_set", "data": {"providerName": "my-anthropic", "models": ["claude-sonnet-5", "claude-opus-5"]}}
```

#### `provider_set`
Confirms `set_provider` took effect for a session, echoing back the model that's now active.
```json
{"type": "provider_set", "data": {"sessionId": "session-main", "providerName": "my-anthropic", "model": "claude-sonnet-5"}}
```

#### `model_options_set`
Confirms `set_model_options` took effect for a session, echoing back whatever was set (fields left unchanged by the request are echoed as empty/zero, not as the value now in effect).
```json
{"type": "model_options_set", "data": {"sessionId": "session-main", "reasoningEffort": "high", "thinkingBudget": 4096, "contextWindow": 100000}}
```

#### `plan_mode_set`
Confirms `set_plan_mode` took effect for a session.
```json
{"type": "plan_mode_set", "data": {"sessionId": "session-main", "enabled": true}}
```
*UI Action*: Reflect the mode in the composer (e.g. disable/relabel the send button, show a "Plan" badge) — the backend doesn't hide the write tools from the *UI*, only from the model, so the frontend still decides how to signal it.

#### `openrouter_oauth_url`
Response to `openrouter_oauth_start`.
```json
{"type": "openrouter_oauth_url", "data": {"requestId": "3f9a...", "url": "https://openrouter.ai/auth?callback_url=...&code_challenge=...&code_challenge_method=S256"}}
```
*UI Action*: Open `url` in the system browser (or an in-app webview) and listen on `callbackUrl` for the redirect carrying `?code=...`; pass that code plus `requestId` to `openrouter_oauth_complete`.

#### `provider_removed`
Confirms `remove_provider`.
```json
{"type": "provider_removed", "data": {"name": "my-anthropic"}}
```

#### `session_list`
Response to `list_sessions`.
```json
{
  "type": "session_list",
  "data": {
    "sessions": [
      {"sessionId": "session-main", "updatedAt": "2026-09-11T10:03:22Z", "messageCount": 14, "title": "Streaming file reader refactor", "archived": false, "linkedSessionId": "session-scratch"}
    ]
  }
}
```
- `title` is omitted (empty string) until it's set — either explicitly via `set_session_title`, or by the backend's own auto-generation/fallback on the session's first turn (see `set_session_title`'s "Auto-generated titles", §3.1); `archived` is omitted (`false`) until a client sets it via `set_session_archived`; `linkedSessionId` is omitted (`null`) until a client pairs it via `set_session_layout`. Only `title` is ever generated by the backend itself, and only that once, into an otherwise-empty field.
*UI Action*: Populate a session switcher/list for the one project you asked about — filter out `archived: true` sessions from the default view, and use `title` in place of a bare `sessionId` wherever the UI would otherwise show one. This does **not** tell you whether a session is currently bound to another live connection — there's no wire event for that yet (§2a's `session_taken_over` only fires *after* a takeover happens, to the connection that lost it). It also carries no message content — call `get_session_history` for the session the user actually opens.

#### `session_updated`
Response to `set_session_title`, `set_session_archived`, or `set_session_layout` — the session's updated summary, same shape as one entry of `session_list`. Also sent unprompted, on the same connection, once a session's first turn resolves an auto-generated (or fallback) title — see `set_session_title`'s "Auto-generated titles" (§3.1); that's the only case where this event isn't a direct response to one of the three `set_session_*` calls.
```json
{"type": "session_updated", "data": {"sessionId": "session-main", "updatedAt": "2026-09-11T10:04:01Z", "messageCount": 14, "title": "Streaming file reader refactor", "archived": false}}
```
*UI Action*: Patch the corresponding entry in whatever session list/switcher state the client holds — no need to re-issue `list_sessions`.

#### `session_deleted`
Response to `delete_session`.
```json
{"type": "session_deleted", "data": {"sessionId": "session-main"}}
```
*UI Action*: Remove the corresponding entry from whatever session list/switcher state the client holds, and if it was the currently-open session, navigate away from it — there's nothing left to `get_session_history` for that id.

#### `session_history`
Response to `get_session_history`.
```json
{
  "type": "session_history",
  "data": {
    "sessionId": "session-main",
    "messages": [
      {"role": "user", "content": "Refactor the file reader to support streaming", "author": "aveline", "authorAvatar": "🐣"},
      {"role": "assistant", "content": "I'll start by reading the current implementation.", "toolCalls": [{"id": "call_abc123", "name": "file_read", "args": {"path": "lib/reader.dart"}}]},
      {"role": "tool", "toolCallId": "call_abc123", "content": "class FileReader { ... }"}
    ]
  }
}
```
*UI Action*: Repaint the session's transcript from `messages` before showing the composer — this is the one point where a client reconstructs prior turns rather than receiving them live. `messages` is the same shape yoakeland itself stores (role/content/toolCalls/toolCallId/author/authorAvatar per turn), not re-derived from `model_delta`/`tool_call_requested`/etc., so a tool call and its result appear as a single history entry each rather than the granular event sequence a *live* turn streams.
- `author`: who actually sent this message. Set on a `user` message from a sender that identified itself — a token-authenticated TCP peer (`register_token`'s named ones, or the legacy shared `"default"` — §3.1), whose name is a real credential, or a client that declared `authorName` on its `user_prompt` (§3.1), which is what the local unix socket relies on since it has no credential to attribute to. Omitted for assistant/tool messages (no human author) and for a `user` message whose sender identified itself as nothing. *UI Action*: render as e.g. "aveline: ..." in a shared session; fall back to a generic label (or nothing) when absent, don't treat its absence as an error.
- `authorAvatar`: that sender's picture, stored alongside the name — either a short emoji glyph or a data URL, whichever the sending client uses (§2a's avatar conventions). Omitted for assistant/tool messages and for a sender that never set one. *UI Action*: render it next to the message; fall back to a placeholder avatar client-side when absent, don't treat its absence as an error. Store it with the message rather than looking it up from the live peer roster: `peer_list` is per-connection and gone after a reload, whereas this is what a repainted transcript has to draw from.

#### `project_opened`
Confirms `open_project`.
```json
{"type": "project_opened", "data": {"path": "/home/user/projects/my-repo", "sandboxTier": "bubblewrap"}}
```
- `path` is the resolved absolute path, which may differ in form from whatever relative/unclean path was sent.
- `sandboxTier`: one of `"bubblewrap"`, `"linux_namespaces"`, or `"local_restricted_fallback"` (§1) — the isolation level `bash` tool calls in this project actually get. *UI Action*: surface a warning (e.g. in a status bar) when this is `"local_restricted_fallback"`, mirroring the backend's own startup log line, rather than only relying on that log line being visible to whoever's running the server.

#### `directory_list`
Response to `list_directory`.
```json
{
  "type": "directory_list",
  "data": {
    "path": "/home/user/projects",
    "entries": [
      {"name": "my-repo", "isDir": true},
      {"name": "other-repo", "isDir": true},
      {"name": "notes.txt", "isDir": false}
    ]
  }
}
```
- `path` is the resolved absolute directory that was listed. `entries` is sorted directories-first, then alphabetically within each group — unlike `list_files` (§3.1), nothing is filtered out (dotfiles and VCS directories included), since this is for navigating *toward* a project root, not listing one.
*UI Action*: Render as a folder browser; let the user drill into a directory by re-issuing `list_directory` with its path, and confirm a selection via `open_project`.

#### `project_list`
Response to `list_projects`.
```json
{"type": "project_list", "data": {"paths": ["/home/user/projects/my-repo", "/home/user/projects/other-repo"]}}
```
*UI Action*: Populate a project switcher — but see §4b's caveat: this reflects only what's been opened this run, not "everywhere the user has ever worked."

#### `file_list`
Response to `list_files`.
```json
{"type": "file_list", "data": {"files": ["README.md", "src/main.go", "src/lib/util.go"]}}
```
*UI Action*: Feed this into `@file` mention autocomplete — filter client-side as the user types, this event isn't re-sent per keystroke. Re-issue `list_files` yourself if you need a refresh (e.g. after the user creates a new file); the backend doesn't push updates on filesystem changes.

#### `cli_availability`
Response to `detect_cli`.
```json
{"type": "cli_availability", "data": {"claude": true, "codex": false}}
```
*UI Action*: Show/hide or enable/disable the `cli_claude`/`cli_codex` provider options accordingly before the user tries to register one.

#### `pong`
Response to a `ping`, from either side.
```json
{"type": "pong", "data": {"nonce": "anything-you-want-echoed-back"}}
```

#### `peer_list`
Pushed to **every** connected peer whenever the connection roster changes (connect, disconnect, rename, avatar change) — push-on-change, never polled, so this never goes stale sitting in a frontend that forgot to re-request it.
```json
{
  "type": "peer_list",
  "data": {
    "peers": [
      {
        "peerId": "a1b2c3d4",
        "name": "aveline's phone",
        "ip": "192.168.1.42",
        "deviceType": "web",
        "avatar": "data:image/png;base64,...",
        "connectedAt": "2026-09-11T20:52:39.448503642Z"
      }
    ]
  }
}
```
*UI Action*: Render the full peer/connection list from `data.peers` on every push — it's always the complete current roster, not a diff. `ip` is empty for a unix-socket peer (no meaningful remote address). `avatar` is omitted entirely for a peer that never set one (render a fallback/initials avatar client-side, don't treat absence as an error). `peerId` is per-**connection**, not per-user: two tabs/devices from the same person are two separate rows, each independently renameable/disconnectable.

#### `server_identity`
Pushed to a peer right after it connects, and to every peer whenever `set_server_identity` changes it.
```json
{"type": "server_identity", "data": {"identity": "aveline's living room"}}
```
*UI Action*: Show this as the server's display name (e.g. in a connection list or title bar) rather than a raw address.

#### `auto_generate_title`
Pushed to a peer right after it connects, and to every peer whenever `set_auto_generate_title` changes it.
```json
{"type": "auto_generate_title", "data": {"enabled": true}}
```
*UI Action*: Reflect this as a toggle in Settings. Flipping it off only changes the server's own default for a `user_prompt` that doesn't specify `autoGenerateTitle` itself — a client already sending that field per-request keeps its own behavior regardless.

#### `remote_access_set`
Response to a successful `set_remote_access`.
```json
{"type": "remote_access_set", "data": {"enabled": true}}
```

#### `remote_password_set`
Response to a successful `set_remote_password`. Carries no data beyond confirming success — the new password was already known to the caller that set it.
```json
{"type": "remote_password_set", "data": {}}
```

#### `token_registered`
Response to a successful `register_token`, carrying the newly generated token — shown only this once (see `register_token`, §3.1).
```json
{"type": "token_registered", "data": {"name": "aveline", "token": "a1b2c3...", "email": "aveline@example.com"}}
```
*UI Action*: Display/copy this token immediately and prompt the user to save it somewhere (a password manager, a paste to the new device) — there's no way to retrieve it again.

#### `token_list`
Response to `list_tokens`. Never carries a raw token value — same scrubbing as `provider_list`'s `apiKey`.
```json
{
  "type": "token_list",
  "data": {
    "tokens": [
      {"name": "aveline", "email": "aveline@example.com", "hasAvatar": true},
      {"name": "hireri", "hasAvatar": false},
      {"name": "default", "hasAvatar": false}
    ]
  }
}
```
*UI Action*: Populate a "people with access" settings list, each row offering a revoke (`remove_token`) action. The implicit `"default"` entry is the legacy `-token`/`set_remote_password` credential — present even if no one has ever called `register_token`.

#### `token_removed`
Response to a successful `remove_token`, echoing `name` back.
```json
{"type": "token_removed", "data": {"name": "aveline"}}
```

---

## 4. State Machine & Turn Lifecycle

```text
 ┌───────────┐
 │   IDLE    │◄────────────────────────────────────────────────┐
 └─────┬─────┘                                                 │
       │ user_prompt                                           │
       ▼                                                       │
 ┌───────────┐                                                 │
 │ STREAMING ├────────► model_delta                            │
 └─────┬─────┘                                                 │
       │                                                       │
       ├─────────────────┐                                     │
       │ no tool calls   │ tool_call_requested                 │
       │                 ▼                                     │
       │          ┌─────────────┐                              │
       │          │ PERM EVAL   │                              │
       │          └──────┬──────┘                              │
       │                 │                                     │
       │         ┌───────┴────────┐                            │
       │         │ allowed        │ requires ask               │
       │         │                ▼                            │
       │         │         ┌─────────────┐                     │
       │         │         │ WAIT PERM   │                     │
       │         │         └──────┬──────┘                     │
       │         │                │ permission_response        │
       │         ▼                ▼                            │
       │   ┌─────────────────────────────┐                     │
       │   │       TOOL EXECUTION        ├─► tool_result       │
       │   └─────────────┬───────────────┘                     │
       │                 │ (loop back for next model response) │
       │                 └─────────────────────────────────────┤
       ▼                                                       │
 ┌───────────┐                                                 │
 │ COMPLETED │ turn_completed                                  │
 └─────┬─────┘                                                 │
       └───────────────────────────────────────────────────────┘
```

### Cancellation Semantics
At **any** point while not in `IDLE` (`STREAMING`, `WAIT PERM`, or `TOOL EXECUTION`), the frontend may send:
```json
{"type": "cancel_turn", "turnId": "<currentTurnId>"}
```
The backend guarantees:
1. Active model streaming context is canceled.
2. Any blocking `WAIT PERM` is unblocked.
3. Any running subprocess in the sandbox is terminated.
4. The turn terminates with a single `turn_completed` frame containing `finishReason: "cancelled"`.
5. The state returns to `IDLE`.

**A cancelled turn is still persisted.** The user's message is written to `.yoake/sessions/<sessionId>.json` as soon as the turn starts — before the model is called at all — and the session is saved again as the turn ends, whatever ended it: normal completion, a cancel, a provider error, or the server being shut down. Consequences a client can rely on: a cancelled or failed turn still appears in `list_sessions` and in `session_history` (with the partial assistant text that had already streamed, if any), and a session that has been prompted once exists on disk even if that first turn never completed. The one exception is `delete_session` (§3.1), which removes the file and is not undone by a turn that was still in flight when it arrived.

### 4a. Concurrency: this state machine is per-session, not per-process
The backend holds one independent instance of this whole state machine **per `sessionId`** (a session registry keyed by ID, created lazily on first use) — this applies regardless of transport, since they all share the same dispatch logic (§2a): even a single `-stdio` connection can run many sessions concurrently, not just multiple connections against a daemon. Two sessions run fully concurrently: a `cancel_turn` for session A never affects session B's in-flight turn, and a slow/hung turn on one session doesn't block any other session's `user_prompt` from being processed immediately. Design the frontend around "N independent conversations, each with its own state machine," not "one conversation, one backend."

### 4b. Multi-project support: one server, many projects, opened on demand
A single `yoakeland` process is **not** limited to one project. Every project (identified by its filesystem path) gets its own fully independent stack — its own sandbox boundary, permission engine (`.yoake/permissions.json`), snapshot store, session store, and its own session registry from §4a — created lazily the first time anything references that path, and cached for the life of the process. Two projects share nothing except the process itself and the server-level provider registry (§6b/§6c): a tool call in project A's sandbox cannot touch project B's files, project A's sessions are invisible to `list_sessions` on project B, and a runaway turn in one project's session doesn't affect the other project at all.

**How a session gets bound to a project:**
- The first `user_prompt` for a given `sessionId` may carry `data.projectPath` (§3.1) — this opens that project if it isn't already, and binds the session to it, permanently, for that session's lifetime.
- `set_provider`, `set_model_options`, and `set_plan_mode` (§3.1) accept the same optional `projectPath` and bind the session by the identical rules. A client needs this because it configures a brand-new chat *before* that chat's first prompt: those settings have to land on the registry the prompt will resolve to, which is the session's own project's, not the server's default. The session registry (and with it the selected provider, plan mode, and per-session state) is per project — see §4a — so resolving a mutation against the wrong one applies it to an agent that session's turns never touch.
- Once bound, later envelopes for that `sessionId` don't need to repeat `projectPath` — omit it and the binding is reused.
- Sending a *different* `projectPath` for an already-bound session is a conflict, not a switch: the backend rejects it with an `error` rather than silently moving the session. A session's project never changes; if the user wants to work on a different folder, that's a new session.
- If a session never specifies `projectPath` and the server has a default project (`-project`), it falls back to that — this is what keeps single-project usage exactly as simple as it's always been. If there's no explicit path *and* no default, the backend returns an `error` (there's nothing to resolve against). Unlike an explicit `projectPath`, this fallback does **not** bind the session: it's resolved fresh on every envelope that omits `projectPath`, so a session that only ever hit the default project this way is still free to bind to a different one the first time it *does* send an explicit `projectPath`. Only an explicit path (`user_prompt`'s, or one of the three above) is ever recorded as a binding.
- `open_project` (§3.1) does the same "open if needed" step without starting a turn, e.g. to validate a folder before creating a session in it, or to call `list_sessions` against it to show existing threads first.

**What this means for a frontend:** the flow described in the intro — "download the UI, pick a folder to work on, on the fly, from a list, and have multiple folders' sessions running at once against different models" — maps directly onto this. A client managing several open folders is not managing several server processes; it's opening several projects on **one** connection to **one** server, and routing UI state by `sessionId` (whose project is whatever it happens to be bound to) exactly as it already does for concurrent sessions in general.

**What this does *not* do:** `list_projects` only reports what's been opened since the process started — it is not a filesystem scan, and it is not persisted, so a freshly restarted server has no memory of which projects a client cares about; a client that wants "reopen where I left off" needs to remember its own list of project paths and either call `open_project` or send a `user_prompt`/`list_sessions` with `projectPath` for each on reconnect.

---

### 4c. Plan mode: restricting the model to read-only tools

Plan mode lets a session investigate and propose an approach without being able to act on it. `set_plan_mode` (§3.1) toggles it per session, effective on that session's next turn.

**The mechanism is tool-set restriction, not call-time interception.** While plan mode is on, the tool definitions sent to the model (the same list that shapes what it's even capable of asking for) are reduced to a read-only allowlist — currently just `file_read`. `file_write`, `file_edit`, and `bash` are not merely denied when called; their definitions are never handed to the model in the first place, so there's nothing for it to call. This mirrors how plan mode works in comparable coding-agent harnesses (e.g. Claude Code's own plan mode): the model can't attempt a mutation it doesn't know exists, rather than attempting one and getting rejected. Concretely, this avoids a class of problems the intercept-and-deny approach has — the model wasting a turn on a call it can't complete, getting confused by a denial, or a permission-cache entry (`remember: true` from an earlier `permission_response`, §3.1) accidentally green-lighting a mutation during planning.

A backend safeguard still rejects a mutating tool call outright if one somehow arrives during plan mode (defense in depth against a misbehaving or stale provider integration) — but this is a backstop, not the primary mechanism, and a frontend should never rely on it being reached in normal operation.

**What plan mode does not do:** it doesn't stop the model from *talking* — `model_delta` streams normally, so a plan can be narrated in prose. It also doesn't change permission behavior for tools it does expose: `file_read` has never required permission (§3.1's `permission_request` is tool-specific), so plan mode adds no new prompts, only removes capabilities. Exiting plan mode (`set_plan_mode` with `enabled: false`) restores the full tool set on the next turn — nothing about history, snapshots, or session state changes.

---

## 5. Structured Diff Rendering (Myers Diff Engine)

When `permission_request` is received for a `file_write` or `file_edit` tool, the `data.diff` field provides pre-computed diff information.

### Diff Data Types
```typescript
interface FileDiff {
  filePath: string;     // Relative path to file
  unified: string;      // Standard unified diff format
  hunks: Hunk[];        // Structured hunks for GUI rendering
}

interface Hunk {
  oldStart: number;
  oldLines: number;
  newStart: number;
  newLines: number;
  lines: DiffLine[];
}

interface DiffLine {
  type: " " | "+" | "-"; // " " = Context, "+" = Added, "-" = Deleted
  content: string;       // Text content (without prefix symbol)
}
```

### Rendering Guidelines for the Frontend
- If `type == "+"`, render green background tint.
- If `type == "-"`, render red background tint.
- If `type == " "`, render neutral editor background.
- Display `filePath` in the modal title bar.
- Provide three user actions:
  1. **Allow Once**: `{"allowed": true, "remember": false}`
  2. **Always Allow**: `{"allowed": true, "remember": true}`
  3. **Deny**: `{"allowed": false, "remember": false}`

---

## 6. Project Artifacts & Filesystem Layout

The backend maintains persistent runtime state in a hidden `.yoake/` directory inside **each open project's** root (see §4b — a server may have several of these live at once, one per opened project, each independent):

```text
<project_root>/
└── .yoake/
    ├── permissions.json        # Persisted permission rules
    ├── usage.json              # Running spend/token totals, per-session and project-wide
    ├── sessions/
    │   └── <sessionId>.json    # Full conversation history, title & archived flag
    └── snapshots/
        └── snap_<timestamp>/   # One turn's pre-mutation backups, grouped
            ├── manifest.json   # Every file this turn touched & rollback instructions
            └── data/           # Original pre-mutation byte copies
```

Provider configs are **not** here — see §6c: they're server-level state, not project-level, and live in `-state-dir` instead.

### Snapshots are per-turn, not per-mutation
Every `file_write`/`file_edit` call within one turn shares that turn's single snapshot: the first mutation in a turn creates it, and every later mutation in the *same* turn — including of a different file — extends the same snapshot rather than creating an independent one. A file already recorded earlier in the turn is never re-captured on a later mutation, since it's the file's state from *before the turn started* that must be preserved, not an intermediate state partway through it. A turn's snapshot stops accepting new files once the turn ends (see `snapshot_capturing`/`snapshot_created`, §3.2, for wire-level visibility into this as it happens).

### Restoring Snapshots
The frontend can prompt the user to revert a turn by sending:
```json
{
  "type": "restore_snapshot",
  "data": { "snapshotId": "snap_1710950400000000" }
}
```
Because a snapshot covers the *entire* turn rather than a single mutation, one `restore_snapshot` call reverts everything that turn touched: every modified file goes back to its exact pre-turn state, and every file the turn created is removed — not just whichever single file happened to trigger that snapshot's creation.

---

## 6b. Provider Registry, Hot-Swapping & Subscription Auth

A **provider** is a named, persisted *credential*, not a model choice — a session selects and switches between providers, and independently picks a model from that provider, at runtime via `register_provider`/`list_providers`/`list_models`/`set_provider` (§3.1/§3.2). This split matters because one `api_key` credential normally unlocks *many* models (an Anthropic key isn't scoped to one Claude model, an OpenRouter key reaches hundreds of models across vendors) — baking a single `model` into the provider config, as an earlier version of this API did, would force registering a separate named provider (and re-entering the same key) per model the user wants to use. `model` now lives only on `set_provider` (per session) and is discovered via `list_models` (per provider), never on `register_provider`/`provider_list`.

Each provider also persists its own **favorite models** (`FavoriteModels` on `ProviderConfig`, set via `set_favorite_models`/§3.1) — server-side, in the same `providers.json` entry as the credential, so a starred model follows the user to any client rather than living in that one client's local storage. `list_models` echoes each model's `isFavorite` status back so a picker can sort/highlight without a second round trip.

Three provider kinds exist:

| `kind` | Needs | Billing | Notes |
|---|---|---|---|
| `api_key` | `httpProvider` (`anthropic`\|`openai`\|`openrouter`) + `apiKey`, optional `baseUrl` | Metered API usage | Calls the provider's HTTP API directly. `list_models` queries that HTTP backend's own models endpoint live, using this same credential. |
| `cli_claude` | — | The user's own Claude subscription | Shells out to the user's own, separately-installed-and-authenticated `claude` CLI (`claude auth login` done outside yoakeland entirely). Yoakeland never sees a credential. `model` on `set_provider` is optional; `list_models` returns the current model slugs from T3 Code's manifest (see below) since there's no key to query. |
| `cli_codex` | — | The user's own ChatGPT subscription | Same idea via the `codex` CLI. |

**Registering `cli_claude`/`cli_codex` is a singleton per kind**: there's only ever one `claude` CLI and one `codex` CLI on the machine the backend runs on, so `register_provider` for either kind always replaces the previously-registered provider of that same kind — even if you give it a different `name` — rather than accumulating stale duplicate entries. The `name` you most recently registered it under is still what you address it by in `set_provider`/`remove_provider`; only the *old* name stops resolving once a new registration of the same kind supersedes it.

**Streaming caveat for `cli_claude`/`cli_codex`**: `model_delta` (§3.2) is normally incremental, token-by-token. For these two provider kinds it is not — each turn is one subprocess run to completion (`claude -p --output-format json` / `codex exec --json`), parsed only after the process exits, so the frontend receives the entire response as a single `model_delta` event rather than a stream of small ones. Expect silence followed by the whole message at once for these providers; don't assume every provider streams at the same granularity.

**Why CLI-subprocess instead of OAuth token reuse**: Anthropic and OpenAI both prohibit third-party tools from using a Claude/ChatGPT *subscription* OAuth token directly against their APIs — that's billing-plan misuse, and Anthropic actively enforces it. Shelling out to the *official, already-authenticated CLI binary* is the sanctioned pattern instead (this is what tools like T3 Code do): the CLI owns its own auth end-to-end, yoakeland just drives it as a subprocess per turn. Both CLIs are themselves full autonomous coding agents with their own tool/sandbox systems, so yoakeland invokes them in a restricted, mutation-disabled mode — `claude -p --disallowedTools Bash,Read,Write,Edit,Glob,Grep,Task,NotebookEdit,TodoWrite,BashOutput,KillShell,SlashCommand`, `codex exec --sandbox read-only -c tools.web_search=true` — they act as pure text-completion backends plus (as of this manifest generation) their own server-side web search, and yoakeland's own permission-gated tool engine (§5, `.yoake/permissions.json`) remains the *only* thing that can actually read/write files or run shell commands. `WebFetch`/`WebSearch` are deliberately left off the claude CLI's disallow-list — read-only, run server-side on Anthropic's own account, no reason to reimplement them client-side. A frontend offering `cli_claude`/`cli_codex` should check the CLI is actually installed first — send `detect_cli` (§3.1) and gate on the `cli_availability` response (§3.2); this checks whichever machine the *backend* runs on, not the frontend's machine, if they differ.

**Web search**: available with no extra setup on `anthropic` (Anthropic's native `web_search_20250305` server tool, added to every request unless `ANTHROPIC_DISABLE_WEB_SEARCH` is set — e.g. for an org that has it disabled in the Console, where including it makes the API reject the whole request with a 400) and `openrouter` (`openrouter:web_search`, opt out with `OPENROUTER_DISABLE_WEB_SEARCH`) — both resolved entirely server-side against the same credential already in use, invisible to yoakeland's own tool-call machinery. Plain `openai` gets none (Chat Completions has no general hosted search tool). Separately, a `web_search` *client* tool (in the same family as `file_read`/`bash`, §3.2's `tool_call_requested`) is always registered, for providers/backends with no native option of their own: with `BRAVE_API_KEY` set it queries Brave's JSON API (the reliable path); with no key it falls back to scraping DuckDuckGo's HTML results for free, which works but is best-effort — DuckDuckGo bot-checks non-browser/datacenter traffic, so this path can intermittently return an error instead of results, and a startup log line names which mode a project is running in.

**Web fetch**: a `web_fetch` *client* tool is also always registered (no key needed, no native-provider equivalent to defer to) — it downloads a URL and extracts just the main readable text (title + body, via a Readability.js port) so a search result can be read without shipping raw page HTML through the model's context. Its dialer refuses to connect to loopback/private/link-local/multicast addresses, including on redirect, so a URL surfaced from search results or fetched page content can't be used to reach internal network services.

**OpenRouter is the one case with real "sign in with your account" OAuth** (`openrouter_oauth_start`/`openrouter_oauth_complete`, §3.1/§3.2): OpenRouter's own PKCE flow is built for third-party apps, unlike Anthropic/OpenAI's consumer subscriptions. The resulting API key is registered as an ordinary `api_key`/`openrouter` provider with `authMethod: "oauth"` for display purposes only — functionally identical to one registered manually.

**Manual API key registration** is the fallback/default path for all three HTTP providers and needs no special UI beyond a form: name, provider, key — model selection happens separately, after registration, via `list_models`/`set_provider`.

**`cli_claude`/`cli_codex` model list source**: rather than a hand-maintained table, `list_models` for these two kinds is served from a cached copy of [T3 Code's](https://github.com/pingdotgg/t3code) `model-manifest.json` (`apps/server/src/provider/model-manifest.json` on its `main` branch — MIT-licensed, public, no auth needed). Mechanics (`internal/provider/manifest.go`):
- **Never blocks a request on the network.** `list_models` always reads whatever was last parsed successfully — an in-memory cache refreshed only in the background.
- **Refreshed on a fixed daily wall-clock schedule** (00:00 UTC), not "24h since last boot" — a restart doesn't drift the schedule or trigger a redundant fetch right after coming back up. One immediate best-effort fetch also happens at startup, still without blocking it.
- **Cached to `<-state-dir>/model-manifest-cache.json`** (raw bytes + `ETag`) so a restart resumes serving the last-fetched copy immediately, and so the next fetch can be conditional (`If-None-Match`) — a day where upstream hasn't changed costs a `304`, not the full ~20KB body.
- **A last-known-good snapshot is embedded in the binary** (`internal/provider/manifest_fallback.json`) as the ultimate fallback, so a fresh install that has never successfully fetched still returns real model slugs instead of nothing.
- **Parsed defensively.** This manifest is T3's internal implementation detail, not a contract with yoakeland — a fetch that 200s but no longer matches the expected shape logs a warning and keeps serving the last-good cache rather than wiping it or crashing. A fetch failure that persists across more than one scheduled attempt (i.e. more than one day) logs a `WARNING`, since T3 moving or deleting the file for good should be noticed, not silently go stale forever.
- **`capabilities` cross-walk**: each `claudeAgent` model's `profile` in the manifest carries an `optionDescriptors` list yoakeland maps into the model's `capabilities` (§3.2) — `effort`'s options become `reasoningEfforts` (a real `select` a client can map to `set_model_options.reasoningEffort`), `fastMode`'s presence becomes `capabilities.fastMode: true`, and `contextWindow`'s options become `contextWindows`. Option values the descriptor marks under `promptInjectedValues` (T3's `ultracode`/`ultrathink`, prompt-text tricks rather than real API parameters) are filtered out rather than modeled. `cli_codex` models get their slug as both `id` and `displayName` and no `capabilities` — T3's manifest doesn't carry a `providers.codex` block with per-model profiles today.
- **Legacy models are included, not dropped.** `currentModels.claudeAgent` is only T3's *current* pick per profile; `providers.claudeAgent.models` entries with `status: "legacy"` (older Opus/Sonnet/Haiku/Fable point releases) are appended after the current ones with `isLegacy: true` (§3.2) rather than silently disappearing from `list_models` once T3 stops calling them current. `cli_codex` has no per-model status data today, so its models are never marked legacy.

**`turn_completed.usage.costUsd` pricing source**: the same fetch/cache/fallback shape as the model manifest above, applied to per-model USD rates instead of model slugs (`internal/provider/pricing_cache.go`) — background-refreshed daily, cached to `<-state-dir>/pricing-cache.json` with conditional `If-None-Match` refetches, an embedded last-known-good snapshot as the ultimate fallback, parsed defensively, never blocking a turn on the network. The upstream source here is [LiteLLM's](https://github.com/BerriAI/litellm) `model_prices_and_context_window.json` (MIT-licensed, public, no auth needed) rather than T3's manifest — the same rate table `ccusage` and comparable cost trackers price against, covering far more models/providers than a hand-maintained map ever would. Lookup tries, in order: the model string as given, `"openrouter/"` + that string (OpenRouter providers pass yoakeland's own model string straight through, e.g. `"anthropic/claude-3.5-sonnet"`, which is exactly the suffix LiteLLM keys its `openrouter/*` entries by), and the bare name after the last `/`. A model this misses entirely falls through to `internal/agent/pricing.go`'s small built-in table, and then to `0` if that has nothing either.

---

## 6c. Provider Registry Is Server-Level, Not Project-Level

Providers/credentials belong to whoever is running the server, not to any one project someone connects to work on — you register an API key once and it's available no matter which project a session is bound to (§4b). Concretely: `providers.json` lives at `<-state-dir>/providers.json` (default `~/.yoake/providers.json`), completely separate from any project's own `.yoake/` directory, and `register_provider`/`list_providers`/`set_provider`/`remove_provider`/the OpenRouter OAuth flow all operate against this one server-wide registry regardless of which session or project you're issuing them from. The only project-relative detail is that `set_provider` builds a `cli_claude`/`cli_codex` provider's subprocess to run with the *session's own bound project* as its working directory — so the same named provider config behaves correctly no matter which project a given session activates it in.

`<-state-dir>` holds every other piece of server-level state alongside `providers.json`: `server.json` (identity + remote-access password) and `tls/` (the auto-managed CA + server certificate backing the optional TCP listener when `-tls-cert`/`-tls-key` aren't given — see §2a for what's in there and what "pairing" a client means).

### Encryption at rest

By default `providers.json` is plain JSON — an API key registered via `register_provider` sits there in the clear, protected only by the file's 0600 permissions (owner read/write only). That's real, but it's not "solid": it does nothing against a backup that doesn't preserve permissions, a copied disk, or anyone with root.

Passing `-master-key` (or setting `YOAKELAND_MASTER_KEY`) turns on real encryption at rest instead:
- The whole file is AES-256-GCM encrypted; the key is derived from the passphrase via scrypt (a random salt per encryption, stored alongside the ciphertext — the salt/nonce aren't secret, only the passphrase is).
- Turning it on for an *existing* plaintext `providers.json` needs no manual migration: the store reads the old plaintext fine, and the very next write (any `register_provider`/`remove_provider`) transparently re-encrypts the whole file going forward.
- A wrong passphrase against an already-encrypted file is a hard startup error, not silent data loss — the server refuses to start rather than quietly discarding an unreadable registry.
- Without `-master-key`/`YOAKELAND_MASTER_KEY` set, the server logs a plaintext warning at startup — insecure-by-omission is always visible, never silent, the same principle behind auto-generating a CA rather than silently running the TCP listener in plaintext (§2a).

This only protects `providers.json`. It says nothing about the OS keychain, and a frontend (e.g. a Tauri client) holding its own copy of a connection token or key locally should use its platform's real credential storage rather than assuming the backend's encryption covers it too.

---

## 7. Reference Frontend Implementation (Dart / Flutter) — historical, non-normative

The Dart snippet below predates daemon mode, multi-session concurrency, and everything in §2a/§3 marked "daemon mode" or "TCP listener" — it spawns a `-stdio` subprocess and talks to nothing else. It's kept for the JSON-lines framing pattern (parse/serialize is transport-agnostic) but should **not** be used as a template for a new client; a new client (e.g. a Tauri app) should dial the local unix socket directly (§2a) rather than spawning a subprocess, and needs the session-registry-aware handling (and, if it wants remote access, the TCP+auth path) this file doesn't have. See the client build checklist in `CLIENT_CHECKLIST.md`.

### `lib/ipc/yoake_backend.dart`

```dart
import 'dart:async';
import 'dart:convert';
import 'dart:io';

class Envelope {
  final String type;
  final String? sessionId;
  final String? turnId;
  final String? toolCallId;
  final Map<String, dynamic>? data;

  Envelope({
    required this.type,
    this.sessionId,
    this.turnId,
    this.toolCallId,
    this.data,
  });

  factory Envelope.fromJson(Map<String, dynamic> json) {
    return Envelope(
      type: json['type'] as String,
      sessionId: json['sessionId'] as String?,
      turnId: json['turnId'] as String?,
      toolCallId: json['toolCallId'] as String?,
      data: json['data'] as Map<String, dynamic>?,
    );
  }

  Map<String, dynamic> toJson() => {
    'type': type,
    if (sessionId != null) 'sessionId': sessionId,
    if (turnId != null) 'turnId': turnId,
    if (toolCallId != null) 'toolCallId': toolCallId,
    if (data != null) 'data': data,
  };
}

class YoakeBackend {
  Process? _process;
  final _eventController = StreamController<Envelope>.broadcast();
  Stream<Envelope> get events => _eventController.stream;

  Future<void> start({
    required String backendBinPath,
    required String projectDir,
    String provider = 'auto',
    String? model,
    String? baseUrl,
  }) async {
    final args = ['-project', projectDir, '-provider', provider];
    if (model != null && model.isNotEmpty) args.addAll(['-model', model]);
    if (baseUrl != null && baseUrl.isNotEmpty) args.addAll(['-base-url', baseUrl]);

    _process = await Process.start(backendBinPath, args);

    // Parse newline-delimited JSON from stdout
    _process!.stdout
        .transform(utf8.decoder)
        .transform(const LineSplitter())
        .listen((line) {
      if (line.trim().isEmpty) return;
      try {
        final map = jsonDecode(line) as Map<String, dynamic>;
        _eventController.add(Envelope.fromJson(map));
      } catch (e) {
        stderr.writeln('Failed to decode IPC frame: $e\nLine: $line');
      }
    });

    // Pipe operational errors from backend stderr
    _process!.stderr
        .transform(utf8.decoder)
        .listen((err) => stderr.write('[Backend Log] $err'));
  }

  void send(Envelope env) {
    if (_process == null) throw StateError('Backend is not running');
    final line = jsonEncode(env.toJson()) + '\n';
    _process!.stdin.write(line);
  }

  void sendPrompt(String sessionId, String turnId, String prompt) {
    send(Envelope(
      type: 'user_prompt',
      sessionId: sessionId,
      turnId: turnId,
      data: {'prompt': prompt, 'sessionId': sessionId},
    ));
  }

  void sendPermissionResponse({
    required String sessionId,
    required String turnId,
    required String toolCallId,
    required bool allowed,
    required bool remember,
  }) {
    send(Envelope(
      type: 'permission_response',
      sessionId: sessionId,
      turnId: turnId,
      toolCallId: toolCallId,
      data: {'allowed': allowed, 'remember': remember},
    ));
  }

  void cancelTurn(String sessionId, String turnId) {
    send(Envelope(
      type: 'cancel_turn',
      sessionId: sessionId,
      turnId: turnId,
      data: {'sessionId': sessionId, 'turnId': turnId},
    ));
  }

  Future<void> dispose() async {
    await _process?.stdin.close();
    _process?.kill();
    await _eventController.close();
  }
}
```

---

## 8. Summary Checklist for Frontend Developers

- [ ] **Single Stdin/Connection Writer**: Never open multiple writers to the same transport (stdin, or one TCP connection).
- [ ] **Frame Buffer**: Ensure line splitting buffers up to 10 MB chunks.
- [ ] **Auth First**: On the optional TCP listener, send `auth` as the very first frame on every new connection and don't send anything else until it succeeds (§2a). Not needed for the local unix socket.
- [ ] **Tool Call Correlation**: Always echo back `toolCallId` in `permission_response`.
- [ ] **Active Turn State**: Maintain a boolean `isTurnActive` **per session**, not globally, since sessions run concurrently (§4a).
- [ ] **Diff Preview**: Check `event.data['diff']` when receiving `permission_request` to display visual diff hunks.
- [ ] **Session Persistence**: Pass `sessionId` in each prompt to preserve conversational context across application restarts. Send `authorName`/`authorAvatar` alongside it (§3.1) and render `session_history`'s `author`/`authorAvatar` (§3.2), so a transcript repainted after a reload still shows who sent each message rather than losing the sender identity a live turn displayed.
- [ ] **Session Takeover**: Handle `session_taken_over` (§3.2) — don't leave a UI waiting forever for events that will never arrive on a displaced connection.
- [ ] **Context & Spend**: Render `turn_completed.data.context`/`.spend` (§3.2) rather than re-deriving them client-side.
- [ ] **Budget Rejections**: Handle `finishReason: "budget_exceeded"` as a distinct, user-visible outcome, not a generic error — the model was never even called.
- [ ] **No-Provider Rejections**: Handle `finishReason: "no_provider"` by prompting the user to register/select a provider, rather than treating it as a generic failure — a server started with no default provider is an expected, supported setup, not a misconfiguration.
- [ ] **Provider Picker**: Populate provider choices from `provider_list`, never hardcode a fixed set — new ones can be registered at any time; gate `cli_claude`/`cli_codex` on `cli_availability` (§3.1/§3.2) first.
- [ ] **Model Picker**: Populate model choices from `list_models` (per selected provider), never hardcode a fixed model set — pass the chosen `id` as `model` on `set_provider`. One registered provider now covers many models; don't ask the user to register a separate provider per model.
- [ ] **Favorite Models**: Let the user star models in the picker (`isFavorite` on each `model_list` entry) and persist the choice via `set_favorite_models`, not local storage — favorites are server-side per provider (§6b) so they follow the user across devices/clients.
- [ ] **Reasoning/Thinking Controls**: Only show a reasoning-effort or thinking-budget control for a model that advertises it via `capabilities` on its `model_list` entry (§3.2) — sending `set_model_options` to a model that doesn't support the field is a silent no-op, not an error, so the UI is what prevents a confusing dead control.
- [ ] **Session Discovery**: Use `list_sessions` (with a `projectPath`) to populate a session switcher on connect, rather than requiring the user to already know a session ID.
- [ ] **Session History**: Call `get_session_history` for whichever session the user actually opens — with its `projectPath`, since a session→project binding is server memory and gone after a restart (§4b) — and repaint its transcript from the response, before treating the composer as ready — a session's past turns are never pushed to a client automatically, whether reconnecting locally or opening the same session from a different device.
- [ ] **Multi-Project Model**: Track your own list of project paths the user has opened (the server's `list_projects` only reflects this run, not history — §4b) and route UI state by `sessionId`, whose bound project can vary per session on a single connection to a single server.
- [ ] **Project Binding Conflicts**: Handle the `error` from sending a different `projectPath` for an already-bound `sessionId` (§4b) as "this session belongs to a different folder," not a generic failure.
- [ ] **File Mentions**: Use `list_files` (§3.1) to source `@file` mention autocomplete rather than shelling out to `ls`/`find` from the frontend — it's already sandboxed to the right project and pre-filtered of VCS/dependency noise.
- [ ] **Project Picker**: Use `list_directory` (§3.1), not a native OS file dialog, to browse for a project folder — a native dialog reflects the frontend's own filesystem, which is wrong the moment the connection is remote (TCP, §2a).
- [ ] **Session Titles/Archiving**: A session's first turn auto-titles itself — the backend races a same-model title request against a plain fallback and pushes the result as an unprompted `session_updated` (§3.1/§3.2). Set `autoGenerateTitle: false` on that first `user_prompt` to skip straight to the fallback, or call `set_session_title` any time to override — it only ever fills an empty title, so it always wins over generation once sent. `set_auto_generate_title`/`auto_generate_title` (§3.1/§3.2) is the same switch as a server-wide Settings default, for a client that never wants to think about the per-request field. Use `set_session_archived` for a hide-don't-delete affordance, and filter `archived: true` sessions out of the default switcher view.
- [ ] **Session Deletion**: Offer a delete action on both active and archived sessions via `delete_session` (§3.1) — confirm first, since unlike archiving it's not reversible. Handle `session_deleted` (§3.2) by removing the entry from the switcher and navigating away if it was open.
- [ ] **Split/Sidebar Layout**: Persist which two sessions are paired via `set_session_layout` (§3.1), reading it back from `linkedSessionId` on `session_list`/`session_updated` (§3.2) — this is what lets a paired session survive a reconnect or a second device opening the project. Keep purely visual state (sidebar collapsed, split ratio, panel width) in client-local storage instead: those are per-device screen-space preferences, not conversation data, and syncing them would fight different window sizes across devices.
- [ ] **Snapshot Picker**: Use `list_snapshots` (§3.1) to populate a restore-snapshot picker rather than requiring a user to already know a `snapshotId` — each entry now represents a whole turn's changes, not a single mutation, so restoring one reverts everything that turn touched (§6).
- [ ] **Snapshot Visibility** (optional): Handle `snapshot_capturing`/`snapshot_created` (§3.2) for a "snapshotting…" indicator or activity log around otherwise-invisible pre-mutation backups; a client that doesn't care can ignore both and rely on `tool_result` alone as before.
- [ ] **Peer List**: Render `peer_list` (§3.2) as a live roster, not a one-time fetch — it's pushed on every connect/disconnect/rename/avatar-change, and each push is the full roster, not a diff. Gate `disconnect_peer`'s "are you sure" UI on the fact that any peer can disconnect any other (§2a) — there's no per-user identity backing a finer-grained check yet.
- [ ] **Peer Identity**: Send `name`/`deviceType`/`avatar` on `auth` (§2a) when connecting to any server (downscale the avatar to 96x96 client-side first — the backend does no image processing and drops anything oversized), and use `set_peer_name`/`set_peer_avatar` (§3.1) to update them live afterward, e.g. from a Settings screen — don't require a reconnect to pick up a profile change.
- [ ] **Remote Access & Password Settings**: Build the "enable remote use / choose a password" UI around `set_remote_access`/`set_remote_password` (§3.1), and disable or hide both controls when the active connection isn't the local unix socket — the backend rejects them over TCP outright.
- [ ] **Per-Person Tokens**: For a server more than one human connects to, build a "people with access" settings list around `register_token`/`list_tokens`/`remove_token` (§3.1) instead of sharing the one `-token`/`set_remote_password` secret — same local-socket-only restriction as above. Render `session_history`'s `author`/`authorAvatar` fields (§3.2) in a shared transcript, and know that a token-backed peer can't `set_peer_name` itself (its name is fixed at registration).
- [ ] **Server Identity**: Show `server_identity` (§3.2, pushed on connect and on change) as the server's display name; let `set_server_identity` rename it from Settings.
- [ ] **Plan Mode**: Reflect `set_plan_mode`/`plan_mode_set` (§3.1/§3.2/§4c) in the composer UI — the restriction is enforced backend-side (the model literally isn't given write tools), but the frontend still owns signaling the mode to the user.
- [ ] **Liveness**: Reply to unsolicited `ping` with `pong` (echoing `nonce`), and treat a connection that's gone quiet for a while as dead and worth reconnecting — see "Keeping a connection alive" in §2a.
- [ ] **Local Connections**: For same-machine use, dial the always-on local unix socket (§2a) directly rather than spawning `yoakeland -stdio` as a subprocess or dialing `127.0.0.1` TCP — no token to manage, the OS enforces who can connect, and the daemon keeps running (and keeps other clients' sessions alive) independent of any one client's lifetime. Reserve `-stdio` for when a frontend genuinely wants to own the backend's whole process lifecycle itself; fall back to TCP+token only for remote/multi-device access.
- [ ] **TLS Awareness**: The TCP listener is always TLS (an operator's own cert, or yoakeland's auto-managed CA — see §2a) — but a client dialing it must still trust the *right* CA rather than skip verification: for the auto-managed case, fetch/copy `<-state-dir>/tls/ca.pem` once during pairing and trust only that certificate (a non-default root-cert-pool), not the public trust store, and never fall back to disabling certificate verification.

For an actual client build plan, see `CLIENT_CHECKLIST.md`.